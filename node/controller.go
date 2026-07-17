package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"

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

// Start implement the Start() function of the service interface
func (c *Controller) Start(x *core.V2Core) error {
	// Init Core
	c.server = x
	var err error
	// First fetch Node Info
	node := c.info
	if node == nil {
		c.info, err = c.apiClient.GetNodeInfo(context.Background())
		if err != nil {
			return fmt.Errorf("get node info error: %s", err)
		}
		node = c.info
	}
	// Update user
	c.userList, err = c.apiClient.GetUserList(context.Background())
	if err != nil {
		return fmt.Errorf("get user list error: %s", err)
	}
	c.aliveMap, err = c.apiClient.GetUserAlive(context.Background())
	if err != nil {
		log.WithFields(log.Fields{"tag": node.Tag, "err": err}).Warn("Get initial alive list failed")
		c.aliveMap = make(map[int]int)
	}
	c.tag = node.Tag

	if node.Security == panel.Tls {
		if err := c.requestCert(); err != nil {
			return fmt.Errorf("request cert error: %s", err)
		}
	}
	// Add new tag
	err = c.server.AddNode(c.tag, node)
	if err != nil {
		return fmt.Errorf("add new node error: %s", err)
	}
	c.limiter = limiter.AddLimiter(
		c.info.Type,
		c.tag,
		c.info.Common.SpeedLimit,
		c.userList,
		c.aliveMap,
	)
	added, err := c.server.AddUsers(&core.AddUsersParams{
		Tag:      c.tag,
		Users:    c.userList,
		NodeInfo: node,
	})
	if err != nil {
		limiter.DeleteLimiter(c.tag)
		_ = c.server.DelNode(c.tag)
		return fmt.Errorf("add users error: %s", err)
	}
	log.WithField("tag", c.tag).Infof("Added %d new users", added)
	c.info = node
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
