package singbox

import (
	"encoding/base64"
	"encoding/pem"
	"net"
	"strings"
	"testing"

	panel "github.com/limo13660/daonode/api/v2board"
)

func TestNormalizeECHPEMContract(t *testing.T) {
	raw := []byte{0, 32, 1, 2, 3, 4, 5, 6}
	encoded := base64.StdEncoding.EncodeToString(raw)

	normalized, err := normalizeECHPEM(encoded, "ECH KEYS")
	if err != nil {
		t.Fatalf("normalizeECHPEM(base64) error = %v", err)
	}
	block, rest := pem.Decode([]byte(normalized))
	if block == nil || block.Type != "ECH KEYS" {
		t.Fatalf("normalizeECHPEM(base64) returned invalid PEM: %q", normalized)
	}
	if string(block.Bytes) != string(raw) || strings.TrimSpace(string(rest)) != "" {
		t.Fatalf("normalizeECHPEM(base64) changed the ECH key payload")
	}

	preserved, err := normalizeECHPEM(normalized, "ECH KEYS")
	if err != nil {
		t.Fatalf("normalizeECHPEM(PEM) error = %v", err)
	}
	preservedBlock, preservedRest := pem.Decode([]byte(preserved))
	if preservedBlock == nil || preservedBlock.Type != "ECH KEYS" ||
		string(preservedBlock.Bytes) != string(raw) || strings.TrimSpace(string(preservedRest)) != "" {
		t.Fatalf("normalizeECHPEM(PEM) changed the ECH key payload")
	}

	if _, err := normalizeECHPEM("not-base64", "ECH KEYS"); err == nil {
		t.Fatal("normalizeECHPEM() accepted invalid ECH data")
	}
	wrongType := string(pem.EncodeToMemory(&pem.Block{Type: "ECH CONFIGS", Bytes: raw}))
	if _, err := normalizeECHPEM(wrongType, "ECH KEYS"); err == nil {
		t.Fatal("normalizeECHPEM() accepted the wrong ECH PEM block type")
	}
}

func TestPureUserDeletionDoesNotRestartNaiveRuntime(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve test port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release test port: %v", err)
	}

	info := &panel.NodeInfo{
		Id:     1,
		Type:   "naive",
		Kernel: "singbox",
		Common: &panel.CommonNode{
			Protocol:          "naive",
			Kernel:            "singbox",
			ListenIP:          "127.0.0.1",
			ServerPort:        port,
			TransportProtocol: "TCP",
			PanelIdentifier:   "test-panel",
			Tls:               panel.Tls,
			CertInfo:          &panel.CertInfo{CertMode: "none"},
		},
	}
	current := NewRuntime("naive-user-delete-test", info).(*runtime)
	users := []panel.UserInfo{
		{Id: 1, Uuid: "password-1"},
		{Id: 2, Uuid: "password-2"},
	}
	if _, err := current.AddUsers(users); err != nil {
		t.Fatalf("start Naive runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := current.Stop(); err != nil {
			t.Errorf("stop Naive runtime: %v", err)
		}
	})
	instance := current.instance
	if instance == nil {
		t.Fatal("Naive runtime did not start")
	}

	if err := current.SyncUsers([]panel.UserInfo{users[1]}, nil); err != nil {
		t.Fatalf("delete Naive user: %v", err)
	}
	if current.instance != instance {
		t.Fatal("pure user deletion restarted the Naive runtime")
	}
	if _, _, exists := current.userForName(current.userName(users[1].Id)); exists {
		t.Fatal("deleted user remained accepted by the policy tracker")
	}
	if _, _, exists := current.userForName(current.userName(users[0].Id)); !exists {
		t.Fatal("remaining user disappeared from the policy tracker")
	}
}
