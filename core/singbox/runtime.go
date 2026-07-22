package singbox

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	singbox "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/bufio"
	singJSON "github.com/sagernet/sing/common/json"
	N "github.com/sagernet/sing/common/network"
	log "github.com/sirupsen/logrus"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/common/rate"
	"github.com/limo13660/daonode/core/contract"
	"github.com/limo13660/daonode/core/shared"
)

const runtimeStopTimeout = 10 * time.Second

type userCounter struct {
	upload   atomic.Int64
	download atomic.Int64
}

type runtime struct {
	*shared.RuntimeServices

	tag  string
	info *panel.NodeInfo

	mu       sync.Mutex
	instance *singbox.Box
	cancel   context.CancelFunc
	users    map[int]panel.UserInfo

	policyMu  sync.RWMutex
	byName    map[string]panel.UserInfo
	counters  map[int]*userCounter
	accepting atomic.Bool
}

// NewRuntime creates a DaoNode adapter around the unmodified official
// sing-box implementation. Protocol parsing, TLS and NaiveProxy framing stay
// inside sing-box; DaoNode supplies only common policy and accounting hooks.
func NewRuntime(tag string, info *panel.NodeInfo) contract.Runtime {
	r := &runtime{
		tag:      tag,
		info:     info,
		users:    make(map[int]panel.UserInfo),
		byName:   make(map[string]panel.UserInfo),
		counters: make(map[int]*userCounter),
	}
	r.RuntimeServices = shared.NewRuntimeServices(tag, r.loadTraffic)
	return r
}

func (r *runtime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restartLocked()
}

func (r *runtime) Stop() error {
	r.accepting.Store(false)
	done := make(chan error, 1)
	go func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		done <- r.stopLocked()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(runtimeStopTimeout):
		return fmt.Errorf("%w while stopping sing-box runtime", contract.ErrRuntimeStopTimeout)
	}
}

// Validate builds the official sing-box instance without starting it. This
// catches invalid Naive, route, ECH, QUIC and TLS configuration before a
// reload stops the currently working instance.
func (r *runtime) Validate(users []panel.UserInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	previousUsers := r.users
	r.users = make(map[int]panel.UserInfo, len(users))
	for _, user := range users {
		r.users[user.Id] = user
	}
	defer func() { r.users = previousUsers }()

	instance, cancel, err := r.buildInstanceLocked()
	if err != nil {
		return err
	}
	cancel()
	if err := instance.Close(); err != nil {
		return fmt.Errorf("close validated sing-box runtime: %w", err)
	}
	return nil
}

func (r *runtime) AddUsers(users []panel.UserInfo) (int, error) {
	if err := r.SyncUsers(nil, users); err != nil {
		return 0, err
	}
	return len(users), nil
}

func (r *runtime) DelUsers(users []panel.UserInfo) error {
	return r.SyncUsers(users, nil)
}

func (r *runtime) SyncUsers(deleted, added []panel.UserInfo) error {
	if len(deleted) == 0 && len(added) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	previous := maps.Clone(r.users)
	for _, user := range deleted {
		delete(r.users, user.Id)
	}
	for _, user := range added {
		r.users[user.Id] = user
	}
	r.rebuildPolicyLocked()
	if r.instance != nil && len(r.users) > 0 && len(added) == 0 {
		// The official Naive authenticator is immutable, but retaining a deleted
		// credential inside it is safe because the tracker consults byName before
		// accepting or accounting a connection. Revoke the deleted users at the
		// policy layer and close only their active streams, leaving every other
		// user's HTTP/2 or HTTP/3 tunnel untouched.
		r.RuntimeServices.SyncUsers(deleted, nil)
		log.WithFields(log.Fields{
			"tag":     r.tag,
			"deleted": len(deleted),
			"users":   len(r.users),
		}).Info("Naive users removed without restarting the runtime")
		return nil
	}
	r.accepting.Store(false)
	r.RuntimeServices.SyncUsers(deleted, added)

	if err := r.restartLocked(); err != nil {
		r.RuntimeServices.SyncUsers(added, deleted)
		r.users = previous
		r.rebuildPolicyLocked()
		if errors.Is(err, contract.ErrRuntimeStopTimeout) {
			return err
		}
		if recoverErr := r.restartLocked(); recoverErr != nil {
			return fmt.Errorf("synchronize sing-box users: %v; restore previous users: %w", err, recoverErr)
		}
		r.accepting.Store(len(r.users) > 0)
		return err
	}
	r.accepting.Store(len(r.users) > 0)
	return nil
}

func (r *runtime) Traffic(minTraffic int) ([]panel.UserTraffic, error) {
	return r.RuntimeServices.Traffic(minTraffic)
}

func (r *runtime) CommitTraffic(traffic []panel.UserTraffic) {
	r.RuntimeServices.CommitTraffic(traffic)
}

