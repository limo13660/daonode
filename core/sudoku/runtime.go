package sudoku

import (
	"errors"
	"fmt"
	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/core/contract"
	"github.com/limo13660/daonode/core/shared"
	"maps"
	"strings"
	"sync"
	"time"
)

type runtime struct {
	*shared.RuntimeServices
	tag      string
	info     *panel.NodeInfo
	mu       sync.Mutex
	instance *serverInstance
	users    map[int]panel.UserInfo
}

const runtimeStopTimeout = 10 * time.Second

func NewRuntime(tag string, info *panel.NodeInfo) contract.Runtime {
	return &runtime{RuntimeServices: shared.NewRuntimeServices(tag), tag: tag, info: info, users: map[int]panel.UserInfo{}}
}
func (r *runtime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// V2Core normally starts a Sudoku listener through AddUsers.  Keep the
	// contract's explicit Start operation idempotent so a second lifecycle
	// call cannot race a live listener and fail with EADDRINUSE.
	if r.instance != nil {
		return nil
	}
	return r.restartLocked()
}
func (r *runtime) Stop() error {
	done := make(chan error, 1)
	go func() { r.mu.Lock(); defer r.mu.Unlock(); done <- r.stopLocked() }()
	select {
	case err := <-done:
		return err
	case <-time.After(runtimeStopTimeout):
		return fmt.Errorf("%w while stopping Sudoku runtime", contract.ErrRuntimeStopTimeout)
	}
}
func (r *runtime) Validate(users []panel.UserInfo) error {
	if err := validateNodeInfo(r.info); err != nil {
		return err
	}
	m := make(map[int]panel.UserInfo, len(users))
	for _, u := range users {
		m[u.Id] = u
	}
	_, err := buildUserConfigs(r.info, m)
	return err
}
func (r *runtime) AddUsers(users []panel.UserInfo) (int, error) {
	return len(users), r.SyncUsers(nil, users)
}
func (r *runtime) DelUsers(users []panel.UserInfo) error { return r.SyncUsers(users, nil) }
func (r *runtime) SyncUsers(deleted, added []panel.UserInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	previous := maps.Clone(r.users)
	for _, u := range deleted {
		delete(r.users, u.Id)
	}
	for _, u := range added {
		r.users[u.Id] = u
	}
	if err := validateUsers(r.users); err != nil {
		r.users = previous
		return err
	}
	var previousConfigs *userConfigSnapshot
	if r.instance != nil {
		previousConfigs = r.instance.users.Load()
	}
	configs, err := buildUserConfigsWithPrevious(r.info, r.users, previousConfigs)
	if err != nil {
		r.users = previous
		return err
	}
	if len(r.users) == 0 {
		if err := r.stopLocked(); err != nil {
			r.users = previous
			return err
		}
	} else if r.instance == nil {
		r.instance, err = startServer(r.info, r.RuntimeServices, configs)
		if err != nil {
			r.users = previous
			return err
		}
	} else {
		r.instance.users.Store(configs)
	}
	r.RuntimeServices.SyncUsers(deleted, added)
	return nil
}
func (r *runtime) Traffic(min int) ([]panel.UserTraffic, error) {
	return r.RuntimeServices.Traffic(min)
}
func (r *runtime) CommitTraffic(t []panel.UserTraffic) { r.RuntimeServices.CommitTraffic(t) }
func (r *runtime) restartLocked() error {
	if err := validateNodeInfo(r.info); err != nil {
		return err
	}
	if len(r.users) == 0 {
		return r.stopLocked()
	}
	configs, err := buildUserConfigs(r.info, r.users)
	if err != nil {
		return err
	}
	instance, err := startServer(r.info, r.RuntimeServices, configs)
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
		return fmt.Errorf("close Sudoku runtime: %w", err)
	} else {
		return err
	}
}

func validateNodeInfo(info *panel.NodeInfo) error {
	if info == nil || info.Common == nil {
		return fmt.Errorf("sudoku node configuration is missing")
	}
	if strings.ToLower(strings.TrimSpace(info.Type)) != "sudoku" {
		return fmt.Errorf("Sudoku runtime does not support protocol %s", info.Type)
	}
	if transport := strings.ToUpper(strings.TrimSpace(info.Common.TransportProtocol)); transport != "" && transport != "TCP" {
		return fmt.Errorf("Sudoku transport protocol must be TCP")
	}
	if info.Common.ServerPort < 1 || info.Common.ServerPort > 65535 {
		return fmt.Errorf("invalid Sudoku server port")
	}
	return nil
}
func validateUsers(users map[int]panel.UserInfo) error {
	for _, u := range users {
		if u.Uuid == "" {
			return fmt.Errorf("Sudoku user %d has empty UUID", u.Id)
		}
	}
	return nil
}

var _ contract.Runtime = (*runtime)(nil)
var _ contract.Validator = (*runtime)(nil)
var _ = time.Second
