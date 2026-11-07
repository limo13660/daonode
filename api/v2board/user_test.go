package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/limo13660/daonode/conf"
)

func TestReportUserTrafficEmptyAndExplicitClearHaveDifferentSemantics(t *testing.T) {
	type requestSnapshot struct {
		clearHeader string
		body        map[string][]int64
	}
	requests := make(chan requestSnapshot, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		var data map[string][]int64
		if err := json.Unmarshal(body, &data); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		requests <- requestSnapshot{
			clearHeader: r.Header.Get("X-Daonode-Clear-Online"),
			body:        data,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":true}`))
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{
		APIHost:    server.URL,
		NodeID:     7,
		Key:        "panel-secret",
		RetryCount: &retryCount,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := client.ReportUserTraffic(context.Background(), nil); err != nil {
		t.Fatalf("ReportUserTraffic() error = %v", err)
	}
	if err := client.ClearPanelOnlineUserCount(context.Background()); err != nil {
		t.Fatalf("ClearPanelOnlineUserCount() error = %v", err)
	}

	first := <-requests
	second := <-requests
	if first.clearHeader != "" || len(first.body) != 0 {
		t.Fatalf("ordinary empty report = %#v, want no clear header and empty body", first)
	}
	if second.clearHeader != "1" || len(second.body) != 0 {
		t.Fatalf("explicit clear report = %#v, want clear header and empty body", second)
	}
}
