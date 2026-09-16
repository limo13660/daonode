package sudoku

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/core/contract"
	"github.com/limo13660/daonode/core/shared"
	transport "github.com/limo13660/daonode/core/sudoku/transport"
	log "github.com/sirupsen/logrus"
)

type userConfig struct {
	user   panel.UserInfo
	cfg    *transport.ProtocolConfig
	tunnel *transport.HTTPMaskTunnelServer
}
type userConfigSnapshot struct{ byHash map[string]userConfig }

var errHTTPMaskHandled = errors.New("Sudoku HTTPMask control connection handled")

type serverInstance struct {
	services *shared.RuntimeServices
	router   *routePolicy
	users    atomic.Pointer[userConfigSnapshot]
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan error
	close    sync.Once
	closeErr error
}

func buildUserConfigs(info *panel.NodeInfo, users map[int]panel.UserInfo) (*userConfigSnapshot, error) {
	return buildUserConfigsWithPrevious(info, users, nil)
}

func buildUserConfigsWithPrevious(info *panel.NodeInfo, users map[int]panel.UserInfo, previous *userConfigSnapshot) (*userConfigSnapshot, error) {
	if err := validateNodeInfo(info); err != nil {
		return nil, err
	}
	ps := info.Common.ProtocolSettings
	aead := strings.ToLower(strings.TrimSpace(ps.AEADMethod))
	if aead == "" {
		aead = "chacha20-poly1305"
	}
	tableType := strings.TrimSpace(ps.TableType)
	if tableType == "" {
		// DaoBoard and the official Sudoku config default to the entropy table.
		// Keep this fallback aligned so a legacy panel response that omits
		// table_type can still complete the handshake.
		tableType = "prefer_entropy"
	}
	normalizedTableType, err := transport.NormalizeTableType(tableType)
	if err != nil {
		return nil, err
	}
	tableType = normalizedTableType
	httpMaskMode := strings.ToLower(strings.TrimSpace(ps.HTTPMaskMode))
	if httpMaskMode == "" {
		httpMaskMode = "legacy"
	} else if httpMaskMode == "split-stream" {
		httpMaskMode = "stream"
	}
	multiplex, err := transport.NormalizeMultiplexMode(ps.Multiplex)
	if err != nil {
		return nil, fmt.Errorf("invalid multiplex: %w", err)
	}
	out := &userConfigSnapshot{byHash: make(map[string]userConfig, len(users))}
	for _, user := range users {
		key := strings.TrimSpace(user.Uuid)
		if key == "" {
			return nil, fmt.Errorf("Sudoku user %d has empty UUID", user.Id)
		}
		hash := transport.KIPUserHashHexFromKey(key)
		if _, ok := out.byHash[hash]; ok {
			return nil, fmt.Errorf("Sudoku user UUID hash is duplicated")
		}
		tables, err := transport.NewServerTablesWithCustomPatterns(key, tableType, ps.CustomTable, ps.CustomTables)
		if err != nil {
			return nil, fmt.Errorf("build Sudoku table for user %d: %w", user.Id, err)
		}
		cfg := &transport.ProtocolConfig{Key: key, AEADMethod: aead, Tables: tables, PaddingMin: ps.PaddingMin, PaddingMax: ps.PaddingMax, EnablePureDownlink: ps.EnablePureDownlink, HandshakeTimeoutSeconds: 5, DisableHTTPMask: !ps.HTTPMask, HTTPMaskMode: httpMaskMode, HTTPMaskTLSEnabled: ps.HTTPMaskTLS, HTTPMaskHost: ps.HTTPMaskHost, HTTPMaskPathRoot: ps.PathRoot, Multiplex: multiplex, HTTPMaskMultiplex: multiplex}
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("validate Sudoku user %d: %w", user.Id, err)
		}
		entry := userConfig{user: user, cfg: cfg}
		if !cfg.DisableHTTPMask && !strings.EqualFold(strings.TrimSpace(cfg.HTTPMaskMode), "") && !strings.EqualFold(strings.TrimSpace(cfg.HTTPMaskMode), "legacy") {
			if previousEntry, ok := previousUserConfig(previous, hash); ok {
				entry.tunnel = previousEntry.tunnel
			}
			if entry.tunnel == nil {
				entry.tunnel = transport.NewHTTPMaskTunnelServerWithFallback(cfg)
			}
		}
		out.byHash[hash] = entry
	}
	return out, nil
}

func previousUserConfig(snapshot *userConfigSnapshot, hash string) (userConfig, bool) {
	if snapshot == nil {
		return userConfig{}, false
	}
	entry, ok := snapshot.byHash[hash]
	return entry, ok
}

