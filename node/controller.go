package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/common/task"
	"github.com/limo13660/daonode/conf"
	"github.com/limo13660/daonode/core"
	"github.com/limo13660/daonode/limiter"
	log "github.com/sirupsen/logrus"
)

type Controller struct {
	server                  *core.V2Core
	apiClient               *panel.Client
	stateMu                 sync.RWMutex
	tag                     string
	limiter                 *limiter.Limiter
	userList                []panel.UserInfo
	aliveMap                map[int]int
	conf                    *conf.NodeConfig
	info                    *panel.NodeInfo
	active                  bool
	nodeInfoMonitorPeriodic *task.Task
	userReportPeriodic      *task.Task
	renewCertPeriodic       *task.Task
	tcpLatencyProbe         *tcpLatencyProbe
	prepared                bool
}

// NewController return a Node controller with default parameters.
func NewController(api *panel.Client, conf *conf.NodeConfig, info *panel.NodeInfo) *Controller {
	controller := &Controller{
		apiClient: api,
		info:      info,
		conf:      conf,
	}
	return controller
}

// Prepare fetches and validates all panel-owned state that can be obtained
// before the active runtime releases its listening ports. Start consumes this
// cached state, keeping a failed panel request from interrupting the running
// node during a configuration reload.
func (c *Controller) Prepare(ctx context.Context) error {
	node := c.info
	if node == nil {
		info, err := c.apiClient.GetNodeInfo(ctx)
		if err != nil {
			return fmt.Errorf("get node info error: %s", err)
		}
		if info == nil {
			return fmt.Errorf("panel returned no node info")
		}
		node = info
	}

	users, err := c.apiClient.GetUserList(ctx)
	if err != nil {
		return fmt.Errorf("get user list error: %s", err)
	}
	alive, err := c.apiClient.GetUserAlive(ctx)
	if err != nil {
		log.WithFields(log.Fields{"tag": node.Tag, "err": err}).Warn("Get initial alive list failed")
		alive = make(map[int]int)
	}

	c.stateMu.Lock()
	c.info = node
	c.userList = cloneUsers(users)
	c.aliveMap = cloneAliveMap(alive)
	c.tag = node.Tag
	c.stateMu.Unlock()

	if node.Security == panel.Tls {
		if err := c.requestCert(); err != nil {
			return fmt.Errorf("request cert error: %s", err)
		}
	}
	c.prepared = true
	return nil
}

// Start implement the Start() function of the service interface
func (c *Controller) Start(x *core.V2Core) error {
	// Init Core
	c.server = x
	if !c.prepared {
		if err := c.Prepare(context.Background()); err != nil {
			return err
		}
	}
	c.stateMu.RLock()
	node := c.info
	users := cloneUsers(c.userList)
	alive := cloneAliveMap(c.aliveMap)
	tag := c.tag
	c.stateMu.RUnlock()
	if node == nil {
		return fmt.Errorf("prepared node info is missing")
	}
	// Add new tag
	err := c.server.AddNode(tag, node)
	if err != nil {
		return fmt.Errorf("add new node error: %s", err)
	}
	c.limiter = limiter.AddLimiter(
		node.Type,
		tag,
		node.Common.SpeedLimit,
		users,
		alive,
	)
	added, err := c.server.AddUsers(&core.AddUsersParams{
		Tag:      tag,
		Users:    users,
		NodeInfo: node,
	})
	if err != nil {
		limiter.DeleteLimiter(tag)
		_ = c.server.DelNode(tag)
		return fmt.Errorf("add users error: %s", err)
	}
	log.WithField("tag", tag).Infof("Added %d new users", added)
	c.active = true
	c.startTCPLatencyProbe(node)
	c.startTasks(node)
	return nil
}

func (c *Controller) startTCPLatencyProbe(node *panel.NodeInfo) {
	if node == nil || node.Common == nil || !strings.EqualFold(node.Common.TransportProtocol, "UDP") {
		return
	}
	if node.Common.ServerPort == 80 &&
		node.Common.CertInfo != nil &&
		node.Common.CertInfo.CertMode == "http" {
		log.WithFields(log.Fields{
			"tag":  c.tag,
			"port": node.Common.ServerPort,
		}).Warn("TCP latency probe disabled because HTTP ACME renewal requires TCP port 80")
		return
	}
	probe, err := startTCPLatencyProbe(
		node.Common.ListenIP,
		node.Common.ServerPort,
	)
	if err == nil {
		c.tcpLatencyProbe = probe
		log.WithFields(log.Fields{
			"tag":  c.tag,
			"port": node.Common.ServerPort,
		}).Info("Started TCP latency probe for UDP node")
		return
	}

	fields := log.Fields{
		"tag":  c.tag,
		"port": node.Common.ServerPort,
		"err":  err,
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		log.WithFields(fields).Info(
			"TCP latency probe port is already in use; keeping the existing TCP service",
		)
		return
	}
	log.WithFields(fields).Warn("Start TCP latency probe failed")
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	var probeErr error
	if c.tcpLatencyProbe != nil {
		probeErr = c.tcpLatencyProbe.Close()
		c.tcpLatencyProbe = nil
	}
	var tasks sync.WaitGroup
	for _, periodic := range []*task.Task{
		c.nodeInfoMonitorPeriodic,
		c.userReportPeriodic,
		c.renewCertPeriodic,
	} {
		if periodic == nil {
			continue
		}
		tasks.Add(1)
		go func(current *task.Task) {
			defer tasks.Done()
			current.Close()
		}(periodic)
	}
	tasks.Wait()
	if c.active && c.server != nil && c.tag != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.flushUserTraffic(ctx); err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Warn("Flush user traffic before stopping runtime failed")
		}
		cancel()
	}
	var err error
	if c.active && c.server != nil && c.tag != "" {
		err = c.server.DelNode(c.tag)
	}
	if c.active && c.tag != "" {
		limiter.DeleteLimiter(c.tag)
	}
	c.active = false
	if err != nil {
		return fmt.Errorf("del node error: %s", err)
	}
	if probeErr != nil {
		return fmt.Errorf("close TCP latency probe: %s", probeErr)
	}
	return nil
}