func (r *runtime) restartLocked() error {
	if r.info == nil || r.info.Common == nil {
		return fmt.Errorf("sing-box node configuration is missing")
	}
	if strings.ToLower(r.info.Type) != "naive" {
		return fmt.Errorf("sing-box runtime does not support protocol %s", r.info.Type)
	}
	if len(r.users) == 0 {
		return r.stopLocked()
	}

	instance, cancel, err := r.buildInstanceLocked()
	if err != nil {
		return err
	}
	if err := r.stopLocked(); err != nil {
		cancel()
		_ = instance.Close()
		return err
	}
	if err := instance.Start(); err != nil {
		cancel()
		_ = instance.Close()
		return fmt.Errorf("start official sing-box NaiveProxy inbound: %w", err)
	}
	r.instance = instance
	r.cancel = cancel
	log.WithFields(log.Fields{
		"tag":       r.tag,
		"protocol":  "naive",
		"port":      r.info.Common.ServerPort,
		"listen_ip": r.info.Common.ListenIP,
		"transport": strings.ToUpper(r.info.Common.TransportProtocol),
		"users":     len(r.users),
	}).Info("Official sing-box runtime started")
	return nil
}

func (r *runtime) buildInstanceLocked() (*singbox.Box, context.CancelFunc, error) {
	common := r.info.Common
	cert := common.CertInfo
	certificateRequired := cert == nil || cert.CertMode != "none"
	if certificateRequired && (cert == nil || cert.CertFile == "" || cert.KeyFile == "") {
		return nil, nil, fmt.Errorf("NaiveProxy TLS certificate files are missing")
	}
	if certificateRequired {
		if _, err := tls.LoadX509KeyPair(cert.CertFile, cert.KeyFile); err != nil {
			return nil, nil, fmt.Errorf("load NaiveProxy TLS certificate: %w", err)
		}
	}
	if !certificateRequired && strings.ToUpper(common.TransportProtocol) != "TCP" {
		return nil, nil, fmt.Errorf("NaiveProxy without a certificate only supports TCP relay nodes")
	}

	users := make([]map[string]any, 0, len(r.users))
	for _, user := range r.users {
		users = append(users, map[string]any{
			"username": r.userName(user.Id),
			"password": user.Uuid,
		})
	}
	inbound := map[string]any{
		"type":        "naive",
		"tag":         r.tag,
		"listen":      common.ListenIP,
		"listen_port": common.ServerPort,
		"network":     strings.ToLower(common.TransportProtocol),
		"users":       users,
	}
	if certificateRequired {
		tlsOptions := map[string]any{
			"enabled":          true,
			"server_name":      common.TlsSettings.PrimaryServerName(),
			"certificate_path": cert.CertFile,
			"key_path":         cert.KeyFile,
		}
		if strings.EqualFold(strings.TrimSpace(common.TlsSettings.ECH), "custom") {
			echKey, err := normalizeECHPEM(common.TlsSettings.ECHKey, "ECH KEYS")
			if err != nil {
				return nil, nil, fmt.Errorf("configure NaiveProxy ECH: %w", err)
			}
			tlsOptions["ech"] = map[string]any{
				"enabled": true,
				"key":     []string{echKey},
			}
		}
		inbound["tls"] = tlsOptions
	}
	if value := strings.TrimSpace(common.ProtocolSettings.QUICCongestionControl); value != "" {
		inbound["quic_congestion_control"] = value
	}
	routeConfig, dnsConfig, err := buildRouteConfig(common.Routes)
	if err != nil {
		return nil, nil, fmt.Errorf("configure NaiveProxy routes: %w", err)
	}
	config := map[string]any{
		"log":      map[string]any{"disabled": true},
		"inbounds": []any{inbound},
		"outbounds": []any{
			map[string]any{"type": "direct", "tag": "direct"},
			map[string]any{"type": "block", "tag": "block"},
		},
		"route": routeConfig,
	}
	if dnsConfig != nil {
		config["dns"] = dnsConfig
	}
	content, err := json.Marshal(config)
	if err != nil {
		return nil, nil, fmt.Errorf("encode sing-box configuration: %w", err)
	}
	ctx, cancel := context.WithCancel(include.Context(context.Background()))
	options, err := singJSON.UnmarshalExtendedContext[option.Options](ctx, content)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("decode sing-box configuration: %w", err)
	}
	instance, err := singbox.New(singbox.Options{Context: ctx, Options: options})
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("create official sing-box runtime: %w", err)
	}
	instance.Router().AppendTracker(&connectionTracker{runtime: r})
	return instance, cancel, nil
}

