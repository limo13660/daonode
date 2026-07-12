package node

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/limo13660/daonode/common/file"
)

func (c *Controller) renewCertTask(_ context.Context) error {
	manager, err := NewLego(c.info.Common.CertInfo)
	if err != nil {
		log.WithField("tag", c.tag).Info("new lego error: ", err)
		return nil
	}
	if err := manager.RenewCert(); err != nil {
		log.WithField("tag", c.tag).Info("renew cert error: ", err)
	}
	return nil
}

func (c *Controller) requestCert() error {
	cert := c.info.Common.CertInfo
	if cert == nil {
		return fmt.Errorf("certificate config is missing")
	}
	switch cert.CertMode {
	case "none", "":
		return nil
	case "file":
		if cert.CertFile == "" || cert.KeyFile == "" {
			return fmt.Errorf("cert file path or key file path is empty")
		}
		if !file.IsExist(cert.CertFile) || !file.IsExist(cert.KeyFile) {
			return fmt.Errorf("cert file or key file does not exist")
		}
	case "dns", "http":
		if file.IsExist(cert.CertFile) && file.IsExist(cert.KeyFile) {
			return nil
		}
		manager, err := NewLego(cert)
		if err != nil {
			return fmt.Errorf("create lego object: %w", err)
		}
		if err := manager.CreateCert(); err != nil {
			return fmt.Errorf("create lego cert: %w", err)
		}
	case "self":
		if file.IsExist(cert.CertFile) && file.IsExist(cert.KeyFile) {
			return nil
		}
		if err := generateSelfSignedCertificate(cert.CertDomain, cert.CertFile, cert.KeyFile); err != nil {
			return fmt.Errorf("generate self-signed cert: %w", err)
		}
	default:
		return fmt.Errorf("unsupported cert mode: %s", cert.CertMode)
	}
	return nil
}

func generateSelfSignedCertificate(domain, certPath, keyPath string) error {
	if domain == "" {
		return fmt.Errorf("certificate domain is empty")
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0755); err != nil {
		return err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: domain},
		DNSNames:              []string{domain},
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().AddDate(1, 0, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certFile, err := os.OpenFile(certPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		certFile.Close()
		return err
	}
	if err := certFile.Close(); err != nil {
		return err
	}
	keyFile, err := os.OpenFile(keyPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		keyFile.Close()
		return err
	}
	return keyFile.Close()
}
