package core

// The server adapter follows the Mieru v3 server integration model from
// github.com/enfein/mieru/v3, which is distributed under GPL-3.0.

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/model"
	mieruserver "github.com/enfein/mieru/v3/apis/server"
	"github.com/enfein/mieru/v3/apis/trafficpattern"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlcommon"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	mierucommon "github.com/enfein/mieru/v3/pkg/common"
	"github.com/enfein/mieru/v3/pkg/protocol"
	"google.golang.org/protobuf/proto"
)

// hotMieruServer implements the public Mieru server interface while retaining
// access to protocol.Mux.SetServerUsers. The upstream interface does not
// expose that safe hot-update operation; closing the mux would interrupt
// established connections.
type hotMieruServer struct {
	mu      sync.Mutex
	running atomic.Bool
	config  *mieruserver.ServerConfig
	mux     *protocol.Mux
}

var _ mieruserver.Server = (*hotMieruServer)(nil)

func newHotMieruServer() *hotMieruServer {
	return &hotMieruServer{}
}

func (s *hotMieruServer) Load() (*mieruserver.ServerConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config == nil {
		return nil, mieruserver.ErrNoServerConfig
	}
	return s.config, nil
}

func (s *hotMieruServer) Store(config *mieruserver.ServerConfig) error {
	if err := validateHotMieruConfig(config); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running.Load() {
		return mieruserver.ErrStoreServerConfigAfterStart
	}
	s.config = config
	return nil
}

func (s *hotMieruServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config == nil {
		return mieruserver.ErrNoServerConfig
	}

	mux := protocol.NewMux(false)
	if s.config.StreamListenerFactory != nil {
		mux.SetStreamListenerFactory(s.config.StreamListenerFactory)
	}
	if s.config.PacketListenerFactory != nil {
		mux.SetPacketListenerFactory(s.config.PacketListenerFactory)
	}
	pattern, err := trafficpattern.NewConfig(s.config.Config.TrafficPattern)
	if err != nil {
		return err
	}
	mux.SetTrafficPattern(pattern).
		SetServerUsers(appctlcommon.UserListToMap(s.config.Config.GetUsers())).
		SetServerUserHintIsMandatory(s.config.Config.GetAdvancedSettings().GetUserHintIsMandatory())
	mtu := mierucommon.DefaultMTU
	if configuredMTU := s.config.Config.GetMtu(); configuredMTU != 0 {
		mtu = int(configuredMTU)
	}
	endpoints, err := appctlcommon.PortBindingsToUnderlayProperties(s.config.Config.GetPortBindings(), mtu)
	if err != nil {
		return err
	}
	mux.SetEndpoints(endpoints)
	if err := mux.Start(); err != nil {
		return err
	}
	s.mux = mux
	s.running.Store(true)
	return nil
}

func (s *hotMieruServer) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running.Store(false)
	if s.mux == nil {
		return nil
	}
	err := s.mux.Close()
	s.mux = nil
	return err
}

func (s *hotMieruServer) IsRunning() bool {
	return s.running.Load()
}

func (s *hotMieruServer) Accept() (net.Conn, *model.Request, error) {
	s.mu.Lock()
	mux := s.mux
	s.mu.Unlock()
	if mux == nil || !s.running.Load() {
		return nil, nil, mieruserver.ErrServerIsNotRunning
	}
	conn, err := mux.Accept()
	if err != nil {
		return nil, nil, err
	}
	if _, ok := conn.(apicommon.UserContext); !ok {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("internal error: connection doesn't implement UserContext interface")
	}
	mierucommon.SetReadTimeout(conn, 10*time.Second)
	defer mierucommon.SetReadTimeout(conn, 0)
	request := &model.Request{}
	if err := request.ReadFromSocks5(conn); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, request, nil
}

func (s *hotMieruServer) UpdateUsers(users []*appctlpb.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.config == nil {
		return mieruserver.ErrNoServerConfig
	}
	updatedConfig := &mieruserver.ServerConfig{
		Config:                proto.Clone(s.config.Config).(*appctlpb.ServerConfig),
		StreamListenerFactory: s.config.StreamListenerFactory,
		PacketListenerFactory: s.config.PacketListenerFactory,
	}
	updatedConfig.Config.Users = users
	if err := validateHotMieruConfig(updatedConfig); err != nil {
		return err
	}
	s.config = updatedConfig
	if s.mux != nil {
		s.mux.SetServerUsers(appctlcommon.UserListToMap(users))
	}
	return nil
}

func validateHotMieruConfig(config *mieruserver.ServerConfig) error {
	validator := mieruserver.NewServer()
	return validator.Store(config)
}
