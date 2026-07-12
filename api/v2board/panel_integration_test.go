package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/limo13660/daonode/conf"
)

func TestDaoBoardPanelFlow(t *testing.T) {
	var trafficRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("node_type") != "daonode" || r.URL.Query().Get("node_id") != "9" || r.URL.Query().Get("token") != "secret" {
			t.Errorf("unexpected panel query: %s", r.URL.RawQuery)
		}
		switch r.URL.Path {
		case "/api/v2/server/config":
			if r.Header.Get("If-None-Match") == `"config-etag"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"config-etag"`)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"protocol": " MIERU ", "server_port": 32000,
				"transport_protocol": "TCP", "mtu": 1400,
				"port_bindings":   []map[string]any{{"port": "32001", "server_port": "32001", "protocol": "UDP"}},
				"traffic_pattern": "CLrv1dcD", "user_hint_is_mandatory": true,
				"username_prefix": "dpaneln9",
				"routes": []map[string]any{{
					"id": 5, "match": []string{"10.0.0.0/8"}, "action": "block_ip", "action_value": nil,
				}},
				"base_config": map[string]any{"push_interval": 30, "pull_interval": 60},
			})
		case "/api/v1/server/UniProxy/user":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"users": []map[string]any{{
				"id": 12, "uuid": "password", "speed_limit": 100, "device_limit": 2,
			}}})
		case "/api/v1/server/UniProxy/alivelist":
			_ = json.NewEncoder(w).Encode(map[string]any{"alive": map[string]int{"12": 1}})
		case "/api/v1/server/UniProxy/push":
			if trafficRequests.Add(1) == 1 {
				http.Error(w, "temporary failure", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"data": true})
		case "/api/v1/server/UniProxy/alive":
			_ = json.NewEncoder(w).Encode(map[string]bool{"data": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{
		APIHost: server.URL, NodeID: 9, Key: "secret", Timeout: 5, RetryCount: &retryCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	node, err := client.GetNodeInfo(ctx)
	if err != nil || node == nil || node.Type != "mieru" || node.Common.ServerPort != 32000 {
		t.Fatalf("GetNodeInfo() = %+v, %v", node, err)
	}
	if node.Common.Protocol != "mieru" || !strings.Contains(node.Tag, "-mieru:9") {
		t.Fatalf("GetNodeInfo() protocol routing = %+v", node)
	}
	if len(node.Common.Routes) != 1 || node.Common.Routes[0].Action != "block_ip" {
		t.Fatalf("GetNodeInfo() routes = %+v", node.Common.Routes)
	}
	if len(node.Common.PortBindings) != 1 || node.Common.PortBindings[0].Protocol != "UDP" || !node.Common.UserHintIsMandatory {
		t.Fatalf("GetNodeInfo() Mieru options = %+v", node.Common)
	}
	if node.Common.TrafficPattern != "CLrv1dcD" {
		t.Fatalf("GetNodeInfo() traffic pattern = %q", node.Common.TrafficPattern)
	}
	if unchanged, err := client.GetNodeInfo(ctx); err != nil || unchanged != nil {
		t.Fatalf("second GetNodeInfo() = %+v, %v", unchanged, err)
	}
	users, err := client.GetUserList(ctx)
	if err != nil || len(users) != 1 || users[0].Id != 12 {
		t.Fatalf("GetUserList() = %+v, %v", users, err)
	}
	alive, err := client.GetUserAlive(ctx)
	if err != nil || alive[12] != 1 {
		t.Fatalf("GetUserAlive() = %+v, %v", alive, err)
	}
	traffic := []UserTraffic{{UID: 12, Upload: 100, Download: 200}}
	if err := client.ReportUserTraffic(ctx, traffic); err == nil {
		t.Fatal("ReportUserTraffic() accepted HTTP 500")
	}
	if err := client.ReportUserTraffic(ctx, traffic); err != nil {
		t.Fatalf("ReportUserTraffic() retry error = %v", err)
	}
	online := map[int][]string{12: {"127.0.0.1"}}
	if err := client.ReportNodeOnlineUsers(ctx, &online); err != nil {
		t.Fatalf("ReportNodeOnlineUsers() error = %v", err)
	}
}

func TestDaoBoardRejectsUnsupportedProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"protocol": "future", "server_port": 32000,
		})
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{
		APIHost: server.URL, NodeID: 9, Key: "secret", Timeout: 5, RetryCount: &retryCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetNodeInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported protocol: future") {
		t.Fatalf("GetNodeInfo() error = %v", err)
	}
}

func TestPanelRejectsFailedUserResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"users":[]}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{
		APIHost: server.URL, NodeID: 9, Key: "secret", Timeout: 5, RetryCount: &retryCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetUserList(context.Background()); err == nil {
		t.Fatal("GetUserList() accepted HTTP 500")
	}
	if _, err := client.GetUserAlive(context.Background()); err == nil {
		t.Fatal("GetUserAlive() accepted HTTP 500")
	}
}
