package node

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/limo13660/daonode/common/file"
)

func (c *Controller) renewCertTask(_ context.Context) error {
	cert := c.info.Common.CertInfo
	if cert == nil {
		return nil
	}
	if cert.CertMode == "self" {
		if selfSignedCertificateIsUsable(cert.CertDomains, cert.CertFile, cert.KeyFile) {
			return nil
		}
		if err := generateSelfSignedCertificate(cert.CertDomains, cert.CertFile, cert.KeyFile); err != nil {
			log.WithField("tag", c.tag).Info("renew self-signed cert error: ", err)
			return nil
		}
		log.WithField("tag", c.tag).Info("self-signed certificate renewed, requesting runtime reload")
		c.requestRuntimeReload()
		return nil
	}
	manager, err := NewLego(c.info.Common.CertInfo)
	if err != nil {
		log.WithField("tag", c.tag).Info("new lego error: ", err)
		return nil
	}
	renewed, err := manager.RenewCert()
	if err != nil {
		log.WithField("tag", c.tag).Info("renew cert error: ", err)
		return nil
	}
	if renewed {
		log.WithField("tag", c.tag).Info("certificate renewed, requesting runtime reload")
		c.requestRuntimeReload()
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
		if selfSignedCertificateIsUsable(cert.CertDomains, cert.CertFile, cert.KeyFile) {
			return nil
		}
		if err := generateSelfSignedCertificate(cert.CertDomains, cert.CertFile, cert.KeyFile); err != nil {
			return fmt.Errorf("generate self-signed cert: %w", err)
		}
	default:
		return fmt.Errorf("unsupported cert mode: %s", cert.CertMode)
	}
	return nil
}

func generateSelfSignedCertificate(domains []string, certPath, keyPath string) error {
	if len(domains) == 0 || domains[0] == "" {
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
		Subject:               pkix.Name{CommonName: domains[0]},
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(90 * 24 * time.Hour),
	}
	for _, domain := range domains {
		if ip := net.ParseIP(domain); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, domain)
		}
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

func selfSignedCertificateIsUsable(domains []string, certPath, keyPath string) bool {
	if len(domains) == 0 || !file.IsExist(certPath) || !file.IsExist(keyPath) {
		return false
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil || len(pair.Certificate) == 0 {
		return false
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false
	}
	if certificate.NotAfter.Sub(certificate.NotBefore) > 180*24*time.Hour ||
		time.Until(certificate.NotAfter) <= 30*24*time.Hour ||
		time.Now().Before(certificate.NotBefore) {
		return false
	}
	for _, domain := range domains {
		if err := certificate.VerifyHostname(domain); err != nil {
			return false
		}
	}
	return true
}
