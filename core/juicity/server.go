package juicity

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daeuniverse/outbound/ciphers"
	"github.com/daeuniverse/outbound/netproxy"
	"github.com/daeuniverse/outbound/pkg/fastrand"
	"github.com/daeuniverse/outbound/pool"
	"github.com/daeuniverse/outbound/protocol"
	"github.com/daeuniverse/outbound/protocol/direct"
	juicityProtocol "github.com/daeuniverse/outbound/protocol/juicity"
	"github.com/daeuniverse/outbound/protocol/shadowsocks"
	"github.com/daeuniverse/outbound/protocol/trojanc"
	"github.com/daeuniverse/outbound/protocol/tuic"
	tuicCommon "github.com/daeuniverse/outbound/protocol/tuic/common"
	quic "github.com/daeuniverse/quic-go"
	"github.com/google/uuid"
	"github.com/juicity/juicity/common/consts"
	log "github.com/sirupsen/logrus"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/core/contract"
	"github.com/limo13660/daonode/core/shared"
)

const authenticateTimeout = 10 * time.Second

var errAuthenticationFailed = errors.New("authentication failed")

type serverInstance struct {
	services   *shared.RuntimeServices
	users      atomic.Pointer[authSnapshot]
	router     *routePolicy
	dialer     netproxy.Dialer
	congestion string

	ctx       context.Context
	cancel    context.CancelFunc
	packet    net.PacketConn
	transport *quic.Transport
	listener  *quic.Listener
	done      chan error

	inFlight  *inFlightUnderlayKey
	endpoints *udpEndpointPool

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
	router, err := compileRoutePolicy(info.Common.Routes)
	if err != nil {
		return nil, fmt.Errorf("configure Juicity routes: %w", err)
	}
	address := net.JoinHostPort(info.Common.ListenIP, strconv.Itoa(info.Common.ServerPort))
	packet, err := net.ListenPacket("udp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for Juicity server: %w", err)
	}
	transport := &quic.Transport{Conn: packet}
	listener, err := transport.Listen(tlsConfig, &quic.Config{
		InitialStreamReceiveWindow:     tuicCommon.InitialStreamReceiveWindow,
		MaxStreamReceiveWindow:         tuicCommon.MaxStreamReceiveWindow,
		InitialConnectionReceiveWindow: tuicCommon.InitialConnectionReceiveWindow,
		MaxConnectionReceiveWindow:     tuicCommon.MaxConnectionReceiveWindow,
		MaxIncomingStreams:             100,
		MaxIncomingUniStreams:          100,
		KeepAlivePeriod:                10 * time.Second,
		DisablePathMTUDiscovery:        false,
		EnableDatagrams:                false,
	})
	if err != nil {
		_ = packet.Close()
		return nil, fmt.Errorf("start Juicity QUIC listener: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	instance := &serverInstance{
		services:   services,
		router:     router,
		dialer:     direct.FullconeDirect,
		congestion: normalizedCongestion(info.Common.ProtocolSettings.QUICCongestionControl),
		ctx:        ctx,
		cancel:     cancel,
		packet:     packet,
		transport:  transport,
		listener:   listener,
		done:       make(chan error, 1),
		inFlight:   newInFlightUnderlayKey(authenticateTimeout),
		endpoints:  newUDPEndpointPool(),
	}
	instance.users.Store(snapshot)
	go instance.serve()
	log.WithFields(log.Fields{
		"protocol":   "juicity",
		"kernel":     "juicity-official-v0.5.0",
		"port":       info.Common.ServerPort,
		"listen_ip":  info.Common.ListenIP,
		"transport":  "UDP",
		"users":      len(snapshot.users),
		"congestion": instance.congestion,
	}).Info("Juicity runtime started")
	return instance, nil
}

func (s *serverInstance) serve() {
	quicDone := make(chan error, 1)
	nonQUICDone := make(chan error, 1)
	go func() { quicDone <- s.serveQUIC() }()
	go func() { nonQUICDone <- s.serveNonQUIC() }()
	var err error
	select {
	case err = <-quicDone:
	case err = <-nonQUICDone:
	}
	stopping := s.ctx.Err() != nil
	s.cancel()
	_ = s.listener.Close()
	if stopping || errors.Is(err, net.ErrClosed) || errors.Is(err, quic.ErrServerClosed) {
		err = nil
	}
	s.done <- err
}

