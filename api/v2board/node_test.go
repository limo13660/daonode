package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/limo13660/daonode/conf"
)

func TestGetNodeInfoNaiveECHContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("node_type"); got != "daonode" {
			t.Errorf("node_type = %q, want daonode", got)
		}
		if got := r.URL.Query().Get("node_id"); got != "7" {
			t.Errorf("node_id = %q, want 7", got)
		}
		if got := r.URL.Query().Get("token"); got != "panel-secret" {
			t.Errorf("token = %q, want panel-secret", got)
		}
		if r.Header.Get("If-None-Match") == `"contract-etag"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"contract-etag"`)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"protocol":           "naive",
			"kernel":             "singbox",
			"listen_ip":          "::",
			"server_port":        8443,
			"speed_limit":        100,
			"transport_protocol": "UDP",
			"tls":                1,
			"tls_settings": map[string]any{
				"server_name":        "edge.naive.example",
				"server_names":       []string{"alt.naive.example"},
				"cert_mode":          "self",
				"reject_unknown_sni": "1",
				"ech":                "custom",
				"ech_server_name":    "public.example",
				"ech_key":            "AQID",
				"ech_config":         "BAUG",
			},
			"protocol_settings": map[string]any{
				"quic_congestion_control": "bbr2",
				"udp_over_tcp":            true,
			},
			"base_config": map[string]any{
				"push_interval": 30,
				"pull_interval": 45,
			},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
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

	info, err := client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatalf("GetNodeInfo() error = %v", err)
	}
	if info.Type != "naive" || info.Kernel != "singbox" {
		t.Fatalf("node selection = %s/%s, want naive/singbox", info.Type, info.Kernel)
	}
	if info.Security != Tls || info.Common.TransportProtocol != "UDP" {
		t.Fatalf("Naive transport contract was not preserved: %#v", info.Common)
	}
	if info.Common.ProtocolSettings.QUICCongestionControl != "bbr2" ||
		!info.Common.ProtocolSettings.UDPOverTCP {
		t.Fatalf("Naive protocol settings = %#v", info.Common.ProtocolSettings)
	}
	if info.Common.TlsSettings.ECH != "custom" ||
		info.Common.TlsSettings.ECHServerName != "public.example" ||
		info.Common.TlsSettings.ECHKey != "AQID" ||
		info.Common.TlsSettings.ECHConfig != "BAUG" {
		t.Fatalf("Naive ECH settings = %#v", info.Common.TlsSettings)
	}
	if info.Common.CertInfo == nil || info.Common.CertInfo.CertMode != "self" ||
		!info.Common.CertInfo.RejectUnknownSni {
		t.Fatalf("Naive certificate settings = %#v", info.Common.CertInfo)
	}
	if info.PushInterval != 30*time.Second || info.PullInterval != 45*time.Second {
		t.Fatalf("panel intervals = %s/%s", info.PushInterval, info.PullInterval)
	}

	unchanged, err := client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatalf("GetNodeInfo() with matching ETag error = %v", err)
	}
	if unchanged != nil {
		t.Fatalf("GetNodeInfo() with matching ETag = %#v, want nil", unchanged)
	}
}
