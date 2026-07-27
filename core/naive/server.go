package naive

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/core/contract"
	"github.com/limo13660/daonode/core/shared"
)

type serverInstance struct {
	handler  *proxyHandler
	http     *http.Server
	http3    *http3.Server
	listener net.Listener
	packet   net.PacketConn
	done     chan error
	close    sync.Once
	closeErr error
}

func startServer(info *panel.NodeInfo, services *shared.RuntimeServices, snapshot *authSnapshot) (*serverInstance, error) {
	if err := validateNodeInfo(info); err != nil {
		return nil, err
	}
	tlsConfig, err := buildTLSConfig(info.Common)
	if err != nil {
		return nil, err
	}
	handler := &proxyHandler{services: services}
	handler.snapshot.Store(snapshot)
	address := net.JoinHostPort(info.Common.ListenIP, fmt.Sprint(info.Common.ServerPort))
	instance := &serverInstance{handler: handler, done: make(chan error, 1)}

	switch strings.ToUpper(info.Common.TransportProtocol) {
	case "", "TCP":
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return nil, fmt.Errorf("listen for official Naive server: %w", err)
		}
		serverHandler := http.Handler(handler)
		if tlsConfig == nil {
			serverHandler = h2c.NewHandler(serverHandler, &http2.Server{})
		}
		httpServer := &http.Server{
			Handler:           serverHandler,
			ReadHeaderTimeout: 15 * time.Second,
			IdleTimeout:       5 * time.Minute,
		}
		if tlsConfig != nil {
			httpServer.TLSConfig = tlsConfig
			if err := http2.ConfigureServer(httpServer, &http2.Server{}); err != nil {
				_ = listener.Close()
				return nil, fmt.Errorf("configure Naive HTTP/2 server: %w", err)
			}
			listener = tls.NewListener(listener, tlsConfig)
		}
		instance.http = httpServer
		instance.listener = listener
		go instance.serveHTTP()
	case "UDP":
		packet, err := net.ListenPacket("udp", address)
		if err != nil {
			return nil, fmt.Errorf("listen for official Naive HTTP/3 server: %w", err)
		}
		http3Server := &http3.Server{
			Handler:     handler,
			TLSConfig:   tlsConfig,
			IdleTimeout: 5 * time.Minute,
		}
		instance.http3 = http3Server
		instance.packet = packet
		go instance.serveHTTP3()
	}

	log.WithFields(log.Fields{
		"protocol":  "naive",
		"kernel":    "forwardproxy",
		"port":      info.Common.ServerPort,
		"listen_ip": info.Common.ListenIP,
		"transport": strings.ToUpper(info.Common.TransportProtocol),
		"users":     len(snapshot.users),
	}).Info("Official Naive runtime started")
	return instance, nil
}

func (s *serverInstance) serveHTTP() {
	err := s.http.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	s.done <- err
}

func (s *serverInstance) serveHTTP3() {
	err := s.http3.Serve(s.packet)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	s.done <- err
}

func (s *serverInstance) Close() error {
	s.close.Do(func() {
		var closeErr error
		if s.handler != nil {
			s.handler.services.CloseAllConnections()
		}
		if s.http != nil {
			closeErr = errors.Join(closeErr, s.http.Close())
		}
		if s.http3 != nil {
			closeErr = errors.Join(closeErr, s.http3.Close())
		}
		if s.listener != nil {
			closeErr = errors.Join(closeErr, s.listener.Close())
		}
		if s.packet != nil {
			closeErr = errors.Join(closeErr, s.packet.Close())
		}
		select {
		case serveErr := <-s.done:
			s.closeErr = joinServerErrors(closeErr, serveErr)
		case <-time.After(runtimeStopTimeout):
			s.closeErr = fmt.Errorf("%w after %s", contract.ErrRuntimeStopTimeout, runtimeStopTimeout)
		}
	})
	return s.closeErr
}

func joinServerErrors(errs ...error) error {
	filtered := make([]error, 0, len(errs))
	for _, err := range errs {
		if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			continue
		}
		filtered = append(filtered, err)
	}
	return errors.Join(filtered...)
}

func buildTLSConfig(common *panel.CommonNode) (*tls.Config, error) {
	if common == nil {
		return nil, fmt.Errorf("Naive node configuration is missing")
	}
	cert := common.CertInfo
	if cert == nil || cert.CertMode == "none" {
		if strings.EqualFold(common.TransportProtocol, "UDP") {
			return nil, fmt.Errorf("Naive HTTP/3 requires a TLS certificate")
		}
		return nil, nil
	}
	if cert.CertFile == "" || cert.KeyFile == "" {
		return nil, fmt.Errorf("Naive TLS certificate files are missing")
	}
	certificate, err := tls.LoadX509KeyPair(cert.CertFile, cert.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load Naive TLS certificate: %w", err)
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
	if strings.EqualFold(strings.TrimSpace(common.TlsSettings.ECH), "custom") {
		keys, err := parseECHKeys(common.TlsSettings.ECHKey)
		if err != nil {
			return nil, fmt.Errorf("configure Naive ECH: %w", err)
		}
		config.MinVersion = tls.VersionTLS13
		config.EncryptedClientHelloKeys = keys
	}
	if cert.RejectUnknownSni {
		allowed := append([]string(nil), cert.CertDomains...)
		allowed = append(allowed, common.TlsSettings.ServerName, common.TlsSettings.ECHServerName)
		config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			for _, name := range allowed {
				if matchServerName(name, hello.ServerName) {
					return nil, nil
				}
			}
			return nil, fmt.Errorf("unrecognized server name")
		}
	}
	return config, nil
}

func parseECHKeys(value string) ([]tls.EncryptedClientHelloKey, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("ECH keys are empty")
	}
	block, rest := pem.Decode([]byte(value))
	if block == nil {
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(raw) == 0 {
			return nil, fmt.Errorf("invalid ECH keys")
		}
		block = &pem.Block{Type: "ECH KEYS", Bytes: raw}
	} else if strings.TrimSpace(string(rest)) != "" {
		return nil, fmt.Errorf("invalid ECH keys PEM")
	}
	if block.Type != "ECH KEYS" {
		return nil, fmt.Errorf("invalid ECH keys PEM type %q", block.Type)
	}
	raw := block.Bytes
	keys := make([]tls.EncryptedClientHelloKey, 0, 1)
	for len(raw) > 0 {
		privateKey, remaining, ok := readUint16Bytes(raw)
		if !ok {
			return nil, fmt.Errorf("parse ECH private key")
		}
		config, remaining, ok := readUint16Bytes(remaining)
		if !ok {
			return nil, fmt.Errorf("parse ECH config")
		}
		keys = append(keys, tls.EncryptedClientHelloKey{
			PrivateKey:  append([]byte(nil), privateKey...),
			Config:      append([]byte(nil), config...),
			SendAsRetry: true,
		})
		raw = remaining
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("ECH keys are empty")
	}
	return keys, nil
}

func readUint16Bytes(raw []byte) ([]byte, []byte, bool) {
	if len(raw) < 2 {
		return nil, raw, false
	}
	length := int(binary.BigEndian.Uint16(raw[:2]))
	if len(raw) < 2+length {
		return nil, raw, false
	}
	return raw[2 : 2+length], raw[2+length:], true
}

func matchServerName(pattern, actual string) bool {
	pattern = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(pattern)), ".")
	actual = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(actual)), ".")
	if pattern == "" || actual == "" {
		return false
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return strings.HasSuffix(actual, suffix) && strings.Count(actual, ".") >= strings.Count(pattern, ".")
	}
	return pattern == actual
}