func (s *serverInstance) serveQUIC() error {
	for {
		conn, err := s.listener.Accept(s.ctx)
		if err != nil {
			return err
		}
		go func(current quic.Connection) {
			if err := s.handleConnection(current); err != nil && !isExpectedNetworkError(err) {
				log.WithError(err).Warn("Juicity connection stopped")
			}
		}(conn)
	}
}

func (s *serverInstance) serveNonQUIC() error {
	buffer := pool.GetFullCap(consts.EthernetMtu + juicityProtocol.CipherConf.SaltLen)
	defer buffer.Put()
	for {
		n, address, err := s.transport.ReadNonQUICPacket(s.ctx, buffer)
		if err != nil {
			if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		payload := pool.Get(n)
		copy(payload, buffer[:n])
		udpAddress, ok := address.(*net.UDPAddr)
		if !ok {
			payload.Put()
			continue
		}
		go func(data pool.PB, source *net.UDPAddr) {
			defer data.Put()
			if err := s.handleNonQUICPacket(data, source); err != nil && !isExpectedNetworkError(err) {
				log.WithError(err).Debug("Juicity underlay packet rejected")
			}
		}(payload, udpAddress)
	}
}

func (s *serverInstance) handleConnection(conn quic.Connection) error {
	if s.congestion == "bbr" {
		tuicCommon.SetCongestionController(conn, "bbr", 10)
	}
	authCtx, cancel := context.WithTimeout(s.ctx, authenticateTimeout)
	id, authStream, err := s.authenticate(authCtx, conn)
	cancel()
	if err != nil {
		_ = conn.CloseWithError(tuic.AuthenticationFailed, "")
		return err
	}
	user, ok := s.services.UserByUUID(id.String())
	if !ok {
		_ = conn.CloseWithError(tuic.AuthenticationFailed, "")
		return errAuthenticationFailed
	}
	handle := newConnectionHandle(conn)
	session, accepted := s.services.OpenSession(user, handle, conn.RemoteAddr().String(), true)
	if !accepted {
		_ = handle.Close()
		return errors.New("Juicity connection rejected by common policy")
	}
	defer session.Close()

	go s.readUnderlayAuthorizations(authStream, session, handle)
	for {
		stream, err := conn.AcceptStream(s.ctx)
		if err != nil {
			return err
		}
		go func(current quic.Stream) {
			if err := s.handleStream(conn, current, session); err != nil && !isExpectedNetworkError(err) {
				log.WithError(err).Debug("Juicity stream stopped")
			}
		}(stream)
	}
}

func (s *serverInstance) authenticate(ctx context.Context, conn quic.Connection) (uuid.UUID, quic.ReceiveStream, error) {
	stream, err := conn.AcceptUniStream(ctx)
	if err != nil {
		return uuid.Nil, nil, err
	}
	reader := bufio.NewReader(stream)
	version, err := reader.Peek(1)
	if err != nil {
		return uuid.Nil, nil, err
	}
	if version[0] != juicityProtocol.Version0 {
		return uuid.Nil, nil, fmt.Errorf("unexpected Juicity version: %d", version[0])
	}
	header, err := tuic.ReadCommandHead(reader)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("read Juicity command head: %w", err)
	}
	if header.TYPE != tuic.AuthenticateType {
		return uuid.Nil, nil, fmt.Errorf("unexpected Juicity command type: %d", header.TYPE)
	}
	auth, err := tuic.ReadAuthenticateWithHead(header, reader)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("read Juicity authentication: %w", err)
	}
	snapshot := s.users.Load()
	credential, ok := snapshot.users[auth.UUID]
	if !ok {
		return uuid.Nil, nil, fmt.Errorf("%w: %s", errAuthenticationFailed, auth.UUID)
	}
	token, err := tuic.GenToken(conn.ConnectionState(), auth.UUID, credential.password)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("generate Juicity authentication token: %w", err)
	}
	if token != auth.TOKEN {
		return uuid.Nil, nil, fmt.Errorf("%w: %s", errAuthenticationFailed, auth.UUID)
	}
	return auth.UUID, stream, nil
}

