package node

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mieruclient "github.com/enfein/mieru/v3/apis/client"
	"github.com/enfein/mieru/v3/apis/model"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	"google.golang.org/protobuf/proto"

	"github.com/limo13660/daonode/conf"
	"github.com/limo13660/daonode/core"
	"github.com/limo13660/daonode/limiter"
)

func TestDaoBoardToMieruEndToEnd(t *testing.T) {
	echoListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoListener.Close()
	go func() {
		conn, acceptErr := echoListener.Accept()
		if acceptErr == nil {
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
		}
	}()

	proxyPort := allocateTCPPort(t)
	panelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("node_type") != "daonode" {
			t.Errorf("node_type = %q", r.URL.Query().Get("node_type"))
		}
		switch r.URL.Path {
		case "/api/v2/server/config":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"protocol": "mieru", "server_port": proxyPort,
				"transport_protocol": "TCP", "mtu": 1400,
				"username_prefix": "de2en5",
				"base_config":     map[string]any{"push_interval": 3600, "pull_interval": 3600},
			})
		case "/api/v1/server/UniProxy/user":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"users": []map[string]any{{
				"id": 42, "uuid": "user-password", "speed_limit": 0, "device_limit": 0,
			}}})
		case "/api/v1/server/UniProxy/alivelist":
			_ = json.NewEncoder(w).Encode(map[string]any{"alive": map[string]int{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer panelServer.Close()

	retryCount := 0
	nodeConfig := conf.NodeConfig{
		APIHost: panelServer.URL, NodeID: 5, Key: "secret", Timeout: 5, RetryCount: &retryCount,
	}
	config := conf.New()
	config.NodeConfigs = []conf.NodeConfig{nodeConfig}
	limiter.Init()
	nodes, err := New(config.NodeConfigs)
	if err != nil {
		t.Fatalf("load panel nodes: %v", err)
	}
	runtimeCore := core.New(config)
	runtimeCore.ReloadCh = make(chan struct{}, 1)
	if err := runtimeCore.Start(nodes.NodeInfos); err != nil {
		t.Fatal(err)
	}
	defer runtimeCore.Close()
	if err := nodes.Start(config.NodeConfigs, runtimeCore); err != nil {
		t.Fatalf("start nodes: %v", err)
	}
	defer nodes.Close()

	transport := appctlpb.TransportProtocol_TCP
	client := mieruclient.NewClient()
	if err := client.Store(&mieruclient.ClientConfig{Profile: &appctlpb.ClientProfile{
		ProfileName: proto.String("e2e"),
		User:        &appctlpb.User{Name: proto.String("de2en5u42"), Password: proto.String("user-password")},
		Servers: []*appctlpb.ServerEndpoint{{
			IpAddress:    proto.String("127.0.0.1"),
			PortBindings: []*appctlpb.PortBinding{{Port: proto.Int32(int32(proxyPort)), Protocol: &transport}},
		}},
		Mtu: proto.Int32(1400),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Stop()

	target := echoListener.Addr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := client.DialContext(ctx, model.NetAddrSpec{
		AddrSpec: model.AddrSpec{IP: target.IP, Port: target.Port}, Net: "tcp",
	})
	if err != nil {
		t.Fatalf("dial through complete node flow: %v", err)
	}
	defer conn.Close()
	payload := []byte("daoboard-daonode-e2e")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, received); err != nil {
		t.Fatal(err)
	}
	if string(received) != string(payload) {
		t.Fatalf("received %q, want %q", received, payload)
	}
}

func allocateTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}
