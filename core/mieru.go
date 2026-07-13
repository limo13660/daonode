package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/constant"
	"github.com/enfein/mieru/v3/apis/model"
	mieruserver "github.com/enfein/mieru/v3/apis/server"
	"github.com/enfein/mieru/v3/apis/trafficpattern"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	mierucommon "github.com/enfein/mieru/v3/pkg/common"
	"github.com/enfein/mieru/v3/pkg/metrics"
	"github.com/enfein/mieru/v3/pkg/sockopts"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/common/format"
	"github.com/limo13660/daonode/common/rate"
	"github.com/limo13660/daonode/limiter"
)

type trafficTotal struct {
	upload   int64
	download int64
}

// ErrRuntimeStopTimeout means the Mieru lifecycle is no longer safe to
// recover in-process. The service supervisor should replace the process.
var ErrRuntimeStopTimeout = errors.New("mieru runtime stop timed out")

var mieruStopTimeout = 5 * time.Second

var committedTraffic sync.Map // node tag + user ID -> trafficTotal

const (
	udpSocketBufferSize     = 16 << 20
	maxSocks5UDPHeaderSize  = 3 + 1 + 1 + 255 + 2
	maxSocks5UDPPayloadSize = 1 << 16
)

type mieruListenerFactory struct {
	listenIP     string
	listenConfig net.ListenConfig
}

func newMieruListenerFactory(listenIP string) *mieruListenerFactory {
	if strings.TrimSpace(listenIP) == "" {
		listenIP = "0.0.0.0"
	}
	return &mieruListenerFactory{
		listenIP:     listenIP,
		listenConfig: net.ListenConfig{Control: sockopts.DefaultListenerControl()},
	}
}

func (l *mieruListenerFactory) Listen(ctx context.Context, network, address string) (net.Listener, error) {
	address, err := l.rewriteAddress(address)
	if err != nil {
		return nil, err
	}
	return l.listenConfig.Listen(ctx, network, address)
}

func (l *mieruListenerFactory) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	address, err := l.rewriteAddress(address)
	if err != nil {
		return nil, err
	}
	conn, err := l.listenConfig.ListenPacket(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if udpConn, ok := conn.(*net.UDPConn); ok {
		_ = udpConn.SetReadBuffer(udpSocketBufferSize)
		_ = udpConn.SetWriteBuffer(udpSocketBufferSize)
	}
	return conn, nil
}

func (l *mieruListenerFactory) rewriteAddress(address string) (string, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("parse Mieru listener address %q: %w", address, err)
	}
	return net.JoinHostPort(l.listenIP, port), nil
}

type mieruRuntime struct {
	tag  string
	info *panel.NodeInfo

	mu      sync.RWMutex
	server  mieruserver.Server
	stopCh  chan struct{}
	router  *routeEngine
	users   map[int]panel.UserInfo
	pending map[int]trafficTotal
	conns   map[int]map[net.Conn]struct{}
}

func newMieruRuntime(tag string, info *panel.NodeInfo) *mieruRuntime {
	return &mieruRuntime{
		tag:     tag,
		info:    info,
		users:   make(map[int]panel.UserInfo),
		pending: make(map[int]trafficTotal),
		conns:   make(map[int]map[net.Conn]struct{}),
	}
}

func (m *mieruRuntime) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restartLocked()
}

func (m *mieruRuntime) Stop() error {
	done := make(chan error, 1)
	go func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		done <- m.stopLocked(true)
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(mieruStopTimeout):
		return fmt.Errorf("%w while acquiring the runtime lock", ErrRuntimeStopTimeout)
	}
}

func (m *mieruRuntime) AddUsers(users []panel.UserInfo) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := maps.Clone(m.users)
	for _, user := range users {
		m.users[user.Id] = user
	}
	if err := m.restartLocked(); err != nil {
		m.users = previous
		if errors.Is(err, ErrRuntimeStopTimeout) {
			return 0, err
		}
		if recoverErr := m.restartLocked(); recoverErr != nil {
			return 0, fmt.Errorf("apply users: %v; restore previous users: %w", err, recoverErr)
		}
		return 0, err
	}
	return len(users), nil
}

