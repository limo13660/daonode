package node

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
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

func NewLego(config *panel.CertInfo) (*Lego, error) {
	if config == nil {
		return nil, fmt.Errorf("certificate config is nil")
	}
	userPath := filepath.Join(filepath.Dir(config.CertFile), "user", fmt.Sprintf("user-%s.json", config.Email))
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
		return l.client.Challenge.SetHTTP01Provider(http01.NewProviderServer("", "80"))
	case "dns":
		for key, value := range l.config.DNSEnv {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
		provider, err := dns.NewDNSChallengeProviderByName(l.config.Provider)
		if err != nil {
			return err
		}
		return l.client.Challenge.SetDNS01Provider(provider)
	default:
		return fmt.Errorf("unsupported ACME cert mode: %s", l.config.CertMode)
	}
}

func (l *Lego) CreateCert() error {
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
	data, err := os.ReadFile(l.config.CertFile)
	if err != nil {
		return err
	}
	shouldRenew, err := l.shouldRenew(data)
	if err != nil || !shouldRenew {
		return err
	}
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
	replacer := strings.NewReplacer("{domain}", l.config.CertDomain, "{email}", l.config.Email)
	return replacer.Replace(path)
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
