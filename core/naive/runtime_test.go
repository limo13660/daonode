package naive

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"golang.org/x/net/http2"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/limiter"
)

func TestOfficialNaiveHTTP2UsesCommonAccounting(t *testing.T) {
	limiter.Init()
	proxyPort := reserveTCPPort(t)
	info := testNodeInfo(proxyPort)
	user := panel.UserInfo{Id: 17, Uuid: "naive-password", DeviceLimit: 1}
	tag := "naive-" + t.Name()
	currentLimiter := limiter.AddLimiter("naive", tag, 0, []panel.UserInfo{user}, map[int]int{})
	t.Cleanup(func() { limiter.DeleteLimiter(tag) })
	current := NewRuntime(tag, info).(*runtime)
	if _, err := current.AddUsers([]panel.UserInfo{user}); err != nil {
		t.Fatalf("AddUsers() error = %v", err)
	}
	t.Cleanup(func() {
		if err := current.Stop(); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})

	target := startEchoServer(t)
	proxyAddress := net.JoinHostPort("127.0.0.1", fmt.Sprint(proxyPort))
	transport := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, _ string, _ *tls.Config) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, proxyAddress)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	requestBody, clientWriter := io.Pipe()
	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Scheme: "http", Host: target},
		Host:   target,
		Header: http.Header{
			"Proxy-Authorization": []string{"Basic " + basicCredential(info, user)},
		},
		Body: requestBody,
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("open HTTP/2 CONNECT tunnel: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", response.StatusCode)
	}

	payload := []byte("common-runtime-accounting")
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := clientWriter.Write(payload)
		writeDone <- writeErr
	}()
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(response.Body, received); err != nil {
		t.Fatalf("read echoed tunnel payload: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write tunnel payload: %v", err)
	}
	if !reflect.DeepEqual(received, payload) {
		t.Fatalf("echoed payload = %q, want %q", received, payload)
	}

	traffic, err := current.Traffic(0)
	if err != nil {
		t.Fatalf("Traffic() error = %v", err)
	}
	want := []panel.UserTraffic{{UID: user.Id, Upload: int64(len(payload)), Download: int64(len(payload))}}
	if !reflect.DeepEqual(traffic, want) {
		t.Fatalf("Traffic() = %#v, want %#v", traffic, want)
	}
	online, err := currentLimiter.GetOnlineDevice()
	if err != nil || len(*online) != 1 {
		t.Fatalf("online devices = %#v, err = %v", online, err)
	}

	_ = clientWriter.Close()
	_ = response.Body.Close()
}

func TestNaiveUserDeletionHotUpdatesOfficialHandler(t *testing.T) {
	limiter.Init()
	info := testNodeInfo(reserveTCPPort(t))
	users := []panel.UserInfo{
		{Id: 1, Uuid: "password-1"},
		{Id: 2, Uuid: "password-2"},
	}
	tag := "naive-" + t.Name()
	limiter.AddLimiter("naive", tag, 0, users, map[int]int{})
	t.Cleanup(func() { limiter.DeleteLimiter(tag) })
	current := NewRuntime(tag, info).(*runtime)
	if _, err := current.AddUsers(users); err != nil {
		t.Fatalf("AddUsers() error = %v", err)
	}
	t.Cleanup(func() { _ = current.Stop() })
	instance := current.instance

	if err := current.SyncUsers([]panel.UserInfo{users[1]}, nil); err != nil {
		t.Fatalf("SyncUsers() error = %v", err)
	}
	if current.instance != instance {
		t.Fatal("deleting one user restarted the official Naive server")
	}
	snapshot := current.instance.handler.snapshot.Load()
	request := &http.Request{Header: http.Header{
		"Proxy-Authorization": []string{"Basic " + basicCredential(info, users[1])},
	}}
	if _, ok := snapshot.authenticate(request); ok {
		t.Fatal("deleted credential remained in the official handler snapshot")
	}
	request.Header.Set("Proxy-Authorization", "Basic "+basicCredential(info, users[0]))
	if found, ok := snapshot.authenticate(request); !ok || found != users[0] {
		t.Fatalf("remaining credential lookup = %#v, %v", found, ok)
	}
}

func TestCompileOfficialNaiveRoutes(t *testing.T) {
	block := "block"
	policy, acl, err := compileRoutePolicy([]panel.Route{
		{Id: 1, Action: "block", Match: []string{"domain:example.com", "keyword:tracker"}},
		{Id: 2, Action: "block_ip", Match: []string{"203.0.113.0/24"}},
		{Id: 3, Action: "block_port", Match: []string{"25,6881-6889"}},
		{Id: 4, Action: "default_out", ActionValue: &block},
	})
	if err != nil {
		t.Fatalf("compileRoutePolicy() error = %v", err)
	}
	if !policy.blockAll || len(policy.domains) != 2 || len(policy.ports) != 2 || len(acl) != 2 {
		t.Fatalf("compiled policy = %#v, ACL = %#v", policy, acl)
	}
	if _, _, err := compileRoutePolicy([]panel.Route{{Id: 9, Action: "block_port", Match: []string{"invalid"}}}); err == nil {
		t.Fatal("compileRoutePolicy() accepted invalid port")
	}
	if _, _, err := compileRoutePolicy([]panel.Route{{Id: 10, Action: "dns"}}); err == nil {
		t.Fatal("compileRoutePolicy() accepted unsupported custom DNS route")
	}
}

func TestParseECHKeys(t *testing.T) {
	privateKey := []byte{1, 2, 3}
	config := []byte{4, 5, 6, 7}
	raw := make([]byte, 0, 2+len(privateKey)+2+len(config))
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(privateKey)))
	raw = append(raw, length...)
	raw = append(raw, privateKey...)
	binary.BigEndian.PutUint16(length, uint16(len(config)))
	raw = append(raw, length...)
	raw = append(raw, config...)
	value := string(pem.EncodeToMemory(&pem.Block{Type: "ECH KEYS", Bytes: raw}))

	keys, err := parseECHKeys(value)
	if err != nil {
		t.Fatalf("parseECHKeys() error = %v", err)
	}
	if len(keys) != 1 || !reflect.DeepEqual(keys[0].PrivateKey, privateKey) ||
		!reflect.DeepEqual(keys[0].Config, config) || !keys[0].SendAsRetry {
		t.Fatalf("parseECHKeys() = %#v", keys)
	}
	if _, err := parseECHKeys(base64.StdEncoding.EncodeToString([]byte{0, 9})); err == nil {
		t.Fatal("parseECHKeys() accepted truncated data")
	}
}

func testNodeInfo(port int) *panel.NodeInfo {
	return &panel.NodeInfo{
		Id:     1,
		Type:   "naive",
		Kernel: "naive",
		Common: &panel.CommonNode{
			Protocol:          "naive",
			Kernel:            "naive",
			ListenIP:          "127.0.0.1",
			ServerPort:        port,
			TransportProtocol: "TCP",
			PanelIdentifier:   "test-panel",
			Tls:               panel.Tls,
			CertInfo:          &panel.CertInfo{CertMode: "none"},
		},
	}
}

func reserveTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release TCP port: %v", err)
	}
	return port
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for echo server: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
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
	return listener.Addr().String()
}

func basicCredential(info *panel.NodeInfo, user panel.UserInfo) string {
	username := panel.BuildPanelUserName(info.Common.EffectivePanelIdentifier(info.Id), user.Id)
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + user.Uuid))
}