func (s *serverInstance) readUnderlayAuthorizations(stream quic.ReceiveStream, session *shared.Session, handle *connectionHandle) {
	for {
		var auth juicityProtocol.UnderlayAuth
		if _, err := auth.Unpack(stream); err != nil {
			if !errors.Is(err, io.EOF) && !isExpectedNetworkError(err) {
				log.WithError(err).Debug("Juicity underlay authentication stream stopped")
			}
			return
		}
		key, ok := underlayKey(auth.IV)
		if !ok {
			continue
		}
		s.inFlight.Store(key, &underlayAuthorization{
			auth:    &auth,
			session: session,
			handle:  handle,
		})
	}
}

func (s *serverInstance) handleStream(conn quic.Connection, stream quic.Stream, session *shared.Session) error {
	metadata, err := readStreamMetadata(stream)
	if err != nil {
		return err
	}
	client := &quicStreamConn{Stream: stream, local: conn.LocalAddr(), remote: conn.RemoteAddr()}
	defer client.Close()
	target := net.JoinHostPort(metadata.Hostname, strconv.Itoa(int(metadata.Port)))
	if s.router.blocked(metadata.Network, metadata.Hostname, int(metadata.Port)) {
		return fmt.Errorf("Juicity route blocked %s", target)
	}
	source := conn.RemoteAddr().String()
	switch metadata.Network {
	case "tcp":
		ctx, cancel := context.WithTimeout(s.ctx, consts.DefaultDialTimeout)
		defer cancel()
		upstream, err := s.dialer.DialContext(ctx, "tcp", target)
		if err != nil {
			return err
		}
		defer upstream.Close()
		log.WithFields(log.Fields{"source": source, "target": target}).Debug("Juicity TCP request")
		return relayTCP(session, client, upstream)
	case "udp":
		log.WithFields(log.Fields{"source": source, "target": target}).Debug("Juicity UDP request")
		return s.relayStreamUDP(session, newStreamPacketConn(client), target)
	default:
		return fmt.Errorf("unexpected Juicity network: %s", metadata.Network)
	}
}

func relayTCP(session *shared.Session, client, upstream netproxy.Conn) error {
	uploadDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(&sessionWriter{writer: upstream, session: session, upload: true}, client)
		if closer, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		_ = upstream.SetReadDeadline(time.Now().Add(10 * time.Second))
		uploadDone <- err
	}()
	_, downloadErr := io.Copy(&sessionWriter{writer: client, session: session}, upstream)
	if closer, ok := client.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
	}
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	uploadErr := <-uploadDone
	return errors.Join(normalizeRelayError(uploadErr), normalizeRelayError(downloadErr))
}

type sessionWriter struct {
	writer  io.Writer
	session *shared.Session
	upload  bool
}

func (w *sessionWriter) Write(buffer []byte) (int, error) {
	if w.upload {
		w.session.WaitUpload(int64(len(buffer)))
	} else {
		w.session.WaitDownload(int64(len(buffer)))
	}
	n, err := w.writer.Write(buffer)
	if w.upload {
		w.session.RecordUpload(int64(n))
	} else {
		w.session.RecordDownload(int64(n))
	}
	return n, err
}