func (m *mieruRuntime) DelUsers(users []panel.UserInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := maps.Clone(m.users)
	previousPending := maps.Clone(m.pending)
	for _, user := range users {
		m.closeUserConnectionsLocked(user.Id)
		delete(m.users, user.Id)
		delete(m.pending, user.Id)
	}
	if err := m.restartLocked(); err != nil {
		m.users = previous
		m.pending = previousPending
		if errors.Is(err, ErrRuntimeStopTimeout) {
			return err
		}
		if recoverErr := m.restartLocked(); recoverErr != nil {
			return fmt.Errorf("delete users: %v; restore previous users: %w", err, recoverErr)
		}
		return err
	}
	return nil
}

func (m *mieruRuntime) restartLocked() error {
	router, err := newRouteEngine(m.info.Common.Routes)
	if err != nil {
		return fmt.Errorf("configure routes: %w", err)
	}
	if len(m.users) == 0 {
		if err := m.stopLocked(false); err != nil {
			return err
		}
		m.router = router
		return nil
	}

	portBindings, err := mieruPortBindings(m.info.Common)
	if err != nil {
		return err
	}
	users := make([]*appctlpb.User, 0, len(m.users))
	for _, user := range m.users {
		name := m.userName(user.Id)
		users = append(users, &appctlpb.User{
			Name:     proto.String(name),
			Password: proto.String(user.Uuid),
		})
	}

	config := &appctlpb.ServerConfig{
		PortBindings: portBindings,
		Users:        users,
		Mtu:          proto.Int32(int32(m.info.Common.MTU)),
	}
	if m.info.Common.UserHintIsMandatory {
		config.AdvancedSettings = &appctlpb.ServerAdvancedSettings{
			UserHintIsMandatory: proto.Bool(true),
		}
	}
	trafficPattern, err := decodeMieruTrafficPattern(m.info.Common.TrafficPattern)
	if err != nil {
		return fmt.Errorf("decode Mieru traffic pattern: %w", err)
	}
	config.TrafficPattern = trafficPattern
	server := mieruserver.NewServer()
	serverConfig := &mieruserver.ServerConfig{Config: config}
	listenerFactory := newMieruListenerFactory(m.info.Common.ListenIP)
	serverConfig.StreamListenerFactory = listenerFactory
	if mieruHasUDPBinding(portBindings) {
		serverConfig.PacketListenerFactory = listenerFactory
	}
	if err := server.Store(serverConfig); err != nil {
		return fmt.Errorf("store mieru config: %w", err)
	}
	if err := m.stopLocked(false); err != nil {
		return err
	}
	if err := server.Start(); err != nil {
		_ = server.Stop()
		return fmt.Errorf("start mieru server: %w", err)
	}

	m.server = server
	m.router = router
	m.stopCh = make(chan struct{})
	go m.acceptLoop(server, m.stopCh)
	log.WithFields(log.Fields{
		"tag":       m.tag,
		"port":      m.info.Common.ServerPort,
		"listen_ip": m.info.Common.ListenIP,
		"transport": strings.ToUpper(m.info.Common.TransportProtocol),
		"bindings":  len(portBindings),
		"users":     len(m.users),
	}).Info("Mieru runtime started")
	return nil
}

func (m *mieruRuntime) stopLocked(closeConnections bool) error {
	if closeConnections {
		for uid := range m.conns {
			m.closeUserConnectionsLocked(uid)
		}
	}
	if m.server == nil {
		return nil
	}
	if m.stopCh != nil {
		close(m.stopCh)
	}
	server := m.server
	m.server = nil
	m.stopCh = nil
	done := make(chan error, 1)
	go func() {
		done <- server.Stop()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(mieruStopTimeout):
		return fmt.Errorf("%w after %s", ErrRuntimeStopTimeout, mieruStopTimeout)
	}
}

func (m *mieruRuntime) acceptLoop(server mieruserver.Server, stopCh <-chan struct{}) {
	for {
		proxyConn, request, err := server.Accept()
		if err != nil {
			select {
			case <-stopCh:
				return
			default:
				log.WithFields(log.Fields{"tag": m.tag, "err": err}).Error("Accept Mieru connection failed")
				continue
			}
		}
		go m.handleConnection(proxyConn, request)
	}
}