func startServer(info *panel.NodeInfo, services *shared.RuntimeServices, users *userConfigSnapshot) (*serverInstance, error) {
	if err := validateNodeInfo(info); err != nil {
		return nil, err
	}
	router, err := compileRoutePolicy(info.Common.Routes)
	if err != nil {
		return nil, fmt.Errorf("configure Sudoku routes: %w", err)
	}
	address := net.JoinHostPort(info.Common.ListenIP, strconv.Itoa(info.Common.ServerPort))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for Sudoku server: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &serverInstance{services: services, router: router, listener: listener, ctx: ctx, cancel: cancel, done: make(chan error, 1)}
	s.users.Store(users)
	go s.serve()
	log.WithFields(log.Fields{"protocol": "sudoku", "port": info.Common.ServerPort, "listen_ip": info.Common.ListenIP, "users": len(users.byHash)}).Info("Sudoku runtime started")
	return s, nil
}

func (s *serverInstance) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				s.done <- nil
			} else {
				s.done <- err
			}
			return
		}
		go s.handle(conn)
	}
}

func (s *serverInstance) handle(raw net.Conn) {
	defer raw.Close()
	entry, conn, meta, err := s.handshake(raw)
	if err != nil {
		return
	}
	// HTTPMask stream/poll sessions are backed by an in-process pipe whose
	// RemoteAddr is a synthetic "pipe" address.  Keep policy and online-user
	// reporting keyed to the actual accepted socket instead.
	session, ok := s.services.OpenSession(entry.user, conn, remoteAddress(raw), true)
	if !ok {
		return
	}
	defer session.Close()
	intent, err := transport.ReadServerSession(conn, meta)
	if err != nil {
		return
	}
	switch intent.Type {
	case transport.SessionTypeTCP:
		s.handleTCP(conn, intent.Target, entry.user, session)
	case transport.SessionTypeUoT:
		s.handleUoT(conn, entry.user, session)
	case transport.SessionTypeMultiplex:
		s.handleMux(conn, entry.user, session)
	}
}

func remoteAddress(c net.Conn) string {
	if c == nil || c.RemoteAddr() == nil {
		return ""
	}
	return c.RemoteAddr().String()
}

// handshake tries the UUID-specific keys against one replayable byte stream. The Sudoku
// client hides the UUID hash inside the encrypted KIP hello, so this is required for raw TCP.
func (s *serverInstance) handshake(raw net.Conn) (userConfig, net.Conn, *transport.HandshakeMeta, error) {
	snapshot := s.users.Load()
	if snapshot == nil {
		return userConfig{}, nil, nil, errors.New("no users")
	}
	entries := make([]userConfig, 0, len(snapshot.byHash))
	for _, e := range snapshot.byHash {
		entries = append(entries, e)
	}
	replay := newReplayConn(raw)
	for _, entry := range entries {
		cfg := *entry.cfg
		handshakeConn := net.Conn(replay)
		if entry.tunnel != nil {
			wrapped, tunnelCfg, done, wrapErr := entry.tunnel.WrapConn(replay)
			if wrapErr != nil || done {
				if done {
					return userConfig{}, nil, nil, errHTTPMaskHandled
				}
				replay.Reset()
				continue
			}
			if wrapped != nil {
				handshakeConn = wrapped
			}
			if tunnelCfg != nil {
				cfg = *tunnelCfg
			}
		}
		conn, meta, err := transport.ServerHandshake(handshakeConn, &cfg)
		if err == nil {
			if meta == nil || meta.UserHash != transport.KIPUserHashHexFromKey(entry.user.Uuid) {
				_ = conn.Close()
				replay.Reset()
				continue
			}
			return entry, conn, meta, nil
		}
		replay.Reset()
	}
	return userConfig{}, nil, nil, errors.New("Sudoku handshake rejected")
}

func (s *serverInstance) handleTCP(conn net.Conn, target string, user panel.UserInfo, session *shared.Session) {
	host, port, err := net.SplitHostPort(target)
	if err != nil || s.router.blocked(target, host, mustPort(port)) {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
	defer cancel()
	out, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
	if err != nil {
		return
	}
	defer out.Close()
	done := make(chan struct{}, 2)
	go func() { proxyCopy(session, out, conn, true); closeWrite(out); done <- struct{}{} }()
	go func() { proxyCopy(session, conn, out, false); closeWrite(conn); done <- struct{}{} }()
	<-done
}

func closeWrite(conn net.Conn) {
	if c, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
	}
}

func mustPort(v string) int { n, _ := strconv.Atoi(v); return n }

func proxyCopy(session *shared.Session, dst net.Conn, src net.Conn, upload bool) {
	buf := make([]byte, 32*1024)
	for {
		n, er := src.Read(buf)
		if n > 0 {
			if upload {
				session.WaitUpload(int64(n))
			} else {
				session.WaitDownload(int64(n))
			}
			w, ew := dst.Write(buf[:n])
			if upload {
				session.RecordUpload(int64(w))
			} else {
				session.RecordDownload(int64(w))
			}
			if ew != nil || w != n {
				return
			}
		}
		if er != nil {
			return
		}
	}
}