func (s *serverInstance) relayStreamUDP(session *shared.Session, client *streamPacketConn, target string) error {
	buffer := pool.GetFullCap(consts.EthernetMtu)
	defer buffer.Put()
	_ = client.SetReadDeadline(time.Now().Add(consts.DefaultNatTimeout))
	n, address, err := client.ReadFrom(buffer)
	if err != nil {
		return fmt.Errorf("read first Juicity UDP packet: %w", err)
	}
	if s.router.blocked("udp", address.Addr().String(), int(address.Port())) {
		return fmt.Errorf("Juicity route blocked %s", address)
	}
	ctx, cancel := context.WithTimeout(s.ctx, consts.DefaultDialTimeout)
	defer cancel()
	connection, err := s.dialer.DialContext(ctx, "udp", target)
	if err != nil {
		return err
	}
	upstream, ok := connection.(netproxy.PacketConn)
	if !ok {
		_ = connection.Close()
		return errors.New("Juicity outbound does not support UDP")
	}
	defer upstream.Close()
	session.WaitUpload(int64(n))
	written, err := upstream.WriteTo(buffer[:n], address.String())
	session.RecordUpload(int64(written))
	if err != nil && !errors.Is(err, net.ErrWriteToConnected) {
		return err
	}
	return relayUDP(session, s.router, upstream, client, len(buffer))
}

func relayUDP(session *shared.Session, router *routePolicy, upstream netproxy.PacketConn, client *streamPacketConn, bufferSize int) error {
	uploadDone := make(chan error, 1)
	go func() {
		buffer := pool.GetFullCap(bufferSize)
		defer buffer.Put()
		for {
			_ = client.SetReadDeadline(time.Now().Add(consts.DefaultNatTimeout))
			n, address, err := client.ReadFrom(buffer)
			if err != nil {
				uploadDone <- normalizeRelayError(err)
				return
			}
			if router.blocked("udp", address.Addr().String(), int(address.Port())) {
				continue
			}
			session.WaitUpload(int64(n))
			written, err := upstream.WriteTo(buffer[:n], address.String())
			session.RecordUpload(int64(written))
			if err != nil && !errors.Is(err, net.ErrWriteToConnected) {
				uploadDone <- normalizeRelayError(err)
				return
			}
		}
	}()
	buffer := pool.GetFullCap(bufferSize)
	defer buffer.Put()
	for {
		_ = upstream.SetReadDeadline(time.Now().Add(consts.DefaultNatTimeout))
		n, address, err := upstream.ReadFrom(buffer)
		if err != nil {
			return errors.Join(normalizeRelayError(err), <-uploadDone)
		}
		session.WaitDownload(int64(n))
		written, err := client.WriteTo(buffer[:n], address.String())
		session.RecordDownload(int64(written))
		if err != nil {
			return errors.Join(normalizeRelayError(err), <-uploadDone)
		}
	}
}