func (m *mieruRuntime) handleConnection(proxyConn net.Conn, request *model.Request) {
	defer proxyConn.Close()
	if request == nil {
		return
	}

	userContext, ok := proxyConn.(apicommon.UserContext)
	if !ok {
		return
	}
	user, ok := m.userByNameAndTrack(userContext.UserName(), proxyConn)
	if !ok {
		return
	}
	defer m.untrackConnection(user.Id, proxyConn)

	nodeLimiter, err := limiter.GetLimiter(m.tag)
	if err != nil {
		return
	}
	ip := remoteIP(proxyConn.RemoteAddr())
	bucket, reject := nodeLimiter.CheckLimit(format.UserTag(m.tag, user.Uuid), ip, true)
	if reject {
		return
	}
	if bucket != nil {
		proxyConn = rate.NewConnRateLimiter(proxyConn, bucket)
	}

	switch request.Command {
	case constant.Socks5ConnectCmd:
		m.handleTCP(proxyConn, request, m.routerSnapshot())
	case constant.Socks5UDPAssociateCmd:
		m.handleUDP(proxyConn, m.routerSnapshot())
	default:
		_ = (&model.Response{
			Reply:    constant.Socks5ReplyCommandNotSupported,
			BindAddr: model.AddrSpec{IP: net.IPv4zero, Port: 0},
		}).WriteToSocks5(proxyConn)
	}
}

func (m *mieruRuntime) handleTCP(proxyConn net.Conn, request *model.Request, router *routeEngine) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target, err := router.dialTCP(ctx, request.DstAddr)
	if err != nil {
		var reply byte = constant.Socks5ReplyConnectionRefused
		if errors.Is(err, errRouteBlocked) {
			reply = constant.Socks5ReplyNotAllowedByRuleSet
		}
		_ = (&model.Response{
			Reply:    reply,
			BindAddr: model.AddrSpec{IP: net.IPv4zero, Port: 0},
		}).WriteToSocks5(proxyConn)
		return
	}
	defer target.Close()

	local, _ := target.LocalAddr().(*net.TCPAddr)
	bind := model.AddrSpec{IP: net.IPv4zero, Port: 0}
	if local != nil {
		bind = model.AddrSpec{IP: local.IP, Port: local.Port}
	}
	if err := (&model.Response{Reply: constant.Socks5ReplySuccess, BindAddr: bind}).WriteToSocks5(proxyConn); err != nil {
		return
	}
	if router.hasTCPProtocolRules() {
		buffer := make([]byte, 4096)
		_ = proxyConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, readErr := proxyConn.Read(buffer)
		_ = proxyConn.SetReadDeadline(time.Time{})
		if n > 0 {
			targetInfo := targetFromAddr("tcp", request.DstAddr)
			if router.decisionWithProtocol(targetInfo, sniffProtocol("tcp", buffer[:n])) == routeBlock {
				return
			}
			if _, err := target.Write(buffer[:n]); err != nil {
				return
			}
		}
		if readErr != nil {
			if netErr, ok := readErr.(net.Error); !ok || !netErr.Timeout() {
				return
			}
		}
	}
	mierucommon.BidiCopy(proxyConn, target)
}

func (m *mieruRuntime) handleUDP(proxyConn net.Conn, router *routeEngine) {
	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return
	}
	defer udpConn.Close()
	_ = udpConn.SetReadBuffer(udpSocketBufferSize)
	_ = udpConn.SetWriteBuffer(udpSocketBufferSize)

	local, ok := udpConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return
	}
	response := &model.Response{
		Reply:    constant.Socks5ReplySuccess,
		BindAddr: model.AddrSpec{IP: net.IPv4zero, Port: local.Port},
	}
	if err := response.WriteToSocks5(proxyConn); err != nil {
		return
	}
	tunnel := apicommon.NewPacketOverStreamTunnel(proxyConn)
	var addrMap sync.Map
	var wg sync.WaitGroup
	var closeOnce sync.Once
	closeConnections := func() {
		_ = udpConn.Close()
		_ = proxyConn.Close()
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1<<16)
		for {
			n, err := tunnel.Read(buf)
			if err != nil {
				closeOnce.Do(closeConnections)
				return
			}
			addr, header, payload, err := parseMieruUDPDatagram(buf[:n])
			if err != nil {
				continue
			}
			resolveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			dst, err := router.resolveUDPAddr(resolveCtx, addr, payload)
			cancel()
			if err != nil {
				if !errors.Is(err, errRouteBlocked) {
					log.WithFields(log.Fields{"tag": m.tag, "destination": addr.String(), "err": err}).Debug("Resolve UDP destination failed")
				}
				continue
			}
			key := canonicalUDPAddrPort(dst.AddrPort())
			if stored, found := addrMap.Load(key); !found || !bytes.Equal(stored.([]byte), header) {
				addrMap.Store(key, append([]byte(nil), header...))
			}
			if _, err := udpConn.WriteToUDPAddrPort(payload, key); err != nil {
				log.WithFields(log.Fields{"tag": m.tag, "destination": dst.String(), "err": err}).Debug("Write UDP destination failed")
			}
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, maxSocks5UDPHeaderSize+maxSocks5UDPPayloadSize)
		payloadBuf := buf[maxSocks5UDPHeaderSize:]
		for {
			n, addr, err := udpConn.ReadFromUDPAddrPort(payloadBuf)
			if err != nil {
				closeOnce.Do(closeConnections)
				return
			}
			addr = canonicalUDPAddrPort(addr)
			var header []byte
			if stored, found := addrMap.Load(addr); found {
				header = stored.([]byte)
			} else {
				header, err = mieruUDPHeader(model.AddrSpec{IP: net.IP(addr.Addr().AsSlice()), Port: int(addr.Port())})
				if err != nil {
					continue
				}
			}
			if len(header) > maxSocks5UDPHeaderSize {
				continue
			}
			packetStart := maxSocks5UDPHeaderSize - len(header)
			copy(buf[packetStart:maxSocks5UDPHeaderSize], header)
			if _, err := tunnel.Write(buf[packetStart : maxSocks5UDPHeaderSize+n]); err != nil {
				closeOnce.Do(closeConnections)
				return
			}
		}
	}()
	wg.Wait()
}

