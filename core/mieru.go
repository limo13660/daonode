package core

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/constant"
	"github.com/enfein/mieru/v3/apis/model"
	mieruserver "github.com/enfein/mieru/v3/apis/server"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	mierucommon "github.com/enfein/mieru/v3/pkg/common"
	"github.com/enfein/mieru/v3/pkg/metrics"
	"github.com/enfein/mieru/v3/pkg/socks5"
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

var committedTraffic sync.Map // username -> trafficTotal

type mieruRuntime struct {
	tag  string
	info *panel.NodeInfo

	mu      sync.RWMutex
	server  mieruserver.Server
	stopCh  chan struct{}
	users   map[string]panel.UserInfo
	pending map[int]trafficTotal
}

func newMieruRuntime(tag string, info *panel.NodeInfo) *mieruRuntime {
	return &mieruRuntime{
		tag:     tag,
		info:    info,
		users:   make(map[string]panel.UserInfo),
		pending: make(map[int]trafficTotal),
	}
}

func (m *mieruRuntime) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restartLocked()
}

func (m *mieruRuntime) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked()
}

func (m *mieruRuntime) AddUsers(users []panel.UserInfo) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, user := range users {
		m.users[user.Uuid] = user
	}
	if err := m.restartLocked(); err != nil {
		return 0, err
	}
	return len(users), nil
}

func (m *mieruRuntime) DelUsers(users []panel.UserInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, user := range users {
		delete(m.users, user.Uuid)
		delete(m.pending, user.Id)
	}
	return m.restartLocked()
}

func (m *mieruRuntime) restartLocked() error {
	if err := m.stopLocked(); err != nil {
		return err
	}
	if len(m.users) == 0 {
		return nil
	}

	transport, err := mieruTransport(m.info.Common.TransportProtocol)
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
		PortBindings: []*appctlpb.PortBinding{{
			Port:     proto.Int32(int32(m.info.Common.ServerPort)),
			Protocol: transport,
		}},
		Users: users,
		Mtu:   proto.Int32(int32(m.info.Common.MTU)),
	}
	server := mieruserver.NewServer()
	if err := server.Store(&mieruserver.ServerConfig{Config: config}); err != nil {
		return fmt.Errorf("store mieru config: %w", err)
	}
	if err := server.Start(); err != nil {
		return fmt.Errorf("start mieru server: %w", err)
	}

	m.server = server
	m.stopCh = make(chan struct{})
	go m.acceptLoop(server, m.stopCh)
	log.WithFields(log.Fields{
		"tag":       m.tag,
		"port":      m.info.Common.ServerPort,
		"transport": strings.ToUpper(m.info.Common.TransportProtocol),
		"users":     len(m.users),
	}).Info("Mieru runtime started")
	return nil
}

func (m *mieruRuntime) stopLocked() error {
	if m.server == nil {
		return nil
	}
	if m.stopCh != nil {
		close(m.stopCh)
	}
	err := m.server.Stop()
	m.server = nil
	m.stopCh = nil
	return err
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

	userContext, ok := proxyConn.(apicommon.UserContext)
	if !ok {
		return
	}
	user, ok := m.userByName(userContext.UserName())
	if !ok {
		return
	}

	if nodeLimiter, err := limiter.GetLimiter(m.tag); err == nil {
		ip := remoteIP(proxyConn.RemoteAddr())
		bucket, reject := nodeLimiter.CheckLimit(format.UserTag(m.tag, user.Uuid), ip, true)
		if reject {
			return
		}
		if bucket != nil {
			proxyConn = rate.NewConnRateLimiter(proxyConn, bucket)
		}
	}

	switch request.Command {
	case constant.Socks5ConnectCmd:
		m.handleTCP(proxyConn, request)
	case constant.Socks5UDPAssociateCmd:
		m.handleUDP(proxyConn)
	default:
		_ = (&model.Response{
			Reply:    constant.Socks5ReplyCommandNotSupported,
			BindAddr: model.AddrSpec{IP: net.IPv4zero, Port: 0},
		}).WriteToSocks5(proxyConn)
	}
}

func (m *mieruRuntime) handleTCP(proxyConn net.Conn, request *model.Request) {
	target, err := net.DialTimeout("tcp", request.DstAddr.String(), 10*time.Second)
	if err != nil {
		_ = (&model.Response{
			Reply:    constant.Socks5ReplyConnectionRefused,
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
	mierucommon.BidiCopy(proxyConn, target)
}

func (m *mieruRuntime) handleUDP(proxyConn net.Conn) {
	udpConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return
	}
	defer udpConn.Close()

	_, portText, err := net.SplitHostPort(udpConn.LocalAddr().String())
	if err != nil {
		return
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return
	}
	response := &model.Response{
		Reply:    constant.Socks5ReplySuccess,
		BindAddr: model.AddrSpec{IP: net.IPv4zero, Port: port},
	}
	if err := response.WriteToSocks5(proxyConn); err != nil {
		return
	}
	tunnel := apicommon.NewPacketOverStreamTunnel(proxyConn)
	socks5.RunUDPAssociateLoop(udpConn, tunnel, &net.Resolver{})
}

func (m *mieruRuntime) Traffic(minTraffic int) ([]panel.UserTraffic, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	threshold := int64(minTraffic * 1000)
	result := make([]panel.UserTraffic, 0)
	for _, user := range m.users {
		name := m.userName(user.Id)
		current := loadMieruTraffic(name)
		committedValue, _ := committedTraffic.LoadOrStore(name, trafficTotal{})
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
		committedTraffic.Store(m.userName(item.UID), current)
		delete(m.pending, item.UID)
	}
}

func (m *mieruRuntime) userName(uid int) string {
	return fmt.Sprintf("%su%d", m.info.Common.UserNamePrefix, uid)
}

func (m *mieruRuntime) userByName(name string) (panel.UserInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, user := range m.users {
		if m.userName(user.Id) == name {
			return user, true
		}
	}
	return panel.UserInfo{}, false
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
