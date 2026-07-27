package shared

import (
	"io"
	"net"
	"reflect"
	"testing"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/limiter"
)

func TestTrafficCommitAndRemovedUserFlushContract(t *testing.T) {
	tag := "test-" + t.Name()
	key := trafficKey{tag: tag, uid: 42}
	committedTraffic.Delete(key)
	t.Cleanup(func() { committedTraffic.Delete(key) })

	service := NewRuntimeServices(tag)
	user := panel.UserInfo{Id: 42, Uuid: "credential", DeviceLimit: 2}
	service.SyncUsers(nil, []panel.UserInfo{user})
	service.RecordUpload(user.Id, 1500)
	service.RecordDownload(user.Id, 600)

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

	service.RecordUpload(user.Id, 200)
	service.RecordDownload(user.Id, 100)
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

func TestOpenConnectionCountsTrafficAndReleasesDevice(t *testing.T) {
	tag := "test-" + t.Name()
	key := trafficKey{tag: tag, uid: 7}
	committedTraffic.Delete(key)
	t.Cleanup(func() { committedTraffic.Delete(key) })
	limiter.Init()
	user := panel.UserInfo{Id: 7, Uuid: "stream-user", DeviceLimit: 1}
	currentLimiter := limiter.AddLimiter("test", tag, 0, []panel.UserInfo{user}, map[int]int{})
	t.Cleanup(func() { limiter.DeleteLimiter(tag) })
	service := NewRuntimeServices(tag)
	service.SyncUsers(nil, []panel.UserInfo{user})

	server, client := net.Pipe()
	wrapped, release, ok := service.OpenConnection(user, server, true)
	if !ok {
		t.Fatal("OpenConnection() rejected current user")
	}
	t.Cleanup(func() { _ = client.Close() })

	writeDone := make(chan error, 1)
	go func() {
		_, err := client.Write([]byte("upload"))
		writeDone <- err
	}()
	buffer := make([]byte, len("upload"))
	if _, err := io.ReadFull(wrapped, buffer); err != nil {
		t.Fatalf("read upload: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write upload: %v", err)
	}

	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, len("download"))
		_, err := io.ReadFull(client, buffer)
		readDone <- err
	}()
	if _, err := wrapped.Write([]byte("download")); err != nil {
		t.Fatalf("write download: %v", err)
	}
	if err := <-readDone; err != nil {
		t.Fatalf("read download: %v", err)
	}

	traffic, err := service.Traffic(0)
	if err != nil {
		t.Fatalf("Traffic() error = %v", err)
	}
	want := []panel.UserTraffic{{UID: user.Id, Upload: 6, Download: 8}}
	if !reflect.DeepEqual(traffic, want) {
		t.Fatalf("Traffic() = %#v, want %#v", traffic, want)
	}

	if err := wrapped.Close(); err != nil {
		t.Fatalf("close accounted stream: %v", err)
	}
	release()
	online, err := currentLimiter.GetOnlineDevice()
	if err != nil {
		t.Fatalf("GetOnlineDevice() error = %v", err)
	}
	if len(*online) != 0 {
		t.Fatalf("closed stream remained online: %#v", *online)
	}
}

func TestPacketSessionUsesCommonTrafficAndUserLookup(t *testing.T) {
	tag := "test-" + t.Name()
	key := trafficKey{tag: tag, uid: 9}
	committedTraffic.Delete(key)
	t.Cleanup(func() { committedTraffic.Delete(key) })
	limiter.Init()
	user := panel.UserInfo{Id: 9, Uuid: "packet-user"}
	limiter.AddLimiter("test", tag, 0, []panel.UserInfo{user}, map[int]int{})
	t.Cleanup(func() { limiter.DeleteLimiter(tag) })
	service := NewRuntimeServices(tag)
	service.SyncUsers(nil, []panel.UserInfo{user})

	if found, ok := service.UserByUUID(user.Uuid); !ok || found != user {
		t.Fatalf("UserByUUID() = %#v, %v", found, ok)
	}
	closer := &testCloser{}
	session, ok := service.OpenSession(user, closer, "198.51.100.10:443", true)
	if !ok {
		t.Fatal("OpenSession() rejected current user")
	}
	session.RecordUpload(1200)
	session.RecordDownload(450)

	traffic, err := service.Traffic(1)
	if err != nil {
		t.Fatalf("Traffic() error = %v", err)
	}
	want := []panel.UserTraffic{{UID: user.Id, Upload: 1200, Download: 450}}
	if !reflect.DeepEqual(traffic, want) {
		t.Fatalf("Traffic() = %#v, want %#v", traffic, want)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Session.Close() error = %v", err)
	}
	if !closer.closed {
		t.Fatal("Session.Close() did not close protocol resource")
	}
}

type testCloser struct {
	closed bool
}

func (c *testCloser) Close() error {
	c.closed = true
	return nil
}