func canonicalUDPAddrPort(addr netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
}

func parseMieruUDPDatagram(packet []byte) (model.AddrSpec, []byte, []byte, error) {
	if len(packet) < 4 || packet[0] != 0 || packet[1] != 0 || packet[2] != 0 {
		return model.AddrSpec{}, nil, nil, errors.New("invalid SOCKS5 UDP datagram")
	}
	reader := bytes.NewReader(packet[3:])
	addr := model.AddrSpec{}
	if err := addr.ReadFromSocks5(reader); err != nil {
		return model.AddrSpec{}, nil, nil, err
	}
	headerLen := len(packet) - reader.Len()
	header := packet[:headerLen]
	return addr, header, packet[headerLen:], nil
}

func mieruUDPHeader(addr model.AddrSpec) ([]byte, error) {
	var buffer bytes.Buffer
	buffer.Write([]byte{0, 0, 0})
	if err := addr.WriteToSocks5(&buffer); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func (m *mieruRuntime) Traffic(minTraffic int) ([]panel.UserTraffic, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	threshold := int64(minTraffic * 1000)
	result := make([]panel.UserTraffic, 0)
	for _, user := range m.users {
		name := m.userName(user.Id)
		current := loadMieruTraffic(name)
		committedValue, _ := committedTraffic.LoadOrStore(m.trafficKey(user.Id), trafficTotal{})
		committed := committedValue.(trafficTotal)
		upload := current.upload - committed.upload
		download := current.download - committed.download
		if upload < 0 || download < 0 {
			committed = trafficTotal{}
			upload = current.upload
			download = current.download
		}
		if upload+download <= threshold {
			continue
		}
		m.pending[user.Id] = current
		result = append(result, panel.UserTraffic{
			UID:      user.Id,
			Upload:   upload,
			Download: download,
		})
	}
	return result, nil
}

func (m *mieruRuntime) CommitTraffic(traffic []panel.UserTraffic) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range traffic {
		current, ok := m.pending[item.UID]
		if !ok {
			continue
		}
		committedTraffic.Store(m.trafficKey(item.UID), current)
		delete(m.pending, item.UID)
	}
}

func (m *mieruRuntime) userName(uid int) string {
	return fmt.Sprintf("%su%d", m.info.Common.UserNamePrefix, uid)
}

func (m *mieruRuntime) userByName(name string) (panel.UserInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.userByNameLocked(name)
}

func (m *mieruRuntime) userByNameLocked(name string) (panel.UserInfo, bool) {
	prefix := m.info.Common.UserNamePrefix + "u"
	idText, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return panel.UserInfo{}, false
	}
	uid, err := strconv.Atoi(idText)
	if err != nil || m.userName(uid) != name {
		return panel.UserInfo{}, false
	}
	user, ok := m.users[uid]
	return user, ok
}

func (m *mieruRuntime) trafficKey(uid int) string {
	return fmt.Sprintf("%s\x00%d", m.tag, uid)
}

