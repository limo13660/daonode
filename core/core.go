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
	SyncUsers([]panel.UserInfo, []panel.UserInfo) error
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
	runtimes := v.runtimes
	v.runtimes = make(map[string]protocolRuntime)
	v.mu.Unlock()

	var stops sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for tag, runtime := range runtimes {
		stops.Add(1)
		go func(currentTag string, currentRuntime protocolRuntime) {
			defer stops.Done()
			if err := currentRuntime.Stop(); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("stop runtime %s: %w", currentTag, err)
				}
				errMu.Unlock()
			}
		}(tag, runtime)
	}
	stops.Wait()
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
	runtime, exists := v.runtimes[tag]
	if !exists {
		v.mu.Unlock()
		return nil
	}
	delete(v.runtimes, tag)
	v.mu.Unlock()
	return runtime.Stop()
}

func (v *V2Core) AddUsers(params *AddUsersParams) (int, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	runtime, exists := v.runtimes[params.Tag]
	if !exists {
		return 0, fmt.Errorf("node %s does not exist", params.Tag)
	}
	return runtime.AddUsers(params.Users)
}

func (v *V2Core) DelUsers(users []panel.UserInfo, tag string, _ *panel.NodeInfo) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	runtime, exists := v.runtimes[tag]
	if !exists {
		return fmt.Errorf("node %s does not exist", tag)
	}
	return runtime.DelUsers(users)
}

// SyncUsers applies deletions and additions as one runtime transaction so the
// protocol can hot-update credentials or perform one coordinated fallback.
func (v *V2Core) SyncUsers(tag string, deleted, added []panel.UserInfo) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	runtime, exists := v.runtimes[tag]
	if !exists {
		return fmt.Errorf("node %s does not exist", tag)
	}
	return runtime.SyncUsers(deleted, added)
}

func (v *V2Core) GetUserTrafficSlice(tag string, minTraffic int) ([]panel.UserTraffic, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	runtime, exists := v.runtimes[tag]
	if !exists {
		return nil, fmt.Errorf("node %s does not exist", tag)
	}
	return runtime.Traffic(minTraffic)
}

func (v *V2Core) CommitUserTraffic(tag string, traffic []panel.UserTraffic) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	runtime, exists := v.runtimes[tag]
	if exists {
		runtime.CommitTraffic(traffic)
	}
}
