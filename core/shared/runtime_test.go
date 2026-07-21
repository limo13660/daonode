package shared

import (
	"reflect"
	"testing"

	panel "github.com/limo13660/daonode/api/v2board"
)

func TestTrafficCommitAndRemovedUserFlushContract(t *testing.T) {
	tag := "test-" + t.Name()
	key := trafficKey{tag: tag, uid: 42}
	committedTraffic.Delete(key)
	t.Cleanup(func() { committedTraffic.Delete(key) })

	totals := map[int]TrafficTotal{
		42: {Upload: 1500, Download: 600},
	}
	service := NewRuntimeServices(tag, func(uid int) (TrafficTotal, error) {
		return totals[uid], nil
	})
	user := panel.UserInfo{Id: 42, Uuid: "credential", DeviceLimit: 2}
	service.SyncUsers(nil, []panel.UserInfo{user})

	first, err := service.Traffic(1)
	if err != nil {
		t.Fatalf("Traffic() error = %v", err)
	}
	wantFirst := []panel.UserTraffic{{UID: 42, Upload: 1500, Download: 600}}
	if !reflect.DeepEqual(first, wantFirst) {
		t.Fatalf("first Traffic() = %#v, want %#v", first, wantFirst)
	}

	stale := append([]panel.UserTraffic(nil), first...)
	stale[0].Upload++
	service.CommitTraffic(stale)
	retry, err := service.Traffic(0)
	if err != nil {
		t.Fatalf("Traffic() after stale commit error = %v", err)
	}
	if !reflect.DeepEqual(retry, wantFirst) {
		t.Fatalf("stale commit advanced baseline: Traffic() = %#v, want %#v", retry, wantFirst)
	}

	service.CommitTraffic(retry)
	unchanged, err := service.Traffic(0)
	if err != nil {
		t.Fatalf("Traffic() after exact commit error = %v", err)
	}
	if len(unchanged) != 0 {
		t.Fatalf("exact commit did not advance baseline: %#v", unchanged)
	}

	totals[42] = TrafficTotal{Upload: 1700, Download: 700}
	belowThreshold, err := service.Traffic(1000)
	if err != nil {
		t.Fatalf("Traffic() below threshold error = %v", err)
	}
	if len(belowThreshold) != 0 {
		t.Fatalf("current user bypassed threshold: %#v", belowThreshold)
	}

	service.SyncUsers([]panel.UserInfo{user}, nil)
	removed, err := service.Traffic(1000)
	if err != nil {
		t.Fatalf("Traffic() for removed user error = %v", err)
	}
	wantRemoved := []panel.UserTraffic{{
		UID:         42,
		Upload:      200,
		Download:    100,
		ForceReport: true,
	}}
	if !reflect.DeepEqual(removed, wantRemoved) {
		t.Fatalf("removed user Traffic() = %#v, want %#v", removed, wantRemoved)
	}
	service.CommitTraffic(removed)

	flushed, err := service.Traffic(0)
	if err != nil {
		t.Fatalf("Traffic() after removed-user commit error = %v", err)
	}
	if len(flushed) != 0 {
		t.Fatalf("removed-user traffic was not flushed: %#v", flushed)
	}
}
