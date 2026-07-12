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
	trafficMin := reportmin
	if devicemin < trafficMin {
		trafficMin = devicemin
	}
	userTraffic, err := c.server.GetUserTrafficSlice(c.tag, trafficMin)
	if err != nil {
		return err
	}
	reportTraffic := filterTrafficByMinimum(userTraffic, reportmin)
	if len(reportTraffic) > 0 {
		err = c.apiClient.ReportUserTraffic(ctx, reportTraffic)
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Info("Report user traffic failed")
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		} else {
			c.server.CommitUserTraffic(c.tag, reportTraffic)
			log.WithField("tag", c.tag).Infof("Report %d users traffic", len(reportTraffic))
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
			}
		}
		log.WithField("tag", c.tag).Infof("Total %d online users, %d Reported", len(*onlineDevice), len(result))
	}

	return nil
}

func filterTrafficByMinimum(traffic []panel.UserTraffic, minimum int) []panel.UserTraffic {
	if minimum <= 0 {
		return traffic
	}
	threshold := int64(minimum * 1000)
	result := make([]panel.UserTraffic, 0, len(traffic))
	for _, item := range traffic {
		if item.Upload+item.Download > threshold {
			result = append(result, item)
		}
	}
	return result
}

func filterOnlineUsers(online []panel.OnlineUser, traffic []panel.UserTraffic, minimum int) []panel.OnlineUser {
	if minimum <= 0 {
		return online
	}
	threshold := int64(minimum * 1000)
	totals := make(map[int]int64, len(traffic))
	for _, item := range traffic {
		totals[item.UID] = item.Upload + item.Download
	}
	result := make([]panel.OnlineUser, 0, len(online))
	for _, item := range online {
		if totals[item.UID] > threshold {
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
			if o.SpeedLimit != u.SpeedLimit || o.DeviceLimit != u.DeviceLimit {
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
