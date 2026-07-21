package node

import (
	"context"
	"fmt"
	"sync"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/conf"
	"github.com/limo13660/daonode/core"
	log "github.com/sirupsen/logrus"
)

type Node struct {
	controllers []*Controller
	NodeInfos   []*panel.NodeInfo
}

// Snapshot is the last-known-good panel state required to restart a runtime
// without contacting the panel. It is intentionally kept in memory only.
type Snapshot struct {
	Nodes []SnapshotNode
}

type SnapshotNode struct {
	Config conf.NodeConfig
	Info   *panel.NodeInfo
	Users  []panel.UserInfo
	Alive  map[int]int
}

func New(nodes []conf.NodeConfig) (*Node, error) {
	n := &Node{
		controllers: make([]*Controller, len(nodes)),
		NodeInfos:   make([]*panel.NodeInfo, len(nodes)),
	}
	for i, node := range nodes {
		p, err := panel.New(&node)
		if err != nil {
			return nil, fmt.Errorf(
				"initialize panel client for node [%s-%d]: %w",
				node.APIHost,
				node.NodeID,
				err,
			)
		}
		info, err := p.GetNodeInfo(context.Background())
		if err != nil {
			return nil, fmt.Errorf(
				"get node info for [%s-%d]: %w",
				node.APIHost,
				node.NodeID,
				err,
			)
		}
		if info == nil {
			return nil, fmt.Errorf("panel returned no node info for [%s-%d]", node.APIHost, node.NodeID)
		}
		controller := NewController(p, &node, info)
		if err := controller.Prepare(context.Background()); err != nil {
			return nil, fmt.Errorf(
				"prepare node [%s-%d]: %w",
				node.APIHost,
				node.NodeID,
				err,
			)
		}
		n.controllers[i] = controller
		n.NodeInfos[i] = info
	}
	return n, nil
}

// NewFromSnapshot rebuilds prepared controllers without depending on the
// panel. The restored controllers resume normal panel polling after Start.
func NewFromSnapshot(snapshot *Snapshot) (*Node, error) {
	if snapshot == nil || len(snapshot.Nodes) == 0 {
		return nil, fmt.Errorf("last-known-good node snapshot is empty")
	}
	n := &Node{
		controllers: make([]*Controller, len(snapshot.Nodes)),
		NodeInfos:   make([]*panel.NodeInfo, len(snapshot.Nodes)),
	}
	for i, state := range snapshot.Nodes {
		config := cloneNodeConfig(state.Config)
		info := cloneNodeInfo(state.Info)
		if info == nil {
			return nil, fmt.Errorf("snapshot node %d has no node info", i)
		}
		client, err := panel.New(&config)
		if err != nil {
			return nil, fmt.Errorf("restore panel client for node [%s-%d]: %w", config.APIHost, config.NodeID, err)
		}
		controller := NewController(client, &config, info)
		controller.userList = cloneUsers(state.Users)
		controller.aliveMap = cloneAliveMap(state.Alive)
		controller.tag = info.Tag
		controller.prepared = true
		n.controllers[i] = controller
		n.NodeInfos[i] = info
	}
	return n, nil
}

// Snapshot captures the active credentials and node configuration so a
// failed candidate can be rolled back even when the panel is unavailable.
func (n *Node) Snapshot() (*Snapshot, error) {
	if n == nil || len(n.controllers) == 0 {
		return nil, fmt.Errorf("active node state is empty")
	}
	snapshot := &Snapshot{Nodes: make([]SnapshotNode, len(n.controllers))}
	for i, controller := range n.controllers {
		if controller == nil || controller.conf == nil {
			return nil, fmt.Errorf("active node %d is incomplete", i)
		}
		controller.stateMu.RLock()
		state := SnapshotNode{
			Config: cloneNodeConfig(*controller.conf),
			Info:   cloneNodeInfo(controller.info),
			Users:  cloneUsers(controller.userList),
			Alive:  cloneAliveMap(controller.aliveMap),
		}
		controller.stateMu.RUnlock()
		if state.Info == nil {
			return nil, fmt.Errorf("active node %d has no node info", i)
		}
		snapshot.Nodes[i] = state
	}
	return snapshot, nil
}

func (n *Node) Start(nodes []conf.NodeConfig, core *core.V2Core) error {
	for i, node := range nodes {
		err := n.controllers[i].Start(core)
		if err != nil {
			for j := i - 1; j >= 0; j-- {
				if closeErr := n.controllers[j].Close(); closeErr != nil {
					log.Errorf("rollback controller failed: %v", closeErr)
				}
			}
			return fmt.Errorf("start node controller [%s-%d] error: %s",
				node.APIHost,
				node.NodeID,
				err)
		}
	}
	return nil
}

func (n *Node) Close() error {
	var closes sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for _, c := range n.controllers {
		closes.Add(1)
		go func(current *Controller) {
			defer closes.Done()
			if err := current.Close(); err != nil {
				log.Errorf("close controller failed: %v", err)
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}(c)
	}
	closes.Wait()
	n.controllers = nil
	return firstErr
}

func cloneNodeConfig(source conf.NodeConfig) conf.NodeConfig {
	result := source
	if source.RetryCount != nil {
		retryCount := *source.RetryCount
		result.RetryCount = &retryCount
	}
	return result
}

func cloneUsers(source []panel.UserInfo) []panel.UserInfo {
	return append([]panel.UserInfo(nil), source...)
}

func cloneAliveMap(source map[int]int) map[int]int {
	result := make(map[int]int, len(source))
	for uid, count := range source {
		result[uid] = count
	}
	return result
}

func cloneNodeInfo(source *panel.NodeInfo) *panel.NodeInfo {
	if source == nil {
		return nil
	}
	result := *source
	if source.Common == nil {
		return &result
	}
	common := *source.Common
	common.PortBindings = append([]panel.PortBinding(nil), source.Common.PortBindings...)
	common.Routes = make([]panel.Route, len(source.Common.Routes))
	for i, route := range source.Common.Routes {
		common.Routes[i] = route
		common.Routes[i].Match = append([]string(nil), route.Match...)
		if route.ActionValue != nil {
			actionValue := *route.ActionValue
			common.Routes[i].ActionValue = &actionValue
		}
	}
	common.TlsSettings.ServerNames = append([]string(nil), source.Common.TlsSettings.ServerNames...)
	if source.Common.BaseConfig != nil {
		base := *source.Common.BaseConfig
		common.BaseConfig = &base
	}
	if source.Common.CertInfo != nil {
		cert := *source.Common.CertInfo
		cert.CertDomains = append([]string(nil), source.Common.CertInfo.CertDomains...)
		cert.DNSEnv = make(map[string]string, len(source.Common.CertInfo.DNSEnv))
		for key, value := range source.Common.CertInfo.DNSEnv {
			cert.DNSEnv[key] = value
		}
		common.CertInfo = &cert
	}
	result.Common = &common
	return &result
}
