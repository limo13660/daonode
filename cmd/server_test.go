package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/limo13660/daonode/conf"
	"github.com/limo13660/daonode/limiter"
	"github.com/limo13660/daonode/node"
)

func TestReloadRestoresLastKnownGoodRuntimeWhenCandidateCannotBind(t *testing.T) {
	limiter.Init()
	oldPort := reserveTCPPort(t)
	oldPanel := newNaivePanel(t, oldPort)
	defer oldPanel.Close()

	oldConfig := testConfig(oldPanel.URL)
	oldNodes, err := node.New(oldConfig.NodeConfigs)
	if err != nil {
		t.Fatalf("prepare old nodes: %v", err)
	}
	reloadCh := make(chan struct{}, 1)
	oldCore, err := startPreparedRuntime(oldConfig, oldNodes, reloadCh)
	if err != nil {
		t.Fatalf("start old runtime: %v", err)
	}
	snapshot, err := oldNodes.Snapshot()
	if err != nil {
		t.Fatalf("snapshot old runtime: %v", err)
	}
	state := &serverRuntime{config: oldConfig, nodes: oldNodes, core: oldCore, snapshot: snapshot}
	defer func() { _ = state.closeActive() }()
	assertTCPListening(t, oldPort)

	busyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve candidate port: %v", err)
	}
	defer busyListener.Close()
	candidatePort := busyListener.Addr().(*net.TCPAddr).Port
	candidatePanel := newNaivePanel(t, candidatePort)
	defer candidatePanel.Close()
	configPath := writeTestConfig(t, candidatePanel.URL)

	if err := reload(configPath, state, reloadCh); err == nil {
		t.Fatal("reload() succeeded while the candidate port was occupied")
	}
	if !state.running() {
		t.Fatal("last-known-good runtime was not restored")
	}
	assertTCPListening(t, oldPort)
}

func TestReloadKeepsActiveRuntimeWhenCandidatePanelIsUnavailable(t *testing.T) {
	limiter.Init()
	oldPort := reserveTCPPort(t)
	oldPanel := newNaivePanel(t, oldPort)
	defer oldPanel.Close()

	oldConfig := testConfig(oldPanel.URL)
	oldNodes, err := node.New(oldConfig.NodeConfigs)
	if err != nil {
		t.Fatalf("prepare old nodes: %v", err)
	}
	reloadCh := make(chan struct{}, 1)
	oldCore, err := startPreparedRuntime(oldConfig, oldNodes, reloadCh)
	if err != nil {
		t.Fatalf("start old runtime: %v", err)
	}
	snapshot, err := oldNodes.Snapshot()
	if err != nil {
		t.Fatalf("snapshot old runtime: %v", err)
	}
	state := &serverRuntime{config: oldConfig, nodes: oldNodes, core: oldCore, snapshot: snapshot}
	defer func() { _ = state.closeActive() }()
	oldNodesPointer := state.nodes
	oldCorePointer := state.core

	configPath := writeTestConfig(t, "http://127.0.0.1:1")
	if err := reload(configPath, state, reloadCh); err == nil {
		t.Fatal("reload() succeeded with an unavailable panel")
	}
	if state.nodes != oldNodesPointer || state.core != oldCorePointer {
		t.Fatal("active runtime changed before candidate preparation completed")
	}
	assertTCPListening(t, oldPort)
}

func TestReloadRetryDelayIsBounded(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 0, want: 10 * time.Second},
		{failures: 1, want: 10 * time.Second},
		{failures: 2, want: 20 * time.Second},
		{failures: 5, want: 160 * time.Second},
		{failures: 6, want: 5 * time.Minute},
		{failures: 100, want: 5 * time.Minute},
	}
	for _, test := range tests {
		if got := reloadRetryDelay(test.failures); got != test.want {
			t.Errorf("reloadRetryDelay(%d) = %s, want %s", test.failures, got, test.want)
		}
	}
}

func newNaivePanel(t *testing.T, port int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/server/config":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"protocol":           "naive",
				"kernel":             "singbox",
				"listen_ip":          "127.0.0.1",
				"server_port":        port,
				"transport_protocol": "TCP",
				"tls":                1,
				"tls_settings": map[string]any{
					"server_name": "node.test",
					"cert_mode":   "none",
				},
				"base_config": map[string]any{
					"push_interval": 3600,
					"pull_interval": 3600,
				},
			})
		case "/api/v1/server/UniProxy/user":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"users": []map[string]any{{"id": 1, "uuid": "test-password", "device_limit": 0}},
			})
		case "/api/v1/server/UniProxy/alivelist":
			_ = json.NewEncoder(w).Encode(map[string]any{"alive": map[int]int{}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
}

func testConfig(apiHost string) *conf.Conf {
	retryCount := 0
	return &conf.Conf{
		NodeConfigs: []conf.NodeConfig{{
			APIHost:    apiHost,
			NodeID:     1,
			Key:        "test-key",
			Timeout:    1,
			RetryCount: &retryCount,
		}},
	}
}

func writeTestConfig(t *testing.T, apiHost string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	content := fmt.Sprintf(`{"Nodes":[{"ApiHost":%q,"NodeID":1,"ApiKey":"test-key","Timeout":1,"RetryCount":0}]}`, apiHost)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func reserveTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return port
}

func assertTCPListening(t *testing.T, port int) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatalf("port %d is not listening: %v", port, err)
	}
	_ = connection.Close()
}
