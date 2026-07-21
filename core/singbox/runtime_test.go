package singbox

import (
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
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
