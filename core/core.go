package core

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/conf"
	"github.com/limo13660/daonode/core/contract"
	"github.com/limo13660/daonode/core/mieru"
)

// ErrRuntimeStopTimeout is kept at the root package for callers that do not
// need to know which kernel produced the lifecycle error.
var ErrRuntimeStopTimeout = contract.ErrRuntimeStopTimeout

type AddUsersParams struct {
	Tag   string
	Users []panel.UserInfo
	*panel.NodeInfo
}

type KernelCapability struct {
	Name      string
	Protocols []string
}

type kernelDefinition struct {
	protocols  map[string]struct{}
	newRuntime func(string, *panel.NodeInfo) contract.Runtime
}

var kernelDefinitions = map[string]kernelDefinition{
	"mieru": {
		protocols:  map[string]struct{}{"mieru": {}},
		newRuntime: mieru.NewRuntime,
	},
}

type V2Core struct {
	Config   *conf.Conf
	ReloadCh chan struct{}

	mu       sync.RWMutex
	runtimes map[string]contract.Runtime
}

func New(config *conf.Conf) *V2Core {
	return &V2Core{
		Config:   config,
		runtimes: make(map[string]contract.Runtime),
	}
}

func (v *V2Core) Start(nodes []*panel.NodeInfo) error {
	for _, info := range nodes {
		if _, _, err := normalizeNodeSelection(info); err != nil {
			return err
		}
	}
	return nil
}

func (v *V2Core) Close() error {
	v.mu.Lock()
	runtimes := v.runtimes
	v.runtimes = make(map[string]contract.Runtime)
	v.mu.Unlock()

	var stops sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for tag, runtime := range runtimes {
		stops.Add(1)
		go func(currentTag string, currentRuntime contract.Runtime) {
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

	kernel, _, err := normalizeNodeSelection(info)
	if err != nil {
		return err
	}
	runtime := kernelDefinitions[kernel].newRuntime(tag, info)
	v.runtimes[tag] = runtime
	return nil
}

// KernelCapabilities returns the protocols implemented by each compiled-in
// kernel. Adding another core/<kernel> adapter requires registering it here.
func KernelCapabilities() []KernelCapability {
	capabilities := make([]KernelCapability, 0, len(kernelDefinitions))
	for name, definition := range kernelDefinitions {
		protocols := make([]string, 0, len(definition.protocols))
		for protocol := range definition.protocols {
			protocols = append(protocols, protocol)
		}
		sort.Strings(protocols)
		capabilities = append(capabilities, KernelCapability{Name: name, Protocols: protocols})
	}
	sort.Slice(capabilities, func(i, j int) bool {
		return capabilities[i].Name < capabilities[j].Name
	})
	return capabilities
}

// Supports reports whether a compiled-in kernel implements a protocol.
func Supports(kernel, protocol string) bool {
	kernel = strings.ToLower(strings.TrimSpace(kernel))
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	definition, ok := kernelDefinitions[kernel]
	if !ok {
		return false
	}
	_, ok = definition.protocols[protocol]
	return ok
}

func normalizeNodeSelection(info *panel.NodeInfo) (string, string, error) {
	if info == nil {
		return "", "", fmt.Errorf("node info is nil")
	}
	protocol := strings.ToLower(strings.TrimSpace(info.Type))
	kernel := strings.ToLower(strings.TrimSpace(info.Kernel))
	if info.Common != nil {
		if protocol == "" {
			protocol = strings.ToLower(strings.TrimSpace(info.Common.Protocol))
		}
		if kernel == "" {
			kernel = strings.ToLower(strings.TrimSpace(info.Common.Kernel))
		}
	}
	if protocol == "" {
		return "", "", fmt.Errorf("node protocol is empty")
	}
	if kernel == "" {
		return "", "", fmt.Errorf("node kernel is empty for protocol %s", protocol)
	}
	if _, ok := kernelDefinitions[kernel]; !ok {
		return "", "", fmt.Errorf("unsupported kernel: %s", kernel)
	}
	if !Supports(kernel, protocol) {
		return "", "", fmt.Errorf("kernel %s does not support protocol %s", kernel, protocol)
	}
	info.Type = protocol
	info.Kernel = kernel
	if info.Common != nil {
		info.Common.Protocol = protocol
		info.Common.Kernel = kernel
	}
	return kernel, protocol, nil
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
