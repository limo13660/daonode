package node

import (
	"context"
	"errors"
	"time"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/common/task"
	vCore "github.com/limo13660/daonode/core"
	log "github.com/sirupsen/logrus"
)

func (c *Controller) startTasks(node *panel.NodeInfo) {
	// fetch node info task
	c.nodeInfoMonitorPeriodic = &task.Task{
		Name:     "nodeInfoMonitor",
		Interval: node.PullInterval,
		Execute:  c.nodeInfoMonitor,
	}
	// fetch user list task
	c.userReportPeriodic = &task.Task{
		Name:     "reportUserTrafficTask",
		Interval: node.PushInterval,
		Execute:  c.reportUserTrafficTask,
	}
	log.WithField("tag", c.tag).Info("Start monitor node status")
	// delay to start nodeInfoMonitor
	_ = c.nodeInfoMonitorPeriodic.Start(false)
	log.WithField("tag", c.tag).Info("Start report node status")
	_ = c.userReportPeriodic.Start(false)
	if node.Security == panel.Tls && c.info.Common.CertInfo != nil {
		switch c.info.Common.CertInfo.CertMode {
		case "none", "", "file":
		default:
			c.renewCertPeriodic = &task.Task{
				Name:     "renewCertTask",
				Interval: 24 * time.Hour,
				Execute:  c.renewCertTask,
			}
			log.WithField("tag", c.tag).Info("Start renew cert")
			_ = c.renewCertPeriodic.Start(true)
		}
	}
}

func (c *Controller) nodeInfoMonitor(ctx context.Context) (err error) {
	// get node info
	newN, err := c.apiClient.GetNodeInfo(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get node info failed")
		return nil
	}
	if newN != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
		}).Info("Node configuration changed, requesting runtime reload")
		if c.server.ReloadCh != nil {
			select {
			case c.server.ReloadCh <- struct{}{}:
			default:
			}
		} else {
			log.Panic("Reload failed")
		}
		// The current controller is about to be replaced. Do not continue with
		// user/alive synchronization against a runtime that is being shut down.
		return nil
	}
	log.WithField("tag", c.tag).Debug("Node info no change")
	return c.syncUsers(ctx)
}

func (c *Controller) syncUsers(ctx context.Context) (err error) {
	// get user info
	newU, err := c.apiClient.GetUserList(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get user list failed")
		return nil
	}
	// node no changed, check users
	if newU != nil {
		deleted, added, modified := compareUserList(c.userList, newU)
		if len(deleted) > 0 || len(added) > 0 {
			err = c.server.SyncUsers(c.tag, deleted, added)
			if err != nil {
				log.WithFields(log.Fields{
					"tag": c.tag,
					"err": err,
				}).Error("Synchronize users failed")
				if errors.Is(err, vCore.ErrRuntimeStopTimeout) {
					c.requestRuntimeReload()
				}
				return nil
			}
		}
		if len(added) > 0 || len(deleted) > 0 || len(modified) > 0 {
			// update Limiter
			c.limiter.UpdateUser(c.tag, added, deleted, modified)
		}
		c.userList = newU
		log.WithField("tag", c.tag).Infof("%d user deleted, %d user added, %d user modified", len(deleted), len(added), len(modified))
	} else {
		log.WithField("tag", c.tag).Debug("User list no change")
	}

	newA, aliveErr := c.apiClient.GetUserAlive(ctx)
	if aliveErr != nil {
		if errors.Is(aliveErr, context.Canceled) || errors.Is(aliveErr, context.DeadlineExceeded) {
			return aliveErr
		}
		log.WithFields(log.Fields{"tag": c.tag, "err": aliveErr}).Error("Get alive list failed")
		return nil
	}
	if newA != nil {
		c.limiter.SetAliveList(newA)
	}
	return nil
}

func (c *Controller) requestRuntimeReload() {
	if c.server.ReloadCh == nil {
		log.WithField("tag", c.tag).Error("Runtime reload channel is unavailable")
		return
	}
	select {
	case c.server.ReloadCh <- struct{}{}:
	default:
	}
}
