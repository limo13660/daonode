package naive

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/core/contract"
	"github.com/limo13660/daonode/core/shared"
)

const runtimeStopTimeout = 10 * time.Second

type runtime struct {
	*shared.RuntimeServices

	tag  string
	info *panel.NodeInfo

	mu       sync.Mutex
	instance *serverInstance
	users    map[int]panel.UserInfo
}

// NewRuntime creates a DaoNode adapter around NaiveProxy's official server,
// the padding-enabled klzgrad/forwardproxy Caddy module.
func NewRuntime(tag string, info *panel.NodeInfo) contract.Runtime {
	r := &runtime{
		tag:   tag,
		info:  info,
		users: make(map[int]panel.UserInfo),
	}
	r.RuntimeServices = shared.NewRuntimeServices(tag)
	return r
}

func (r *runtime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restartLocked()
}

func (r *runtime) Stop() error {
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
		return fmt.Errorf("%w while stopping official Naive runtime", contract.ErrRuntimeStopTimeout)
	}
}

func (r *runtime) Validate(users []panel.UserInfo) error {
	if err := validateNodeInfo(r.info); err != nil {
		return err
	}
	userMap := make(map[int]panel.UserInfo, len(users))
	for _, user := range users {
		userMap[user.Id] = user
	}
	if _, err := newAuthSnapshot(r.info, userMap); err != nil {
		return err
	}
	_, err := buildTLSConfig(r.info.Common)
	return err
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

	snapshot, err := newAuthSnapshot(r.info, r.users)
	if err != nil {
		r.users = previous
		return err
	}
	if len(r.users) == 0 {
		if err := r.stopLocked(); err != nil {
			r.users = previous
			return err
		}
		r.RuntimeServices.SyncUsers(deleted, added)
		return nil
	}
	if r.instance == nil {
		instance, err := startServer(r.info, r.RuntimeServices, snapshot)
		if err != nil {
			r.users = previous
			return err
		}
		r.instance = instance
	} else {
		r.instance.handler.snapshot.Store(snapshot)
	}
	r.RuntimeServices.SyncUsers(deleted, added)
	log.WithFields(log.Fields{
		"tag":        r.tag,
		"protocol":   "naive",
		"users":      len(r.users),
		"added":      len(added),
		"deleted":    len(deleted),
		"hot_update": r.instance != nil,
	}).Info("Official Naive users synchronized")
	return nil
}

func (r *runtime) Traffic(minTraffic int) ([]panel.UserTraffic, error) {
	return r.RuntimeServices.Traffic(minTraffic)
}

func (r *runtime) CommitTraffic(traffic []panel.UserTraffic) {
	r.RuntimeServices.CommitTraffic(traffic)
}

func (r *runtime) restartLocked() error {
	if err := validateNodeInfo(r.info); err != nil {
		return err
	}
	if len(r.users) == 0 {
		return r.stopLocked()
	}
	snapshot, err := newAuthSnapshot(r.info, r.users)
	if err != nil {
		return err
	}
	instance, err := startServer(r.info, r.RuntimeServices, snapshot)
	if err != nil {
		return err
	}
	if err := r.stopLocked(); err != nil {
		_ = instance.Close()
		return err
	}
	r.instance = instance
	return nil
}

func (r *runtime) stopLocked() error {
	r.RuntimeServices.CloseAllConnections()
	if r.instance == nil {
		return nil
	}
	instance := r.instance
	r.instance = nil
	if err := instance.Close(); err != nil && !errors.Is(err, contract.ErrRuntimeStopTimeout) {
		return fmt.Errorf("close official Naive runtime: %w", err)
	} else {
		return err
	}
}

func validateNodeInfo(info *panel.NodeInfo) error {
	if info == nil || info.Common == nil {
		return fmt.Errorf("Naive node configuration is missing")
	}
	if strings.ToLower(strings.TrimSpace(info.Type)) != "naive" {
		return fmt.Errorf("Naive runtime does not support protocol %s", info.Type)
	}
	transport := strings.ToUpper(strings.TrimSpace(info.Common.TransportProtocol))
	if transport == "" {
		transport = "TCP"
	}
	if transport != "TCP" && transport != "UDP" {
		return fmt.Errorf("unsupported Naive transport: %s", transport)
	}
	cert := info.Common.CertInfo
	if transport == "UDP" && (cert == nil || cert.CertMode == "none") {
		return fmt.Errorf("Naive HTTP/3 requires a TLS certificate")
	}
	return nil
}

var _ contract.Runtime = (*runtime)(nil)
var _ contract.Validator = (*runtime)(nil)