func (s *serverInstance) handleNonQUICPacket(buffer []byte, address *net.UDPAddr) error {
	if len(buffer) < juicityProtocol.CipherConf.SaltLen {
		return fmt.Errorf("insufficient Juicity underlay data: %d bytes", len(buffer))
	}
	source := address.AddrPort()
	endpoint, created, err := s.endpoints.GetOrCreate(source, func() (*udpEndpoint, error) {
		key, ok := underlayKey(buffer[:juicityProtocol.CipherConf.SaltLen])
		if !ok {
			return nil, errAuthenticationFailed
		}
		authorization := s.inFlight.Evict(s.ctx, key)
		if authorization == nil || authorization.auth.Metadata == nil {
			return nil, errAuthenticationFailed
		}
		metadata := authorization.auth.Metadata
		target := net.JoinHostPort(metadata.Hostname, strconv.Itoa(int(metadata.Port)))
		if s.router.blocked("udp", metadata.Hostname, int(metadata.Port)) {
			return nil, fmt.Errorf("Juicity route blocked %s", target)
		}
		endpoint, err := newUDPEndpoint(s.ctx, target, s.dialer, &underlayMetadata{
			masterKey: authorization.auth.Psk,
			session:   authorization.session,
		}, func(data []byte, metadata *underlayMetadata) error {
			metadata.session.WaitDownload(int64(len(data)))
			salt := pool.Get(juicityProtocol.CipherConf.SaltLen)
			defer salt.Put()
			_, _ = fastrand.Read(salt)
			salt[0], salt[1] = 0, 0
			encrypted, err := shadowsocks.EncryptUDPFromPool(&shadowsocks.Key{
				CipherConf: juicityProtocol.CipherConf,
				MasterKey:  metadata.masterKey,
			}, data, salt, ciphers.JuicityReusedInfo)
			if err != nil {
				return err
			}
			defer encrypted.Put()
			_, err = s.transport.WriteTo(encrypted, address)
			if err == nil {
				metadata.session.RecordDownload(int64(len(data)))
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		endpoint.setCloseCallback(func() { authorization.handle.RemoveEndpoint(endpoint) })
		if !authorization.handle.AddEndpoint(endpoint) {
			_ = endpoint.Close()
			return nil, net.ErrClosed
		}
		return endpoint, nil
	})
	if err != nil {
		return err
	}
	metadata := endpoint.metadata
	decrypted, err := shadowsocks.DecryptUDPFromPool(&shadowsocks.Key{
		CipherConf: juicityProtocol.CipherConf,
		MasterKey:  metadata.masterKey,
	}, buffer, ciphers.JuicityReusedInfo)
	if err != nil {
		if created {
			s.endpoints.Remove(source, endpoint)
		}
		return err
	}
	defer decrypted.Put()
	metadata.session.WaitUpload(int64(len(decrypted)))
	written, err := endpoint.Write(decrypted)
	metadata.session.RecordUpload(int64(written))
	return err
}

func (s *serverInstance) Close() error {
	s.close.Do(func() {
		s.cancel()
		s.services.CloseAllConnections()
		s.inFlight.Close()
		s.endpoints.Close()
		closeErr := errors.Join(s.listener.Close(), s.transport.Close(), s.packet.Close())
		select {
		case serveErr := <-s.done:
			s.closeErr = joinServerErrors(closeErr, serveErr)
		case <-time.After(runtimeStopTimeout):
			s.closeErr = fmt.Errorf("%w after %s", contract.ErrRuntimeStopTimeout, runtimeStopTimeout)
		}
	})
	return s.closeErr
}

func buildTLSConfig(common *panel.CommonNode) (*tls.Config, error) {
	if common == nil || common.CertInfo == nil {
		return nil, errors.New("Juicity TLS certificate configuration is missing")
	}
	cert := common.CertInfo
	if cert.CertMode == "" || cert.CertMode == "none" || cert.CertFile == "" || cert.KeyFile == "" {
		return nil, errors.New("Juicity requires TLS certificate files")
	}
	certificate, err := tls.LoadX509KeyPair(cert.CertFile, cert.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load Juicity TLS certificate: %w", err)
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h3"},
	}
	if strings.EqualFold(strings.TrimSpace(common.TlsSettings.ECH), "custom") {
		keys, err := parseECHKeys(common.TlsSettings.ECHKey)
		if err != nil {
			return nil, fmt.Errorf("configure Juicity ECH: %w", err)
		}
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
			return nil, errors.New("unrecognized server name")
		}
	}
	return config, nil
}

func parseECHKeys(value string) ([]tls.EncryptedClientHelloKey, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("ECH keys are empty")
	}
	block, rest := pem.Decode([]byte(value))
	if block == nil {
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(raw) == 0 {
			return nil, errors.New("invalid ECH keys")
		}
		block = &pem.Block{Type: "ECH KEYS", Bytes: raw}
	} else if strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("invalid ECH keys PEM")
	}
	if block.Type != "ECH KEYS" {
		return nil, fmt.Errorf("invalid ECH keys PEM type %q", block.Type)
	}
	raw := block.Bytes
	keys := make([]tls.EncryptedClientHelloKey, 0, 1)
	for len(raw) > 0 {
		privateKey, remaining, ok := readUint16Bytes(raw)
		if !ok {
			return nil, errors.New("parse ECH private key")
		}
		config, remaining, ok := readUint16Bytes(remaining)
		if !ok {
			return nil, errors.New("parse ECH config")
		}
		keys = append(keys, tls.EncryptedClientHelloKey{
			PrivateKey:  append([]byte(nil), privateKey...),
			Config:      append([]byte(nil), config...),
			SendAsRetry: true,
		})
		raw = remaining
	}
	if len(keys) == 0 {
		return nil, errors.New("ECH keys are empty")
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

func normalizedCongestion(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "bbr"
	}
	return value
}

func normalizeRelayError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || isExpectedNetworkError(err) {
		return nil
	}
	return err
}

