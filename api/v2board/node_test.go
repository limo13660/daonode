package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
			"kernel":             "naive",
			"panel_identifier":   "ysbl-panel",
			"username_prefix":    "ysbl-panel",
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
	if info.Type != "naive" || info.Kernel != "naive" {
		t.Fatalf("node selection = %s/%s, want naive/naive", info.Type, info.Kernel)
	}
	if info.Security != Tls || info.Common.TransportProtocol != "UDP" {
		t.Fatalf("Naive transport contract was not preserved: %#v", info.Common)
	}
	if info.Common.PanelIdentifier != "ysbl-panel" || info.Common.UserNamePrefix != "ysbl-panel" {
		t.Fatalf("panel identifier contract = %#v", info.Common)
	}
	if info.Common.ProtocolSettings.QUICCongestionControl != "bbr2" ||
		!info.Common.ProtocolSettings.UDPOverTCP {
		t.Fatalf("Naive protocol settings = %#v", info.Common.ProtocolSettings)
	}
	if settings := info.Common.ProtocolSettings; settings.AEADMethod != "" || settings.PaddingMin != 0 ||
		settings.PaddingMax != 0 || settings.TableType != "" || settings.EnablePureDownlink ||
		settings.HTTPMask || settings.HTTPMaskMode != "" || settings.Multiplex != "" ||
		len(settings.CustomTables) != 0 {
		t.Fatalf("Naive protocol settings contain Sudoku defaults: %#v", settings)
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

func TestGetNodeInfoJuicityContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"protocol":           "juicity",
			"kernel":             "juicity",
			"listen_ip":          "0.0.0.0",
			"server_port":        443,
			"transport_protocol": "UDP",
			"tls":                1,
			"tls_settings": map[string]any{
				"server_name": "edge.juicity.example",
				"cert_mode":   "self",
			},
			"protocol_settings": map[string]any{
				"quic_congestion_control": "CUBIC",
			},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 9, Key: "secret", RetryCount: &retryCount})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	info, err := client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatalf("GetNodeInfo() error = %v", err)
	}
	if info.Type != "juicity" || info.Kernel != "juicity" || info.Common.TransportProtocol != "UDP" {
		t.Fatalf("Juicity selection = %#v", info)
	}
	if info.Common.ProtocolSettings.QUICCongestionControl != "cubic" {
		t.Fatalf("Juicity congestion = %q, want cubic", info.Common.ProtocolSettings.QUICCongestionControl)
	}
	if info.Common.CertInfo == nil || info.Common.CertInfo.CertMode != "self" {
		t.Fatalf("Juicity certificate = %#v", info.Common.CertInfo)
	}
}

func TestGetNodeInfoSudokuContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"protocol":           "sudoku",
			"kernel":             "sudoku",
			"listen_ip":          "0.0.0.0",
			"server_port":        2087,
			"transport_protocol": "TCP",
			"tls":                0,
			"protocol_settings": map[string]any{
				"aead_method":          "chacha20-poly1305",
				"padding_min":          0,
				"padding_max":          0,
				"table_type":           "up_ascii_down_entropy",
				"enable_pure_downlink": true,
				"http_mask":            true,
				"http_mask_mode":       "ws",
				"http_mask_tls":        true,
				"http_mask_host":       "cdn.example.com",
				"path_root":            "sudoku",
				"multiplex":            "on",
				"custom_table":         "xxppvvvv",
				"custom_tables":        []string{"xxppvvvv", "xpxpvvvv"},
			},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 11, Key: "secret", RetryCount: &retryCount})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	info, err := client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatalf("GetNodeInfo() error = %v", err)
	}
	if info.Type != "sudoku" || info.Kernel != "sudoku" || info.Security != None || info.Common.TransportProtocol != "TCP" {
		t.Fatalf("Sudoku selection = %#v", info)
	}
	settings := info.Common.ProtocolSettings
	if settings.AEADMethod != "chacha20-poly1305" || settings.PaddingMin != 0 || settings.PaddingMax != 0 ||
		settings.TableType != "up_ascii_down_entropy" || !settings.EnablePureDownlink ||
		!settings.HTTPMask || settings.HTTPMaskMode != "ws" || !settings.HTTPMaskTLS ||
		settings.HTTPMaskHost != "cdn.example.com" || settings.PathRoot != "sudoku" ||
		settings.Multiplex != "on" || settings.CustomTable != "xxppvvvv" || len(settings.CustomTables) != 2 {
		t.Fatalf("Sudoku protocol settings = %#v", settings)
	}
}

func TestGetNodeInfoSudokuRejectsUDP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol":"sudoku","kernel":"sudoku","listen_ip":"0.0.0.0","server_port":2087,"transport_protocol":"UDP"}`))
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 12, Key: "secret", RetryCount: &retryCount})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := client.GetNodeInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "must be TCP") {
		t.Fatalf("GetNodeInfo() error = %v, want TCP validation error", err)
	}
}