func normalizeECHPEM(value, blockType string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is empty", strings.ToLower(blockType))
	}
	if block, rest := pem.Decode([]byte(value)); block != nil {
		if block.Type != blockType || strings.TrimSpace(string(rest)) != "" {
			return "", fmt.Errorf("invalid %s PEM", strings.ToLower(blockType))
		}
		return value, nil
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) == 0 {
		return "", fmt.Errorf("invalid base64 %s", strings.ToLower(blockType))
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: raw})), nil
}

func (r *runtime) stopLocked() error {
	instance := r.instance
	cancel := r.cancel
	r.instance = nil
	r.cancel = nil
	r.CloseAllConnections()
	if cancel != nil {
		cancel()
	}
	if instance == nil {
		return nil
	}
	if err := instance.Close(); err != nil {
		return fmt.Errorf("close official sing-box runtime: %w", err)
	}
	return nil
}

func (r *runtime) rebuildPolicyLocked() {
	byName := make(map[string]panel.UserInfo, len(r.users))
	r.policyMu.Lock()
	for _, user := range r.users {
		byName[r.userName(user.Id)] = user
		if r.counters[user.Id] == nil {
			r.counters[user.Id] = &userCounter{}
		}
	}
	r.byName = byName
	r.policyMu.Unlock()
}

func (r *runtime) userName(uid int) string {
	identifier := r.info.Common.EffectivePanelIdentifier(r.info.Id)
	return panel.BuildPanelUserName(identifier, uid)
}

func (r *runtime) userForName(name string) (panel.UserInfo, *userCounter, bool) {
	r.policyMu.RLock()
	user, ok := r.byName[name]
	counter := r.counters[user.Id]
	r.policyMu.RUnlock()
	return user, counter, ok && counter != nil
}

func (r *runtime) loadTraffic(uid int) (shared.TrafficTotal, error) {
	r.policyMu.RLock()
	counter := r.counters[uid]
	r.policyMu.RUnlock()
	if counter == nil {
		return shared.TrafficTotal{}, nil
	}
	return shared.TrafficTotal{
		Upload:   counter.upload.Load(),
		Download: counter.download.Load(),
	}, nil
}

type connectionTracker struct {
	runtime *runtime
}

func (t *connectionTracker) RoutedConnection(_ context.Context, conn net.Conn, metadata adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) net.Conn {
	if !t.runtime.accepting.Load() {
		_ = conn.Close()
		return conn
	}
	user, counter, ok := t.runtime.userForName(metadata.User)
	if !ok {
		_ = conn.Close()
		return conn
	}
	limited, release, accepted := t.runtime.OpenConnection(user, conn, true)
	if !accepted {
		log.WithFields(log.Fields{
			"tag":    t.runtime.tag,
			"user":   metadata.User,
			"source": metadata.Source.String(),
		}).Debug("Naive connection rejected by user or device policy")
		_ = conn.Close()
		return conn
	}
	counted := bufio.NewCounterConn(
		limited,
		[]N.CountFunc{func(value int64) { counter.upload.Add(value) }},
		[]N.CountFunc{func(value int64) { counter.download.Add(value) }},
	)
	return &closeHookConn{ExtendedConn: counted, release: release}
}

func (t *connectionTracker) RoutedPacketConnection(_ context.Context, conn N.PacketConn, metadata adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) N.PacketConn {
	if !t.runtime.accepting.Load() || !t.runtime.info.Common.ProtocolSettings.UDPOverTCP {
		_ = conn.Close()
		return conn
	}
	user, counter, ok := t.runtime.userForName(metadata.User)
	if !ok {
		_ = conn.Close()
		return conn
	}
	bucket, release, accepted := t.runtime.OpenPacketConnection(user, conn, metadata.Source.String(), true)
	if !accepted {
		_ = conn.Close()
		return conn
	}
	limited := conn
	if bucket != nil {
		limited = rate.NewPacketConnRateLimiter(limited, bucket)
	}
	counted := bufio.NewCounterPacketConn(
		limited,
		[]N.CountFunc{func(value int64) { counter.upload.Add(value) }},
		[]N.CountFunc{func(value int64) { counter.download.Add(value) }},
	)
	return &closeHookPacketConn{PacketConn: counted, release: release}
}

type closeHookConn struct {
	N.ExtendedConn
	release func()
	once    sync.Once
}

func (c *closeHookConn) Close() error {
	err := c.ExtendedConn.Close()
	c.once.Do(c.release)
	return err
}

func (c *closeHookConn) Upstream() any {
	return c.ExtendedConn
}

type closeHookPacketConn struct {
	N.PacketConn
	release func()
	once    sync.Once
}

func (c *closeHookPacketConn) Close() error {
	err := c.PacketConn.Close()
	c.once.Do(c.release)
	return err
}

func (c *closeHookPacketConn) Upstream() any {
	return c.PacketConn
}
