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