func (m *mieruRuntime) userByNameAndTrack(name string, conn net.Conn) (panel.UserInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.userByNameLocked(name)
	if !ok {
		return panel.UserInfo{}, false
	}
	uid := user.Id
	connections := m.conns[uid]
	if connections == nil {
		connections = make(map[net.Conn]struct{})
		m.conns[uid] = connections
	}
	connections[conn] = struct{}{}
	return user, true
}

func (m *mieruRuntime) untrackConnection(uid int, conn net.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	connections := m.conns[uid]
	delete(connections, conn)
	if len(connections) == 0 {
		delete(m.conns, uid)
	}
}

func (m *mieruRuntime) closeUserConnectionsLocked(uid int) {
	for conn := range m.conns[uid] {
		_ = conn.Close()
	}
	delete(m.conns, uid)
}

func (m *mieruRuntime) routerSnapshot() *routeEngine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.router == nil {
		return &routeEngine{}
	}
	return m.router
}

func loadMieruTraffic(userName string) trafficTotal {
	var total trafficTotal
	for _, metric := range metrics.GetMetricsForUser(userName) {
		switch metric.Name() {
		case metrics.UserMetricUploadBytes:
			total.upload = metric.Load()
		case metrics.UserMetricDownloadBytes:
			total.download = metric.Load()
		}
	}
	return total
}

func mieruTransport(value string) (*appctlpb.TransportProtocol, error) {
	switch strings.ToUpper(value) {
	case "TCP", "":
		return appctlpb.TransportProtocol_TCP.Enum(), nil
	case "UDP":
		return appctlpb.TransportProtocol_UDP.Enum(), nil
	default:
		return nil, fmt.Errorf("unsupported Mieru transport: %s", value)
	}
}

func mieruPortBindings(common *panel.CommonNode) ([]*appctlpb.PortBinding, error) {
	bindings := make([]*appctlpb.PortBinding, 0, 1+len(common.PortBindings))
	seen := make(map[string]struct{})
	add := func(value, protocol string) error {
		transport, err := mieruTransport(protocol)
		if err != nil {
			return err
		}
		binding, key, err := newMieruPortBinding(value, transport)
		if err != nil {
			return err
		}
		if _, ok := seen[key]; ok {
			return nil
		}
		seen[key] = struct{}{}
		bindings = append(bindings, binding)
		return nil
	}
	if err := add(strconv.Itoa(common.ServerPort), common.TransportProtocol); err != nil {
		return nil, err
	}
	for _, binding := range common.PortBindings {
		if err := add(binding.ServerPort, binding.Protocol); err != nil {
			return nil, fmt.Errorf("invalid additional Mieru port binding: %w", err)
		}
	}
	return bindings, nil
}

func newMieruPortBinding(value string, transport *appctlpb.TransportProtocol) (*appctlpb.PortBinding, string, error) {
	value = strings.TrimSpace(value)
	if port, err := strconv.Atoi(value); err == nil {
		if port < 1 || port > 65535 {
			return nil, "", fmt.Errorf("port %d is outside 1-65535", port)
		}
		key := fmt.Sprintf("%s/%s", value, transport.String())
		return &appctlpb.PortBinding{Port: proto.Int32(int32(port)), Protocol: transport}, key, nil
	}
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return nil, "", fmt.Errorf("invalid port or port range %q", value)
	}
	from, fromErr := strconv.Atoi(parts[0])
	to, toErr := strconv.Atoi(parts[1])
	if fromErr != nil || toErr != nil || from < 1 || to > 65535 || from > to {
		return nil, "", fmt.Errorf("invalid port range %q", value)
	}
	normalized := fmt.Sprintf("%d-%d", from, to)
	key := fmt.Sprintf("%s/%s", normalized, transport.String())
	return &appctlpb.PortBinding{PortRange: proto.String(normalized), Protocol: transport}, key, nil
}

func mieruHasUDPBinding(bindings []*appctlpb.PortBinding) bool {
	for _, binding := range bindings {
		if binding.GetProtocol() == appctlpb.TransportProtocol_UDP {
			return true
		}
	}
	return false
}

func decodeMieruTrafficPattern(value string) (*appctlpb.TrafficPattern, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	pattern, err := trafficpattern.Decode(value)
	if err != nil {
		return nil, fmt.Errorf("decode official Mieru traffic pattern: %w", err)
	}
	if err := trafficpattern.Validate(pattern); err != nil {
		return nil, fmt.Errorf("validate official Mieru traffic pattern: %w", err)
	}
	return pattern, nil
}

func remoteIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
