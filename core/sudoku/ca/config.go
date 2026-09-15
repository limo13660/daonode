package ca

import (
	"crypto/x509"
	"github.com/metacubex/tls"
)

type Option struct{ TLSConfig *tls.Config }

func GetTLSConfig(opt Option) (*tls.Config, error) {
	cfg := opt.TLSConfig
	if cfg == nil {
		cfg = &tls.Config{}
	}
	if cfg.RootCAs == nil {
		pool, err := x509.SystemCertPool()
		if err == nil {
			cfg.RootCAs = pool
		}
	}
	return cfg, nil
}
