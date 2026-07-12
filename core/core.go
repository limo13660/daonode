package core

import (
	"fmt"
	"sync"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/conf"
)

type AddUsersParams struct {
	Tag   string
	Users []panel.UserInfo
	*panel.NodeInfo
}

type protocolRuntime interface {
	Start() error
	Stop() error
	AddUsers([]panel.UserInfo) (int, error)
	DelUsers([]panel.UserInfo) error
	Traffic(int) ([]panel.UserTraffic, error)
	CommitTraffic([]panel.UserTraffic)
}

type V2Core struct {
	Config   *conf.Conf
	ReloadCh chan struct{}

	mu       sync.RWMutex
	runtimes map[string]protocolRuntime
}

func New(config *conf.Conf) *V2Core {
	return &V2Core{
		Config:   config,
		runtimes: make(map[string]protocolRuntime),
	}
}

func (v *V2Core) Start(_ []*panel.NodeInfo) error {
	return nil
}

func (v *V2Core) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	var firstErr error
	for tag, runtime := range v.runtimes {
		if err := runtime.Stop(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("stop runtime %s: %w", tag, err)
		}
	}
	v.runtimes = make(map[string]protocolRuntime)
	return firstErr
}

func (v *V2Core) AddNode(tag string, info *panel.NodeInfo) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if _, exists := v.runtimes[tag]; exists {
		return fmt.Errorf("node %s already exists", tag)
	}

	var runtime protocolRuntime
	switch info.Type {
	case "mieru":
		runtime = newMieruRuntime(tag, info)
	default:
		return fmt.Errorf("unsupported protocol: %s", info.Type)
	}
	v.runtimes[tag] = runtime
	return nil
}

func (v *V2Core) DelNode(tag string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	runtime, exists := v.runtimes[tag]
	if !exists {
		return nil
	}
	delete(v.runtimes, tag)
	return runtime.Stop()
}

func (v *V2Core) AddUsers(params *AddUsersParams) (int, error) {
	runtime, err := v.runtime(params.Tag)
	if err != nil {
		return 0, err
	}
	return runtime.AddUsers(params.Users)
}

func (v *V2Core) DelUsers(users []panel.UserInfo, tag string, _ *panel.NodeInfo) error {
	runtime, err := v.runtime(tag)
	if err != nil {
		return err
	}
	return runtime.DelUsers(users)
}

func (v *V2Core) GetUserTrafficSlice(tag string, minTraffic int) ([]panel.UserTraffic, error) {
	runtime, err := v.runtime(tag)
	if err != nil {
		return nil, err
	}
	return runtime.Traffic(minTraffic)
}

func (v *V2Core) CommitUserTraffic(tag string, traffic []panel.UserTraffic) {
	runtime, err := v.runtime(tag)
	if err == nil {
		runtime.CommitTraffic(traffic)
	}
}

func (v *V2Core) runtime(tag string) (protocolRuntime, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	runtime, exists := v.runtimes[tag]
	if !exists {
		return nil, fmt.Errorf("node %s does not exist", tag)
	}
	return runtime, nil
}
