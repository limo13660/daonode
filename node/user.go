package node

import (
	"context"
	"errors"

	panel "github.com/limo13660/daonode/api/v2board"
	log "github.com/sirupsen/logrus"
)

func (c *Controller) reportUserTrafficTask(ctx context.Context) (err error) {
	var reportmin = 0
	var devicemin = 0
	if c.info.Common.BaseConfig != nil {
		reportmin = c.info.Common.BaseConfig.NodeReportMinTraffic
		devicemin = c.info.Common.BaseConfig.DeviceOnlineMinTraffic
	}
	// Keep the traffic snapshot threshold independent from the online-device
	// threshold. This matches v2node and ensures every returned item is either
	// reported or retained for a later retry.
	userTraffic, err := c.server.GetUserTrafficSlice(c.tag, reportmin)
	if err != nil {
		return err
	}
	if len(userTraffic) > 0 {
		err = c.apiClient.ReportUserTraffic(ctx, userTraffic)
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Info("Report user traffic failed")
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		} else {
			c.server.CommitUserTraffic(c.tag, userTraffic)
			log.WithField("tag", c.tag).Infof("Report %d users traffic", len(userTraffic))
			//log.WithField("tag", c.tag).Debugf("User traffic: %+v", userTraffic)
		}
	}

	if onlineDevice, err := c.limiter.GetOnlineDevice(); err != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Info("Get online device failed")
	} else if len(*onlineDevice) > 0 {
		result := filterOnlineUsers(*onlineDevice, userTraffic, devicemin)
		data := make(map[int][]string)
		for _, onlineuser := range result {
			// json structure: { UID1:["ip1","ip2"],UID2:["ip3","ip4"] }
			data[onlineuser.UID] = append(data[onlineuser.UID], onlineuser.IP)
		}
		if len(data) != 0 {
			err := c.apiClient.ReportNodeOnlineUsers(ctx, &data)
			if err != nil {
				log.WithFields(log.Fields{
					"tag": c.tag,
					"err": err,
				}).Info("Report online users failed")
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
			} else {
				c.limiter.MarkOnlineDeviceReported(result)
			}
		}
		log.WithField("tag", c.tag).Infof("Total %d online users, %d Reported", len(*onlineDevice), len(result))
	}

	return nil
}

// flushUserTraffic makes one best-effort submission after the
// periodic tasks have stopped and before the runtime counters are discarded.
// A failed submission is deliberately left uncommitted.
func (c *Controller) flushUserTraffic(ctx context.Context) error {
	userTraffic, err := c.server.GetUserTrafficSlice(c.tag, 0)
	if err != nil {
		return err
	}
	if len(userTraffic) == 0 {
		return nil
	}
	if err := c.apiClient.ReportUserTraffic(ctx, userTraffic); err != nil {
		return err
	}
	c.server.CommitUserTraffic(c.tag, userTraffic)
	log.WithField("tag", c.tag).Infof("Flushed %d users traffic before runtime stop", len(userTraffic))
	return nil
}

func filterOnlineUsers(online []panel.OnlineUser, traffic []panel.UserTraffic, minimum int) []panel.OnlineUser {
	if minimum <= 0 {
		return online
	}
	threshold := int64(minimum) * 1000
	// Match v2node's semantics: only users explicitly present in this traffic
	// snapshot and below the device threshold are excluded. A connected user
	// with no traffic in this interval is still reported to /alive.
	noCountUID := make(map[int]struct{}, len(traffic))
	for _, item := range traffic {
		if item.Upload+item.Download < threshold {
			noCountUID[item.UID] = struct{}{}
		}
	}
	result := make([]panel.OnlineUser, 0, len(online))
	for _, item := range online {
		if _, excluded := noCountUID[item.UID]; !excluded {
			result = append(result, item)
		}
	}
	return result
}

func compareUserList(old, new []panel.UserInfo) (deleted, added, modified []panel.UserInfo) {
	oldMap := make(map[string]panel.UserInfo, len(old))
	for _, u := range old {
		oldMap[u.Uuid] = u
	}

	for _, u := range new {
		if o, ok := oldMap[u.Uuid]; !ok {
			added = append(added, u)
		} else {
			if o.Id != u.Id {
				deleted = append(deleted, o)
				added = append(added, u)
			} else if o.DeviceLimit != u.DeviceLimit || o.SpeedLimit != u.SpeedLimit {
				modified = append(modified, u)
			}
			delete(oldMap, u.Uuid)
		}
	}

	for _, o := range oldMap {
		deleted = append(deleted, o)
	}

	return deleted, added, modified
}
