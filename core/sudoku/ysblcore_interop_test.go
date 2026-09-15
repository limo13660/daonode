//go:build interop

package sudoku

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/core/shared"
	"github.com/limo13660/daonode/limiter"
	client "github.com/metacubex/mihomo/adapter/outbound"
	constant "github.com/metacubex/mihomo/constant"
)

func TestYSBLCoreClientInterop(t *testing.T) {
	const validKey = "959df3cf-197d-4b6d-be9f-1b4ec3ad4e9f"
	users := map[int]panel.UserInfo{
		1: {Id: 1, Uuid: "4b9edc89-cf4c-4210-bd8e-e8db8a3731ad"},
		2: {Id: 2, Uuid: validKey},
		3: {Id: 3, Uuid: "50793b13-733c-49b9-aa4b-5f7006bc22c4"},
	}

	// Legacy HTTPMask is the panel default and must remain compatible with
	// clients that send the historical fake HTTP/1.1 request before the
	// encrypted Sudoku handshake. Keep it in the same cross-repository test
	// matrix as the newer tunnel modes.
	for _, mode := range []string{"raw", "legacy", "stream", "poll", "ws"} {
		t.Run("tcp/"+mode, func(t *testing.T) {
			target := startInteropEchoServer(t)
			server := startInteropSudokuServer(t, mode, users)

			badClient := newInteropSudokuClient(t, server, mode, "wrong-user-key", "off")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := badClient.DialContext(ctx, interopMetadata(t, target, constant.TCP))
			cancel()
			_ = badClient.Close()
			if err == nil {
				t.Fatal("client with an unknown key was accepted")
			}

			goodClient := newInteropSudokuClient(t, server, mode, validKey, "off")
			defer goodClient.Close()
			ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := goodClient.DialContext(ctx, interopMetadata(t, target, constant.TCP))
			if err != nil {
				t.Fatalf("dial through daonode: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

			payload := []byte("ysblcore-daonode-sudoku-" + mode)
			if _, err := conn.Write(payload); err != nil {
				t.Fatalf("write payload: %v", err)
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("read payload: %v", err)
			}
			if string(got) != string(payload) {
				t.Fatalf("payload = %q, want %q", got, payload)
			}
		})
	}

	for _, mode := range []string{"raw", "legacy", "stream", "poll", "ws"} {
		t.Run("uot/"+mode, func(t *testing.T) {
			target := startInteropUDPEchoServer(t)
			server := startInteropSudokuServer(t, mode, users)
			goodClient := newInteropSudokuClient(t, server, mode, validKey, "off")
			defer goodClient.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			packetConn, err := goodClient.ListenPacketContext(ctx, interopMetadata(t, target, constant.UDP))
			if err != nil {
				t.Fatalf("open UoT through daonode: %v", err)
			}
			defer packetConn.Close()
			_ = packetConn.SetDeadline(time.Now().Add(10 * time.Second))

			targetAddr, err := net.ResolveUDPAddr("udp", target)
			if err != nil {
				t.Fatalf("resolve UDP echo target: %v", err)
			}
			payloads := [][]byte{
				[]byte("ysblcore-daonode-sudoku-uot-" + mode + "-1"),
				[]byte("ysblcore-daonode-sudoku-uot-" + mode + "-2"),
				[]byte("ysblcore-daonode-sudoku-uot-" + mode + "-3"),
			}
			// Queue several datagrams before reading responses.  This exercises
			// the server's per-destination UDP session instead of the old
			// one-datagram socket lifecycle.
			for _, payload := range payloads {
				if _, err := packetConn.WriteTo(payload, targetAddr); err != nil {
					t.Fatalf("write UoT payload: %v", err)
				}
			}
			for range payloads {
				got := make([]byte, 128)
				n, _, err := packetConn.ReadFrom(got)
				if err != nil {
					t.Fatalf("read UoT payload: %v", err)
				}
				matched := false
				for _, payload := range payloads {
					if string(got[:n]) == string(payload) {
						matched = true
						break
					}
				}
				if !matched {
					t.Fatalf("unexpected UoT payload = %q", got[:n])
				}
			}
		})
	}

	for _, mode := range []string{"raw", "stream"} {
		t.Run("multiplex/"+mode, func(t *testing.T) {
			target := startInteropEchoServer(t)
			server := startInteropSudokuServer(t, mode, users)
			goodClient := newInteropSudokuClient(t, server, mode, validKey, "on")
			defer goodClient.Close()

			for stream := 1; stream <= 2; stream++ {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				conn, err := goodClient.DialContext(ctx, interopMetadata(t, target, constant.TCP))
				cancel()
				if err != nil {
					t.Fatalf("dial multiplex stream %d through daonode: %v", stream, err)
				}
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				payload := []byte(fmt.Sprintf("ysblcore-daonode-sudoku-mux-%s-%d", mode, stream))
				if _, err := conn.Write(payload); err != nil {
					_ = conn.Close()
					t.Fatalf("write multiplex stream %d: %v", stream, err)
				}
				got := make([]byte, len(payload))
				if _, err := io.ReadFull(conn, got); err != nil {
					_ = conn.Close()
					t.Fatalf("read multiplex stream %d: %v", stream, err)
				}
				_ = conn.Close()
				if string(got) != string(payload) {
					t.Fatalf("multiplex stream %d payload = %q, want %q", stream, got, payload)
				}
			}
		})
	}
}

func startInteropSudokuServer(t *testing.T, mode string, users map[int]panel.UserInfo) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve Sudoku port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	info := sudokuNodeInfo()
	info.Common.ServerPort = port
	info.Common.ProtocolSettings.PaddingMin = 0
	info.Common.ProtocolSettings.PaddingMax = 0
	info.Common.ProtocolSettings.EnablePureDownlink = true
	if mode != "raw" {
		info.Common.ProtocolSettings.HTTPMask = true
		info.Common.ProtocolSettings.HTTPMaskMode = mode
	}

	configs, err := buildUserConfigs(info, users)
	if err != nil {
		t.Fatalf("build server users: %v", err)
	}
	userList := make([]panel.UserInfo, 0, len(users))
	for _, user := range users {
		userList = append(userList, user)
	}
	tag := fmt.Sprintf("sudoku-interop-%s-%d", mode, port)
	limiter.Init()
	limiter.AddLimiter("sudoku", tag, 0, userList, nil)
	t.Cleanup(func() { limiter.DeleteLimiter(tag) })
	services := shared.NewRuntimeServices(tag)
	services.SyncUsers(nil, userList)
	instance, err := startServer(info, services, configs)
	if err != nil {
		t.Fatalf("start daonode Sudoku server: %v", err)
	}
	t.Cleanup(func() {
		services.CloseAllConnections()
		if err := instance.Close(); err != nil {
			t.Errorf("close daonode Sudoku server: %v", err)
		}
	})
	return instance.listener.Addr().String()
}

func newInteropSudokuClient(t *testing.T, server, mode, key, multiplex string) *client.Sudoku {
	t.Helper()
	host, portText, err := net.SplitHostPort(server)
	if err != nil {
		t.Fatalf("split Sudoku address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse Sudoku port: %v", err)
	}
	httpMask := mode != "raw"
	paddingMin, paddingMax := 0, 0
	enablePureDownlink := true
	option := client.SudokuOption{
		Name:               "interop-" + mode,
		Server:             host,
		Port:               port,
		Key:                key,
		AEADMethod:         "chacha20-poly1305",
		PaddingMin:         &paddingMin,
		PaddingMax:         &paddingMax,
		TableType:          "prefer_entropy",
		EnablePureDownlink: &enablePureDownlink,
		HTTPMask:           &httpMask,
		Multiplex:          multiplex,
	}
	if httpMask {
		option.HTTPMaskMode = mode
	}
	outbound, err := client.NewSudoku(option)
	if err != nil {
		t.Fatalf("create YSBLCore Sudoku client: %v", err)
	}
	return outbound
}

func interopMetadata(t *testing.T, target string, network constant.NetWork) *constant.Metadata {
	t.Helper()
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatalf("split target address: %v", err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("parse target port: %v", err)
	}
	return &constant.Metadata{
		NetWork: network,
		DstIP:   netip.MustParseAddr(host),
		DstPort: uint16(port),
	}
}

func startInteropEchoServer(t *testing.T) string {
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
		case <-time.After(5 * time.Second):
			t.Error("echo server did not stop")
		}
	})
	return listener.Addr().String()
}

func startInteropUDPEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("start UDP echo server: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64*1024)
		for {
			n, addr, err := listener.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if _, err := listener.WriteToUDP(buf[:n], addr); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("UDP echo server did not stop")
		}
	})
	return listener.LocalAddr().String()
}
