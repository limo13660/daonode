package panel

import (
	"path/filepath"
	"testing"
	"time"
)

func TestIntervalToTime(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  time.Duration
	}{
		{name: "integer", value: 30, want: 30 * time.Second},
		{name: "float", value: float64(45), want: 45 * time.Second},
		{name: "string", value: "60", want: time.Minute},
		{name: "default", value: nil, want: time.Minute},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := intervalToTime(test.value)
			if err != nil {
				t.Fatalf("intervalToTime() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("intervalToTime() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestBuildCertInfoUsesDaoNodePaths(t *testing.T) {
	info := buildCertInfo(7, "future", TlsSettings{
		ServerName: "node.example",
		CertMode:   "dns",
		Provider:   "cloudflare",
		DNSEnv:     "CF_DNS_API_TOKEN=secret,ZONE=example",
	})
	if info.CertFile != filepath.Join("/etc/daonode", "future7.cer") {
		t.Fatalf("CertFile = %q", info.CertFile)
	}
	if info.KeyFile != filepath.Join("/etc/daonode", "future7.key") {
		t.Fatalf("KeyFile = %q", info.KeyFile)
	}
	if info.CertDomain != "node.example" || info.DNSEnv["CF_DNS_API_TOKEN"] != "secret" {
		t.Fatalf("certificate config = %+v", info)
	}
}
