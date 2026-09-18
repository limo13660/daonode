package sudoku

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/core/shared"
	transport "github.com/limo13660/daonode/core/sudoku/transport"
	"github.com/limo13660/daonode/limiter"
)

func TestHTTPMaskMultiUserRoutesSecondUUID(t *testing.T) {
	users := map[int]panel.UserInfo{
		1: {Id: 1, Uuid: "4b9edc89-cf4c-4210-bd8e-e8db8a3731ad"},
		2: {Id: 2, Uuid: "959df3cf-197d-4b6d-be9f-1b4ec3ad4e9f"},
		3: {Id: 3, Uuid: "50793b13-733c-49b9-aa4b-5f7006bc22c4"},
	}

	for _, mode := range []string{"stream", "poll", "ws"} {
		t.Run(mode, func(t *testing.T) {
			target := startSudokuTestEchoServer(t)
			server := startMultiUserHTTPMaskServer(t, mode, users)
			conn := dialMultiUserHTTPMask(t, server, mode, users[2].Uuid, 10*time.Second)
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

			encodedTarget, err := transport.EncodeAddress(target)
			if err != nil {
				t.Fatalf("encode echo target: %v", err)
			}
			if err := transport.WriteKIPMessage(conn, transport.KIPTypeOpenTCP, encodedTarget); err != nil {
				t.Fatalf("open TCP session: %v", err)
			}

			payload := []byte("daonode-sudoku-multiuser-" + mode)
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write echo payload: %v", err)
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("read echo payload: %v", err)
			}
			if string(got) != string(payload) {
				t.Fatalf("echo payload = %q, want %q", got, payload)
			}
		})
	}
}

func TestHTTPMaskMultiUserWithoutEarlyHandshakeRoutesSecondUUID(t *testing.T) {
	users := map[int]panel.UserInfo{
		1: {Id: 1, Uuid: "4b9edc89-cf4c-4210-bd8e-e8db8a3731ad"},
		2: {Id: 2, Uuid: "959df3cf-197d-4b6d-be9f-1b4ec3ad4e9f"},
		3: {Id: 3, Uuid: "50793b13-733c-49b9-aa4b-5f7006bc22c4"},
	}

	for _, mode := range []string{"stream", "poll", "ws"} {
		t.Run(mode, func(t *testing.T) {
			target := startSudokuTestEchoServer(t)
			server := startMultiUserSudokuServer(t, mode, users)
			clientCfg, err := newMultiUserClientConfig(server, mode, users[2].Uuid)
			if err != nil {
				t.Fatalf("configure HTTPMask client: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			dial := func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			raw, err := transport.DialHTTPMaskTunnel(ctx, server, clientCfg, dial, nil)
			if err != nil {
				t.Fatalf("dial HTTPMask %s without early handshake: %v", mode, err)
			}
			handshakeCfg := *clientCfg
			handshakeCfg.DisableHTTPMask = true
			conn, err := transport.ClientHandshake(raw, &handshakeCfg)
			if err != nil {
				_ = raw.Close()
				t.Fatalf("handshake HTTPMask %s after upgrade: %v", mode, err)
			}
			defer conn.Close()
			assertSudokuTCPEcho(t, conn, target, "daonode-sudoku-no-early-"+mode)
		})
	}
}

func TestMultiUserRawAndLegacyRouteSecondUUID(t *testing.T) {
	users := map[int]panel.UserInfo{
		1: {Id: 1, Uuid: "4b9edc89-cf4c-4210-bd8e-e8db8a3731ad"},
		2: {Id: 2, Uuid: "959df3cf-197d-4b6d-be9f-1b4ec3ad4e9f"},
		3: {Id: 3, Uuid: "50793b13-733c-49b9-aa4b-5f7006bc22c4"},
	}

	tests := []struct {
		name       string
		serverMode string
		clientMode string
	}{
		{name: "raw", serverMode: "raw", clientMode: "raw"},
		{name: "legacy", serverMode: "legacy", clientMode: "legacy"},
		{name: "raw-fallback-from-stream", serverMode: "stream", clientMode: "raw"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := startSudokuTestEchoServer(t)
			server := startMultiUserSudokuServer(t, test.serverMode, users)
			conn := dialMultiUserSudoku(t, server, test.clientMode, users[2].Uuid, 10*time.Second)
			defer conn.Close()
			assertSudokuTCPEcho(t, conn, target, "daonode-sudoku-multiuser-"+test.name)
		})
	}
}

func TestHTTPMaskMultiUserRejectsUnknownUUIDPromptly(t *testing.T) {
	users := map[int]panel.UserInfo{
		1: {Id: 1, Uuid: "4b9edc89-cf4c-4210-bd8e-e8db8a3731ad"},
		2: {Id: 2, Uuid: "959df3cf-197d-4b6d-be9f-1b4ec3ad4e9f"},
		3: {Id: 3, Uuid: "50793b13-733c-49b9-aa4b-5f7006bc22c4"},
	}

	for _, mode := range []string{"stream", "poll", "ws"} {
		t.Run(mode, func(t *testing.T) {
			server := startMultiUserSudokuServer(t, mode, users)
			started := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := dialMultiUserHTTPMaskContext(ctx, server, mode, "unknown-sudoku-user-key")
			if err == nil {
				t.Fatal("unknown UUID was accepted")
			}
			if elapsed := time.Since(started); elapsed >= 3*time.Second {
				t.Fatalf("unknown UUID rejection took %s", elapsed)
			}
		})
	}
}

func TestEstablishedSessionsReleaseHandshakeSlots(t *testing.T) {
	users := map[int]panel.UserInfo{
		1: {Id: 1, Uuid: "959df3cf-197d-4b6d-be9f-1b4ec3ad4e9f"},
	}
	target := startSudokuTestEchoServer(t)
	server := startMultiUserSudokuServer(t, "raw", users)

	connections := make([]net.Conn, 0, maxConcurrentSudokuHandshakes+1)
	t.Cleanup(func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	})
	for i := 0; i <= maxConcurrentSudokuHandshakes; i++ {
		conn := dialMultiUserSudoku(t, server, "raw", users[1].Uuid, 5*time.Second)
		assertSudokuTCPEcho(t, conn, target, fmt.Sprintf("handshake-slot-%d", i))
		connections = append(connections, conn)
	}
}

