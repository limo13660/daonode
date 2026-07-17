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
		n.controllers[i] = NewController(p, &node, info)
		n.NodeInfos[i] = info
	}
	return n, nil
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
