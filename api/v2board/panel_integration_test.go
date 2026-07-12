package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
				"protocol": "mieru", "server_port": 32000,
				"transport_protocol": "TCP", "mtu": 1400,
				"username_prefix": "dpaneln9",
				"base_config":     map[string]any{"push_interval": 30, "pull_interval": 60},
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
