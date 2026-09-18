package sudoku

import (
	"net"
	"testing"
)

func TestReplayConnStopsCachingAfterCommit(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	replay := newReplayConn(server)
	firstDone := make(chan struct{})
	go func() {
		_, _ = client.Write([]byte("hello"))
		close(firstDone)
	}()
	buf := make([]byte, 5)
	if _, err := replay.Read(buf); err != nil {
		t.Fatalf("read handshake bytes: %v", err)
	}
	<-firstDone
	if got := len(replay.cache); got != 5 {
		t.Fatalf("replay cache length = %d, want 5", got)
	}

	replay.Commit()
	if replay.recording || len(replay.cache) != 0 {
		t.Fatalf("replay cache was not released on commit")
	}

	secondDone := make(chan struct{})
	go func() {
		_, _ = client.Write([]byte("proxy traffic"))
		close(secondDone)
	}()
	traffic := make([]byte, len("proxy traffic"))
	if _, err := replay.Read(traffic); err != nil {
		t.Fatalf("read proxy bytes: %v", err)
	}
	<-secondDone
	if got := len(replay.cache); got != 0 {
		t.Fatalf("replay cache grew after commit: %d bytes", got)
	}
}