func TestGetNodeInfoSudokuAcceptsLegacyAliasesAndDefaults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"protocol":"SUDOKU",
			"kernel":"SUDOKU",
			"listen_ip":"0.0.0.0",
			"server_port":2087,
			"protocol_settings":{
				"aead":"CHACHA20-POLY1305",
				"ascii":"prefer_numeric",
				"httpmask":{"disable":false,"mode":"split-stream","tls":true,"host":"cdn.example.com","path":"/sudoku/","multiplex":"high"},
				"custom_tables":"[\"xxppvvvv\",\"xpxpvvvv\"]"
			}
		}`))
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 13, Key: "secret", RetryCount: &retryCount})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	info, err := client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatalf("GetNodeInfo() error = %v", err)
	}
	settings := info.Common.ProtocolSettings
	if info.Type != "sudoku" || info.Kernel != "sudoku" || settings.AEADMethod != "chacha20-poly1305" ||
		settings.TableType != "prefer_ascii" || !settings.HTTPMask || settings.HTTPMaskMode != "stream" ||
		!settings.HTTPMaskTLS || settings.HTTPMaskHost != "cdn.example.com" || settings.PathRoot != "sudoku" ||
		settings.Multiplex != "on" || len(settings.CustomTables) != 2 {
		t.Fatalf("legacy Sudoku settings = %#v", settings)
	}
}

func TestGetNodeInfoSudokuUsesOfficialDefaultsWhenSettingsAreMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol":"sudoku","kernel":"sudoku","listen_ip":"0.0.0.0","server_port":2087}`))
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 14, Key: "secret", RetryCount: &retryCount})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	info, err := client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatalf("GetNodeInfo() error = %v", err)
	}
	settings := info.Common.ProtocolSettings
	if settings.AEADMethod != "chacha20-poly1305" || settings.PaddingMin != 10 || settings.PaddingMax != 30 ||
		settings.TableType != "prefer_ascii" || !settings.EnablePureDownlink || !settings.HTTPMask ||
		settings.HTTPMaskMode != "legacy" || settings.HTTPMaskTLS || settings.Multiplex != "off" {
		t.Fatalf("Sudoku defaults = %#v", settings)
	}
}

func TestGetNodeInfoSudokuDefaultsPreserveExplicitZeroAndFalse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol":"sudoku","kernel":"sudoku","listen_ip":"0.0.0.0","server_port":2087,"protocol_settings":{"padding_min":0,"padding_max":0,"enable_pure_downlink":false,"http_mask":false}}`))
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 17, Key: "secret", RetryCount: &retryCount})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	info, err := client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatalf("GetNodeInfo() error = %v", err)
	}
	settings := info.Common.ProtocolSettings
	if settings.PaddingMin != 0 || settings.PaddingMax != 0 || settings.EnablePureDownlink || settings.HTTPMask {
		t.Fatalf("explicit Sudoku zero/false settings were replaced: %#v", settings)
	}
	if settings.AEADMethod != "chacha20-poly1305" || settings.TableType != "prefer_ascii" ||
		settings.HTTPMaskMode != "legacy" || settings.Multiplex != "off" {
		t.Fatalf("missing Sudoku settings did not receive defaults: %#v", settings)
	}
}

func TestGetNodeInfoSudokuRejectsUnsupportedSettings(t *testing.T) {
	tests := []struct {
		name    string
		setting string
		value   string
		want    string
	}{
		{name: "aead", setting: "aead_method", value: "aes-256-gcm", want: "aead_method"},
		{name: "table", setting: "table_type", value: "random", want: "table_type"},
		{name: "http mask mode", setting: "http_mask_mode", value: "cdn", want: "http_mask_mode"},
		{name: "multiplex", setting: "multiplex", value: "always", want: "multiplex"},
		{name: "path root", setting: "path_root", value: "nested/path", want: "path_root"},
		{name: "custom table", setting: "custom_table", value: "invalid", want: "custom table"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				body := fmt.Sprintf(`{"protocol":"sudoku","kernel":"sudoku","listen_ip":"0.0.0.0","server_port":2087,"protocol_settings":{"%s":%q}}`, test.setting, test.value)
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			retryCount := 0
			client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 15, Key: "secret", RetryCount: &retryCount})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if _, err := client.GetNodeInfo(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("GetNodeInfo() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestGetNodeInfoSudokuTreatsEmptyLegacyFieldsAsDefaults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol":"sudoku","kernel":"sudoku","listen_ip":"0.0.0.0","server_port":2087,"protocol_settings":{"aead_method":"","padding_min":"","padding_max":"","table_type":"","http_mask_mode":"","multiplex":""}}`))
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{APIHost: server.URL, NodeID: 16, Key: "secret", RetryCount: &retryCount})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	info, err := client.GetNodeInfo(context.Background())
	if err != nil {
		t.Fatalf("GetNodeInfo() error = %v", err)
	}
	settings := info.Common.ProtocolSettings
	if settings.AEADMethod != "chacha20-poly1305" || settings.PaddingMin != 10 || settings.PaddingMax != 30 ||
		settings.TableType != "prefer_ascii" || settings.HTTPMaskMode != "legacy" || settings.Multiplex != "off" {
		t.Fatalf("empty Sudoku fields = %#v", settings)
	}
}
