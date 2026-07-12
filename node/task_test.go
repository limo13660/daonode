package node

import (
	"testing"

	panel "github.com/limo13660/daonode/api/v2board"
)

func TestRenewedUserIsAddedAfterExpiredList(t *testing.T) {
	user := panel.UserInfo{Id: 7, Uuid: "renewed-user", SpeedLimit: 20, DeviceLimit: 2}
	deleted, added, modified := compareUserList([]panel.UserInfo{user}, []panel.UserInfo{})
	if len(deleted) != 1 || len(added) != 0 || len(modified) != 0 {
		t.Fatalf("expire diff = deleted:%v added:%v modified:%v", deleted, added, modified)
	}

	deleted, added, modified = compareUserList([]panel.UserInfo{}, []panel.UserInfo{user})
	if len(deleted) != 0 || len(added) != 1 || len(modified) != 0 {
		t.Fatalf("renew diff = deleted:%v added:%v modified:%v", deleted, added, modified)
	}
}

func TestFilterOnlineUsersRequiresMinimumTraffic(t *testing.T) {
	online := []panel.OnlineUser{
		{UID: 1, IP: "192.0.2.1"},
		{UID: 2, IP: "192.0.2.2"},
		{UID: 3, IP: "192.0.2.3"},
	}
	traffic := []panel.UserTraffic{
		{UID: 1, Upload: 600, Download: 500},
		{UID: 2, Upload: 500, Download: 500},
	}
	got := filterOnlineUsers(online, traffic, 1)
	if len(got) != 1 || got[0].UID != 1 {
		t.Fatalf("filterOnlineUsers() = %+v, want only UID 1", got)
	}
}

func TestFilterTrafficByMinimum(t *testing.T) {
	traffic := []panel.UserTraffic{
		{UID: 1, Upload: 1001},
		{UID: 2, Upload: 1000},
		{UID: 3, Upload: 999},
	}
	got := filterTrafficByMinimum(traffic, 1)
	if len(got) != 1 || got[0].UID != 1 {
		t.Fatalf("filterTrafficByMinimum() = %+v, want only UID 1", got)
	}
}