func isExpectedNetworkError(err error) bool {
	if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func joinServerErrors(errs ...error) error {
	filtered := make([]error, 0, len(errs))
	for _, err := range errs {
		if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, quic.ErrServerClosed) {
			continue
		}
		filtered = append(filtered, err)
	}
	return errors.Join(filtered...)
}

func underlayKey(raw []byte) ([juicityProtocol.UnderlaySaltLen]byte, bool) {
	var key [juicityProtocol.UnderlaySaltLen]byte
	if len(raw) < len(key) {
		return key, false
	}
	copy(key[:], raw[:len(key)])
	return key, true
}

func readStreamMetadata(stream io.Reader) (*trojanc.Metadata, error) {
	var network [1]byte
	if _, err := io.ReadFull(stream, network[:]); err != nil {
		return nil, err
	}
	metadata := &trojanc.Metadata{Network: trojanc.ParseNetwork(network[0])}
	if metadata.Network != "tcp" && metadata.Network != "udp" {
		return nil, fmt.Errorf("unexpected Juicity network byte: %d", network[0])
	}
	if _, err := metadata.Unpack(stream); err != nil {
		return nil, fmt.Errorf("read Juicity destination: %w", err)
	}
	return metadata, nil
}

type quicStreamConn struct {
	quic.Stream
	local  net.Addr
	remote net.Addr
}

func (c *quicStreamConn) LocalAddr() net.Addr  { return c.local }
func (c *quicStreamConn) RemoteAddr() net.Addr { return c.remote }
func (c *quicStreamConn) SetDeadline(deadline time.Time) error {
	return errors.Join(c.SetReadDeadline(deadline), c.SetWriteDeadline(deadline))
}

type streamPacketConn struct {
	connection      *quicStreamConn
	readMu          sync.Mutex
	writeMu         sync.Mutex
	domainIPMapping sync.Map
}

func newStreamPacketConn(connection *quicStreamConn) *streamPacketConn {
	return &streamPacketConn{connection: connection}
}

func (c *streamPacketConn) ReadFrom(buffer []byte) (int, netip.AddrPort, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	metadata := &trojanc.Metadata{}
	if _, err := metadata.Unpack(c.connection); err != nil {
		return 0, netip.AddrPort{}, err
	}
	address, err := metadata.DomainIpMapping(&c.domainIPMapping)
	if err != nil {
		return 0, netip.AddrPort{}, err
	}
	var lengthBuffer [2]byte
	if _, err := io.ReadFull(c.connection, lengthBuffer[:]); err != nil {
		return 0, netip.AddrPort{}, err
	}
	length := int(binary.BigEndian.Uint16(lengthBuffer[:]))
	if length <= len(buffer) {
		n, err := io.ReadFull(c.connection, buffer[:length])
		return n, address, err
	}
	n, err := io.ReadFull(c.connection, buffer)
	if err != nil {
		return n, address, err
	}
	_, err = io.CopyN(io.Discard, c.connection, int64(length-len(buffer)))
	return n, address, err
}

func (c *streamPacketConn) WriteTo(buffer []byte, address string) (int, error) {
	metadata, err := protocol.ParseMetadata(address)
	if err != nil {
		return 0, err
	}
	wrapped := trojanc.Metadata{Metadata: metadata, Network: "udp"}
	frame := make([]byte, wrapped.Len()+2+len(buffer))
	length := wrapped.PackTo(frame)
	binary.BigEndian.PutUint16(frame[length:], uint16(len(buffer)))
	copy(frame[length+2:], buffer)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.connection.Write(frame); err != nil {
		return 0, err
	}
	return len(buffer), nil
}

func (c *streamPacketConn) Close() error { return c.connection.Close() }
func (c *streamPacketConn) SetReadDeadline(deadline time.Time) error {
	return c.connection.SetReadDeadline(deadline)
}
func (c *streamPacketConn) SetWriteDeadline(deadline time.Time) error {
	return c.connection.SetWriteDeadline(deadline)
}

var _ netproxy.Conn = (*quicStreamConn)(nil)

var _ io.Closer = (*serverInstance)(nil)
