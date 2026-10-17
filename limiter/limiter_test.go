package limiter

import (
	"testing"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/common/format"
)

func TestReportedDeviceCanReconnectBeforePanelAliveCacheRefresh(t *testing.T) {
	Init()
	const tag = "device-reconnect-test"
	user := panel.UserInfo{Id: 42, Uuid: "credential", DeviceLimit: 1}
	current := AddLimiter("naive", tag, 0, []panel.UserInfo{user}, map[int]int{})
	t.Cleanup(func() { DeleteLimiter(tag) })
	userTag := format.UserTag(tag, user.Uuid)

	if _, rejected := current.CheckLimit(userTag, "198.51.100.10", true); rejected {
		t.Fatal("first connection was rejected")
	}
	current.MarkOnlineDeviceReported([]panel.OnlineUser{{UID: user.Id, IP: "198.51.100.10"}})
	current.SetAliveList(map[int]int{user.Id: 1})
	current.ReleaseConnection(userTag, "198.51.100.10")

	if offline := current.GetPendingOfflineUsers(); len(offline) != 1 || offline[0].UID != user.Id {
		t.Fatalf("pending offline snapshot = %#v, want UID %d", offline, user.Id)
	}
	if _, rejected := current.CheckLimit(userTag, "198.51.100.10", true); rejected {
		t.Fatal("same reported IP was rejected while the panel count was stale")
	}
	current.ReleaseConnection(userTag, "198.51.100.10")
	if _, rejected := current.CheckLimit(userTag, "198.51.100.11", true); !rejected {
		t.Fatal("different IP bypassed the stale device-limit count")
	}

	offline := current.GetPendingOfflineUsers()
	current.SetAliveList(map[int]int{})
	current.MarkOfflineUsersReported(offline)
	if _, rejected := current.CheckLimit(userTag, "198.51.100.11", true); rejected {
		t.Fatal("new IP remained blocked after the panel accepted the offline report")
	}
}

func TestDelayedOfflineAcknowledgementDoesNotEraseReconnect(t *testing.T) {
	Init()
	const tag = "offline-ack-race-test"
	user := panel.UserInfo{Id: 7, Uuid: "credential", DeviceLimit: 1}
	current := AddLimiter("naive", tag, 0, []panel.UserInfo{user}, map[int]int{})
	t.Cleanup(func() { DeleteLimiter(tag) })
	userTag := format.UserTag(tag, user.Uuid)

	if _, rejected := current.CheckLimit(userTag, "203.0.113.7", true); rejected {
		t.Fatal("first connection was rejected")
	}
	current.MarkOnlineDeviceReported([]panel.OnlineUser{{UID: user.Id, IP: "203.0.113.7"}})
	current.SetAliveList(map[int]int{user.Id: 1})
	current.ReleaseConnection(userTag, "203.0.113.7")
	staleSnapshot := current.GetPendingOfflineUsers()

	if _, rejected := current.CheckLimit(userTag, "203.0.113.7", true); rejected {
		t.Fatal("reconnect was rejected")
	}
	current.MarkOfflineUsersReported(staleSnapshot)
	current.ReleaseConnection(userTag, "203.0.113.7")
	if offline := current.GetPendingOfflineUsers(); len(offline) != 1 {
		t.Fatalf("delayed acknowledgement erased the reconnect state: %#v", offline)
	}
}

func TestPerUserSpeedLimitIsCreatedAndUpdated(t *testing.T) {
	Init()
	const tag = "user-speed-limit-test"
	limited := panel.UserInfo{Id: 10, Uuid: "limited", SpeedLimit: 10}
	unlimited := panel.UserInfo{Id: 11, Uuid: "unlimited"}
	current := AddLimiter("sudoku", tag, 100, []panel.UserInfo{limited, unlimited}, nil)
	t.Cleanup(func() { DeleteLimiter(tag) })

	limitedBucket := current.UserSpeedLimiter(format.UserTag(tag, limited.Uuid))
	if limitedBucket == nil {
		t.Fatal("limited user did not receive a speed bucket")
	}
	if unlimitedBucket := current.UserSpeedLimiter(format.UserTag(tag, unlimited.Uuid)); unlimitedBucket != nil {
		t.Fatal("unlimited user received a speed bucket")
	}
	if effective := current.SpeedLimiter(format.UserTag(tag, limited.Uuid)); effective != current.NodeSpeedLimiter {
		t.Fatal("node speed limit did not take precedence over the user limit")
	}

	current.UpdateUser(tag, nil, nil, []panel.UserInfo{{Id: limited.Id, Uuid: limited.Uuid, SpeedLimit: 20}})
	if got := current.UserSpeedLimiter(format.UserTag(tag, limited.Uuid)); got != limitedBucket {
		t.Fatal("speed-limit update replaced the active bucket")
	}
	current.UpdateUser(tag, nil, nil, []panel.UserInfo{{Id: limited.Id, Uuid: limited.Uuid, SpeedLimit: 0}})
	if got := current.UserSpeedLimiter(format.UserTag(tag, limited.Uuid)); got != nil {
		t.Fatal("clearing speed limit kept a user bucket")
	}
}

func TestUserSpeedLimitIsEffectiveWhenNodeIsUnlimited(t *testing.T) {
	Init()
	const tag = "user-speed-only-test"
	user := panel.UserInfo{Id: 12, Uuid: "speed-only", SpeedLimit: 100}
	current := AddLimiter("sudoku", tag, 0, []panel.UserInfo{user}, nil)
	t.Cleanup(func() { DeleteLimiter(tag) })

	if current.NodeSpeedLimiter != nil {
		t.Fatal("unlimited node unexpectedly created a node bucket")
	}
	userBucket := current.UserSpeedLimiter(format.UserTag(tag, user.Uuid))
	if userBucket == nil {
		t.Fatal("user speed limit did not create a bucket")
	}
	if effective := current.SpeedLimiter(format.UserTag(tag, user.Uuid)); effective != userBucket {
		t.Fatal("user speed limit was not selected for an unlimited node")
	}
}