func startMultiUserHTTPMaskServer(t *testing.T, mode string, users map[int]panel.UserInfo) string {
	t.Helper()
	return startMultiUserSudokuServer(t, mode, users)
}

func startMultiUserSudokuServer(t *testing.T, mode string, users map[int]panel.UserInfo) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve Sudoku port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	info := sudokuNodeInfo()
	info.Common.ServerPort = port
	if mode != "raw" {
		info.Common.ProtocolSettings.HTTPMask = true
		info.Common.ProtocolSettings.HTTPMaskMode = mode
	}
	info.Common.ProtocolSettings.PaddingMin = 0
	info.Common.ProtocolSettings.PaddingMax = 0
	info.Common.ProtocolSettings.EnablePureDownlink = true
	configs, err := buildUserConfigs(info, users)
	if err != nil {
		t.Fatalf("build Sudoku users: %v", err)
	}

	userList := make([]panel.UserInfo, 0, len(users))
	for _, user := range users {
		userList = append(userList, user)
	}
	tag := fmt.Sprintf("sudoku-httpmask-multiuser-%s-%d", mode, port)
	limiter.Init()
	limiter.AddLimiter("sudoku", tag, 0, userList, nil)
	services := shared.NewRuntimeServices(tag)
	services.SyncUsers(nil, userList)
	instance, err := startServer(info, services, configs)
	if err != nil {
		limiter.DeleteLimiter(tag)
		t.Fatalf("start Sudoku server: %v", err)
	}
	t.Cleanup(func() {
		services.CloseAllConnections()
		if err := instance.Close(); err != nil {
			t.Errorf("close Sudoku server: %v", err)
		}
		limiter.DeleteLimiter(tag)
	})
	return instance.listener.Addr().String()
}

func dialMultiUserHTTPMask(t *testing.T, server, mode, key string, timeout time.Duration) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := dialMultiUserHTTPMaskContext(ctx, server, mode, key)
	if err != nil {
		t.Fatalf("dial HTTPMask %s: %v", mode, err)
	}
	return conn
}

func dialMultiUserSudoku(t *testing.T, server, mode, key string, timeout time.Duration) net.Conn {
	t.Helper()
	if mode != "raw" && mode != "legacy" {
		return dialMultiUserHTTPMask(t, server, mode, key, timeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", server)
	if err != nil {
		t.Fatalf("dial Sudoku %s: %v", mode, err)
	}
	clientCfg, err := newMultiUserClientConfig(server, mode, key)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("configure Sudoku %s: %v", mode, err)
	}
	conn, err := transport.ClientHandshake(raw, clientCfg)
	if err != nil {
		_ = raw.Close()
		t.Fatalf("handshake Sudoku %s: %v", mode, err)
	}
	return conn
}

func dialMultiUserHTTPMaskContext(ctx context.Context, server, mode, key string) (net.Conn, error) {
	clientCfg, err := newMultiUserClientConfig(server, mode, key)
	if err != nil {
		return nil, err
	}
	handshakeCfg := *clientCfg
	handshakeCfg.DisableHTTPMask = true
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	upgrade := func(raw net.Conn) (net.Conn, error) {
		return transport.ClientHandshake(raw, &handshakeCfg)
	}
	return transport.DialHTTPMaskTunnel(ctx, server, clientCfg, dial, upgrade)
}

func newMultiUserClientConfig(server, mode, key string) (*transport.ProtocolConfig, error) {
	clientTables, err := transport.NewClientTablesWithCustomPatterns(transport.ClientAEADSeed(key), "prefer_entropy", "", nil)
	if err != nil {
		return nil, err
	}
	disableHTTPMask := mode == "raw"
	if disableHTTPMask {
		mode = "legacy"
	}
	return &transport.ProtocolConfig{
		ServerAddress:           server,
		Key:                     key,
		AEADMethod:              "chacha20-poly1305",
		Tables:                  clientTables,
		PaddingMin:              0,
		PaddingMax:              0,
		EnablePureDownlink:      true,
		DisableHTTPMask:         disableHTTPMask,
		HTTPMaskMode:            mode,
		HandshakeTimeoutSeconds: 5,
	}, nil
}

func assertSudokuTCPEcho(t *testing.T, conn net.Conn, target, payloadText string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	encodedTarget, err := transport.EncodeAddress(target)
	if err != nil {
		t.Fatalf("encode echo target: %v", err)
	}
	if err := transport.WriteKIPMessage(conn, transport.KIPTypeOpenTCP, encodedTarget); err != nil {
		t.Fatalf("open TCP session: %v", err)
	}
	payload := []byte(payloadText)
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write echo payload: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo payload: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo payload = %q, want %q", got, payload)
	}
}

func startSudokuTestEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start echo server: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("echo server did not stop")
		}
	})
	return listener.Addr().String()
}