func (s *serverInstance) handleUoT(conn net.Conn, user panel.UserInfo, session *shared.Session) {
	ctx, cancel := context.WithCancel(s.ctx)
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	type udpFlow struct {
		target       string
		responseAddr string
		conn         *net.UDPConn
	}
	var (
		flowsMu sync.Mutex
		flows   = make(map[string]*udpFlow)
		flowsWG sync.WaitGroup
		writeMu sync.Mutex
	)
	writeBack := func(target string, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return transport.WriteDatagram(conn, target, payload)
	}
	removeFlow := func(flow *udpFlow) {
		if flow == nil {
			return
		}
		flowsMu.Lock()
		if current := flows[flow.target]; current == flow {
			delete(flows, flow.target)
		}
		flowsMu.Unlock()
		_ = flow.conn.Close()
	}
	defer func() {
		// Close the client-side stream before waiting for response workers; a
		// worker may be blocked writing a datagram while the runtime is being
		// stopped.
		_ = conn.Close()
		cancel()
		flowsMu.Lock()
		active := make([]*udpFlow, 0, len(flows))
		for target, flow := range flows {
			delete(flows, target)
			active = append(active, flow)
		}
		flowsMu.Unlock()
		for _, flow := range active {
			_ = flow.conn.Close()
		}
		flowsWG.Wait()
	}()

	for {
		target, payload, err := transport.ReadDatagram(conn)
		if err != nil {
			return
		}
		host, port, _ := net.SplitHostPort(target)
		if s.router.blocked(target, host, mustPort(port)) {
			continue
		}
		udpAddr, err := net.ResolveUDPAddr("udp", target)
		if err != nil {
			continue
		}

		// Keep one connected UDP socket per destination.  A single request/
		// response socket works for DNS, but it drops subsequent datagrams
		// from long-lived UDP protocols and serialises unrelated destinations.
		flowsMu.Lock()
		flow := flows[target]
		if flow == nil {
			out, dialErr := net.DialUDP("udp", nil, udpAddr)
			if dialErr == nil {
				flow = &udpFlow{
					target:       target,
					responseAddr: out.RemoteAddr().String(),
					conn:         out,
				}
				flows[target] = flow
				flowsWG.Add(1)
				go func(current *udpFlow) {
					defer flowsWG.Done()
					defer removeFlow(current)
					readBuf := make([]byte, 64*1024)
					for {
						_ = current.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
						rn, _, readErr := current.conn.ReadFromUDP(readBuf)
						if rn > 0 {
							session.WaitDownload(int64(rn))
							if writeErr := writeBack(current.responseAddr, readBuf[:rn]); writeErr != nil {
								return
							}
							session.RecordDownload(int64(rn))
						}
						if readErr != nil {
							if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
								return
							}
							return
						}
						select {
						case <-ctx.Done():
							return
						default:
						}
					}
				}(flow)
			}
		}
		flowsMu.Unlock()
		if flow == nil {
			continue
		}
		session.WaitUpload(int64(len(payload)))
		written, err := flow.conn.Write(payload)
		if written > 0 {
			session.RecordUpload(int64(written))
		}
		if err != nil || written != len(payload) {
			removeFlow(flow)
			continue
		}
	}
}

func (s *serverInstance) handleMux(conn net.Conn, user panel.UserInfo, session *shared.Session) {
	mux, err := transport.AcceptMultiplexServer(conn)
	if err != nil {
		return
	}
	defer mux.Close()
	for {
		stream, target, err := mux.AcceptTCP()
		if err != nil {
			return
		}
		go func() {
			defer stream.Close()
			host, port, err := net.SplitHostPort(target)
			if err != nil || s.router.blocked(target, host, mustPort(port)) {
				return
			}
			dialCtx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
			out, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", target)
			cancel()
			if err != nil {
				return
			}
			defer out.Close()
			go proxyCopy(session, out, stream, true)
			proxyCopy(session, stream, out, false)
		}()
	}
}

func (s *serverInstance) Close() error {
	s.close.Do(func() {
		s.cancel()
		_ = s.listener.Close()
		select {
		case err := <-s.done:
			s.closeErr = err
		case <-time.After(runtimeStopTimeout):
			s.closeErr = fmt.Errorf("%w after %s", contract.ErrRuntimeStopTimeout, runtimeStopTimeout)
		}
	})
	return s.closeErr
}

type replayConn struct {
	net.Conn
	mu     sync.Mutex
	cache  []byte
	offset int
}

func newReplayConn(c net.Conn) *replayConn { return &replayConn{Conn: c} }
func (r *replayConn) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.offset < len(r.cache) {
		n := copy(p, r.cache[r.offset:])
		r.offset += n
		return n, nil
	}
	n, e := r.Conn.Read(p)
	if n > 0 {
		r.cache = append(r.cache, p[:n]...)
		r.offset += n
	}
	return n, e
}
func (r *replayConn) Reset() { r.mu.Lock(); r.offset = 0; r.mu.Unlock() }

var _ io.ReadWriter = (*replayConn)(nil)
