package node

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSelfSignedCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "node.cer")
	keyPath := filepath.Join(dir, "node.key")
	if err := generateSelfSignedCertificate("node.example", certPath, keyPath); err != nil {
		t.Fatalf("generateSelfSignedCertificate() error = %v", err)
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("certificate PEM is invalid")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("node.example"); err != nil {
		t.Fatalf("certificate hostname verification failed: %v", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		t.Fatalf("private key PEM type = %v", keyBlock)
	}
	if _, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
		t.Fatalf("private key is invalid: %v", err)
	}
}
