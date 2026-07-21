package node

import (
	"testing"

	panel "github.com/limo13660/daonode/api/v2board"
)

func TestCompareUserListContract(t *testing.T) {
	oldUsers := []panel.UserInfo{
		{Id: 1, Uuid: "stable", DeviceLimit: 1},
		{Id: 2, Uuid: "old-password", DeviceLimit: 2},
		{Id: 3, Uuid: "deleted", DeviceLimit: 3},
	}
	newUsers := []panel.UserInfo{
		{Id: 1, Uuid: "stable", DeviceLimit: 9},
		{Id: 2, Uuid: "new-password", DeviceLimit: 2},
		{Id: 4, Uuid: "added", DeviceLimit: 4},
	}

	deleted, added, modified := compareUserList(oldUsers, newUsers)

	assertUserIDs(t, "deleted", deleted, map[int]bool{2: true, 3: true})
	assertUserIDs(t, "added", added, map[int]bool{2: true, 4: true})
	assertUserIDs(t, "modified", modified, map[int]bool{1: true})
	if modified[0].DeviceLimit != 9 {
		t.Fatalf("modified user DeviceLimit = %d, want 9", modified[0].DeviceLimit)
	}
}

func assertUserIDs(t *testing.T, label string, users []panel.UserInfo, want map[int]bool) {
	t.Helper()
	if len(users) != len(want) {
		t.Fatalf("%s users = %#v, want IDs %#v", label, users, want)
	}
	for _, user := range users {
		if !want[user.Id] {
			t.Fatalf("%s contains unexpected user ID %d", label, user.Id)
		}
	}
}
