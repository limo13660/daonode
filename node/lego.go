package node

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/http01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns"
	"github.com/go-acme/lego/v4/registration"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/common/file"
)

type Lego struct {
	client *lego.Client
	config *panel.CertInfo
}

var (
	dnsEnvironmentMu sync.Mutex
	httpChallengeMu  sync.Mutex
)

func NewLego(config *panel.CertInfo) (*Lego, error) {
	if config == nil {
		return nil, fmt.Errorf("certificate config is nil")
	}
	if config.Email == "" || config.CertDomain == "" {
		return nil, fmt.Errorf("ACME email or certificate domain is empty")
	}
	certPath := expandCertificatePath(config.CertFile, config.CertDomain, config.Email)
	userPath := filepath.Join(filepath.Dir(certPath), "user", "user-"+acmeAccountID(config.Email)+".json")
	user, err := newLegoUser(userPath, config.Email)
	if err != nil {
		return nil, fmt.Errorf("create ACME user: %w", err)
	}
	legoConfig := lego.NewConfig(user)
	legoConfig.Certificate.KeyType = certcrypto.RSA2048
	client, err := lego.NewClient(legoConfig)
	if err != nil {
		return nil, err
	}
	manager := &Lego{client: client, config: config}
	if err := manager.setProvider(); err != nil {
		return nil, fmt.Errorf("set challenge provider: %w", err)
	}
	return manager, nil
}

func (l *Lego) setProvider() error {
	switch l.config.CertMode {
	case "http":
		return l.client.Challenge.SetHTTP01Provider(http01.NewProviderServer("", ""))
	case "dns":
		provider, err := newDNSChallengeProvider(l.config.Provider, l.config.DNSEnv)
		if err != nil {
			return err
		}
		return l.client.Challenge.SetDNS01Provider(provider)
	default:
		return fmt.Errorf("unsupported ACME cert mode: %s", l.config.CertMode)
	}
}

func (l *Lego) CreateCert() error {
	unlock := l.lockHTTPChallenge()
	defer unlock()
	resource, err := l.client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: []string{l.config.CertDomain},
		Bundle:  true,
	})
	if err != nil {
		return err
	}
	return l.writeCert(resource)
}

func (l *Lego) RenewCert() error {
	data, err := os.ReadFile(l.expandPath(l.config.CertFile))
	if err != nil {
		return err
	}
	shouldRenew, err := l.shouldRenew(data)
	if err != nil || !shouldRenew {
		return err
	}
	unlock := l.lockHTTPChallenge()
	defer unlock()
	resource, err := l.client.Certificate.Renew(certificate.Resource{
		Domain:      l.config.CertDomain,
		Certificate: data,
	}, true, false, "")
	if err != nil {
		return err
	}
	return l.writeCert(resource)
}

func (l *Lego) shouldRenew(data []byte) (bool, error) {
	cert, err := certcrypto.ParsePEMCertificate(data)
	if err != nil {
		return false, err
	}
	return time.Until(cert.NotAfter) <= 30*24*time.Hour, nil
}

func (l *Lego) writeCert(resource *certificate.Resource) error {
	if err := writeManagedFile(l.expandPath(l.config.CertFile), resource.Certificate, 0644); err != nil {
		return err
	}
	return writeManagedFile(l.expandPath(l.config.KeyFile), resource.PrivateKey, 0600)
}

func (l *Lego) expandPath(path string) string {
	return expandCertificatePath(path, l.config.CertDomain, l.config.Email)
}

func (l *Lego) lockHTTPChallenge() func() {
	if l.config.CertMode != "http" {
		return func() {}
	}
	httpChallengeMu.Lock()
	return httpChallengeMu.Unlock
}

func expandCertificatePath(path, domain, email string) string {
	replacer := strings.NewReplacer("{domain}", domain, "{email}", email)
	return replacer.Replace(path)
}

func acmeAccountID(email string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return fmt.Sprintf("%x", digest[:8])
}

func newDNSChallengeProvider(providerName string, variables map[string]string) (challenge.Provider, error) {
	dnsEnvironmentMu.Lock()
	defer dnsEnvironmentMu.Unlock()

	previous := make(map[string]*string, len(variables))
	for key, value := range variables {
		if current, exists := os.LookupEnv(key); exists {
			currentCopy := current
			previous[key] = &currentCopy
		} else {
			previous[key] = nil
		}
		if err := os.Setenv(key, value); err != nil {
			return nil, err
		}
	}
	defer func() {
		for key, value := range previous {
			if value == nil {
				_ = os.Unsetenv(key)
				continue
			}
			_ = os.Setenv(key, *value)
		}
	}()

	return dns.NewDNSChallengeProviderByName(providerName)
}

func writeManagedFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

type legoUser struct {
	Email        string                 `json:"email"`
	Registration *registration.Resource `json:"registration"`
	KeyEncoded   string                 `json:"key"`
	key          crypto.PrivateKey
}

func (u *legoUser) GetEmail() string                        { return u.Email }
func (u *legoUser) GetRegistration() *registration.Resource { return u.Registration }
func (u *legoUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

func newLegoUser(path, email string) (*legoUser, error) {
	user := &legoUser{}
	if file.IsExist(path) {
		if err := user.load(path); err != nil {
			return nil, err
		}
		if user.Email == email {
			return user, nil
		}
	}
	user.Email = email
	if err := registerLegoUser(user, path); err != nil {
		return nil, err
	}
	return user, nil
}

func registerLegoUser(user *legoUser, path string) error {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	user.key = privateKey
	client, err := lego.NewClient(lego.NewConfig(user))
	if err != nil {
		return err
	}
	resource, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return err
	}
	user.Registration = resource
	return user.save(path)
}

func (u *legoUser) save(path string) error {
	encoded, err := x509.MarshalECPrivateKey(u.key.(*ecdsa.PrivateKey))
	if err != nil {
		return err
	}
	u.KeyEncoded = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}))
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	u.KeyEncoded = ""
	return writeManagedFile(path, data, 0600)
}

func (u *legoUser) load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, u); err != nil {
		return err
	}
	block, _ := pem.Decode([]byte(u.KeyEncoded))
	if block == nil {
		return fmt.Errorf("ACME user private key is invalid")
	}
	u.key, err = x509.ParseECPrivateKey(block.Bytes)
	return err
}
