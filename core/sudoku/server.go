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
	user             panel.UserInfo
	cfg              *transport.ProtocolConfig
	tunnel           *transport.HTTPMaskTunnelServer
	tableFingerprint string
}
type userConfigSnapshot struct {
	byHash  map[string]userConfig
	entries []userConfig
}

var errHTTPMaskHandled = errors.New("Sudoku HTTPMask control connection handled")

// A Sudoku handshake is deliberately CPU-heavy (table probing, AEAD and
// X25519). Keep unauthenticated work bounded so a burst of probes cannot
// starve established proxy sessions or make the host appear hung.
const (
	// Keep the expensive unauthenticated path small on low-memory nodes. A
	// single Shadowrocket client can open several probes and reconnects at once.
	maxConcurrentSudokuHandshakes = 8
	// HTTPMask stream/poll may legitimately use several underlying sockets
	// for one logical connection, so keep this equal to the global budget.
	maxConcurrentSudokuHandshakesPerSource = 8
)

type serverInstance struct {
	services           *shared.RuntimeServices
	router             *routePolicy
	users              atomic.Pointer[userConfigSnapshot]
	listener           net.Listener
	ctx                context.Context
	cancel             context.CancelFunc
	done               chan error
	close              sync.Once
	closeErr           error
	connMu             sync.Mutex
	conns              map[net.Conn]struct{}
	handlers           sync.WaitGroup
	handshakeSlots     chan struct{}
	handshakeMu        sync.Mutex
	handshakesBySource map[string]int
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
	out := &userConfigSnapshot{
		byHash:  make(map[string]userConfig, len(users)),
		entries: make([]userConfig, 0, len(users)),
	}
	for _, user := range users {
		key := strings.TrimSpace(user.Uuid)
		if key == "" {
			return nil, fmt.Errorf("Sudoku user %d has empty UUID", user.Id)
		}
		hash := transport.KIPUserHashHexFromKey(key)
		if _, ok := out.byHash[hash]; ok {
			return nil, fmt.Errorf("Sudoku user UUID hash is duplicated")
		}
		cfg := &transport.ProtocolConfig{Key: key, AEADMethod: aead, PaddingMin: ps.PaddingMin, PaddingMax: ps.PaddingMax, EnablePureDownlink: ps.EnablePureDownlink, HandshakeTimeoutSeconds: 5, DisableHTTPMask: !ps.HTTPMask, HTTPMaskMode: httpMaskMode, HTTPMaskTLSEnabled: ps.HTTPMaskTLS, HTTPMaskHost: ps.HTTPMaskHost, HTTPMaskPathRoot: ps.PathRoot, Multiplex: multiplex, HTTPMaskMultiplex: multiplex}
		tableFingerprint := sudokuTableFingerprint(tableType, ps.CustomTable, ps.CustomTables)
		if previousEntry, ok := previousUserConfig(previous, hash); ok && previousEntry.cfg != nil && previousEntry.tableFingerprint == tableFingerprint && sameSudokuProtocolConfig(previousEntry.cfg, cfg) {
			// User polling frequently returns a changed list while the protocol
			// settings remain identical. Reuse the immutable table set instead
			// of rebuilding the expensive Sudoku mapping for every UUID.
			previousEntry.user = user
			out.byHash[hash] = previousEntry
			out.entries = append(out.entries, previousEntry)
			continue
		}
		tables, err := transport.NewServerTablesWithCustomPatterns(transport.ServerAEADSeed(key), tableType, ps.CustomTable, ps.CustomTables)
		if err != nil {
			return nil, fmt.Errorf("build Sudoku table for user %d: %w", user.Id, err)
		}
		cfg.Tables = tables
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("validate Sudoku user %d: %w", user.Id, err)
		}
		entry := userConfig{user: user, cfg: cfg, tableFingerprint: tableFingerprint}
		out.byHash[hash] = entry
		out.entries = append(out.entries, entry)
	}

	// HTTPMask stream/poll/ws must be shared by all UUIDs on this listener. A
	// per-user tunnel would consume the request/session in the first user's
	// handler, then force the server to retry against the remaining users. The
	// shared tunnel decrypts the early Sudoku hello once and tags the resulting
	// connection with its UUID hash before ServerHandshake runs.
	if len(out.entries) > 0 && !out.entries[0].cfg.DisableHTTPMask &&
		!strings.EqualFold(strings.TrimSpace(out.entries[0].cfg.HTTPMaskMode), "") &&
		!strings.EqualFold(strings.TrimSpace(out.entries[0].cfg.HTTPMaskMode), "legacy") {
		configs := make([]*transport.ProtocolConfig, 0, len(out.entries))
		for i := range out.entries {
			configs = append(configs, out.entries[i].cfg)
		}
		var tunnel *transport.HTTPMaskTunnelServer
		if previous != nil && sameHTTPMaskUserSet(previous, out) {
			for _, entry := range previous.entries {
				if entry.tunnel != nil {
					tunnel = entry.tunnel
					break
				}
			}
		}
		if tunnel == nil {
			tunnel = transport.NewHTTPMaskMultiUserTunnelServerWithFallback(configs)
		}
		for i := range out.entries {
			out.entries[i].tunnel = tunnel
			out.byHash[transport.KIPUserHashHexFromKey(out.entries[i].user.Uuid)] = out.entries[i]
		}
	}
	return out, nil
}

func sameHTTPMaskUserSet(previous, current *userConfigSnapshot) bool {
	if previous == nil || current == nil || len(previous.entries) != len(current.entries) {
		return false
	}
	for _, entry := range current.entries {
		old, ok := previous.byHash[transport.KIPUserHashHexFromKey(entry.user.Uuid)]
		if !ok || old.tunnel == nil || !sameSudokuProtocolConfig(old.cfg, entry.cfg) || old.tableFingerprint != entry.tableFingerprint {
			return false
		}
	}
	return true
}

func sudokuTableFingerprint(tableType, customTable string, customTables []string) string {
	return strings.Join(append([]string{tableType, strings.TrimSpace(customTable)}, customTables...), "\x00")
}

func previousUserConfig(snapshot *userConfigSnapshot, hash string) (userConfig, bool) {
	if snapshot == nil {
		return userConfig{}, false
	}
	entry, ok := snapshot.byHash[hash]
	return entry, ok
}

// closeReplacedHTTPMaskTunnels releases tunnel session reapers when a panel
// sync removes a user or changes its HTTPMask settings. Tunnels that are still
// referenced by the new immutable snapshot are retained.
func closeReplacedHTTPMaskTunnels(previous, current *userConfigSnapshot) {
	if previous == nil {
		return
	}
	active := make(map[*transport.HTTPMaskTunnelServer]struct{})
	if current != nil {
		for _, entry := range current.entries {
			if entry.tunnel != nil {
				active[entry.tunnel] = struct{}{}
			}
		}
	}
	closed := make(map[*transport.HTTPMaskTunnelServer]struct{})
	for _, entry := range previous.entries {
		if entry.tunnel == nil {
			continue
		}
		if _, ok := active[entry.tunnel]; ok {
			continue
		}
		if _, ok := closed[entry.tunnel]; ok {
			continue
		}
		closed[entry.tunnel] = struct{}{}
		_ = entry.tunnel.Close()
	}
}

func sameSudokuProtocolConfig(a, b *transport.ProtocolConfig) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Key == b.Key &&
		a.AEADMethod == b.AEADMethod &&
		a.PaddingMin == b.PaddingMin &&
		a.PaddingMax == b.PaddingMax &&
		a.EnablePureDownlink == b.EnablePureDownlink &&
		a.HandshakeTimeoutSeconds == b.HandshakeTimeoutSeconds &&
		a.DisableHTTPMask == b.DisableHTTPMask &&
		a.HTTPMaskMode == b.HTTPMaskMode &&
		a.HTTPMaskTLSEnabled == b.HTTPMaskTLSEnabled &&
		a.HTTPMaskHost == b.HTTPMaskHost &&
		a.HTTPMaskPathRoot == b.HTTPMaskPathRoot &&
		a.Multiplex == b.Multiplex &&
		a.HTTPMaskMultiplex == b.HTTPMaskMultiplex
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
	s := &serverInstance{services: services, router: router, listener: listener, ctx: ctx, cancel: cancel, done: make(chan error, 1), conns: make(map[net.Conn]struct{}), handshakeSlots: make(chan struct{}, maxConcurrentSudokuHandshakes), handshakesBySource: make(map[string]int)}
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
		s.handlers.Add(1)
		s.connMu.Lock()
		s.conns[conn] = struct{}{}
		s.connMu.Unlock()
		go func() {
			defer s.handlers.Done()
			defer func() {
				s.connMu.Lock()
				delete(s.conns, conn)
				s.connMu.Unlock()
			}()
			s.handle(conn)
		}()
	}
}

func (s *serverInstance) handle(raw net.Conn) {
	defer raw.Close()
	source := remoteSourceAddress(raw)
	if !s.acquireHandshakeSlot(source) {
		return
	}
	handshakeSlotHeld := true
	defer func() {
		if handshakeSlotHeld {
			s.releaseHandshakeSlot(source)
		}
	}()
	entry, conn, meta, err := s.handshake(raw)
	if err != nil {
		return
	}
	// Established proxy sessions must not consume the unauthenticated
	// handshake budget; release the slot as soon as authentication succeeds.
	s.releaseHandshakeSlot(source)
	handshakeSlotHeld = false
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

func (s *serverInstance) acquireHandshakeSlot(source string) bool {
	select {
	case s.handshakeSlots <- struct{}{}:
	case <-s.ctx.Done():
		return false
	default:
		// Refuse excess unauthenticated handshakes instead of allowing a burst
		// of malformed connections to consume one goroutine and a full key/table
		// probe for every user indefinitely.
		return false
	}
	if source == "" {
		return true
	}
	s.handshakeMu.Lock()
	count := s.handshakesBySource[source]
	if count >= maxConcurrentSudokuHandshakesPerSource {
		s.handshakeMu.Unlock()
		<-s.handshakeSlots
		return false
	}
	s.handshakesBySource[source] = count + 1
	s.handshakeMu.Unlock()
	return true
}

func (s *serverInstance) releaseHandshakeSlot(source string) {
	if source != "" {
		s.handshakeMu.Lock()
		if count := s.handshakesBySource[source]; count <= 1 {
			delete(s.handshakesBySource, source)
		} else {
			s.handshakesBySource[source] = count - 1
		}
		s.handshakeMu.Unlock()
	}
	<-s.handshakeSlots
}

func remoteAddress(c net.Conn) string {
	if c == nil || c.RemoteAddr() == nil {
		return ""
	}
	return c.RemoteAddr().String()
}

func remoteSourceAddress(c net.Conn) string {
	if c == nil || c.RemoteAddr() == nil {
		return ""
	}
	address := c.RemoteAddr().String()
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

// handshake tries the UUID-specific keys against one replayable byte stream. The Sudoku
// client hides the UUID hash inside the encrypted KIP hello, so this is required for raw TCP.
func (s *serverInstance) handshake(raw net.Conn) (userConfig, net.Conn, *transport.HandshakeMeta, error) {
	snapshot := s.users.Load()
	if snapshot == nil {
		return userConfig{}, nil, nil, errors.New("no users")
	}
	entries := snapshot.entries
	if len(entries) == 0 && len(snapshot.byHash) > 0 {
		// Keep compatibility with snapshots assembled by older callers/tests.
		entries = make([]userConfig, 0, len(snapshot.byHash))
		for _, e := range snapshot.byHash {
			entries = append(entries, e)
		}
	}
	handshakeBudget := 5 * time.Second
	for _, entry := range entries {
		if entry.tunnel != nil {
			// HTTPMask stream/poll authorization can legitimately need a few
			// extra round trips. HandleConn applies its own bounded header read;
			// keep the outer budget aligned with that path.
			handshakeBudget = 15 * time.Second
			break
		}
	}
	deadline := time.Now().Add(handshakeBudget)
	replay := newReplayConn(raw)
	// A tunneled HTTPMask connection may already contain the encrypted KIP
	// hello. The shared tunnel decrypts it once and tags the returned stream
	// with the UUID hash. Route directly to that user; retrying the same stream
	// through every UUID would consume the session and can create unbounded
	// HTTP/session work under a multi-user listener.
	if len(entries) > 0 && entries[0].tunnel != nil {
		wrapped, tunnelCfg, done, wrapErr := entries[0].tunnel.WrapConn(replay)
		if wrapErr != nil || done {
			if done {
				return userConfig{}, nil, nil, errHTTPMaskHandled
			}
			return userConfig{}, nil, nil, wrapErr
		}
		if wrapped != nil {
			if hash, ok := transport.EarlyHandshakeUserHash(wrapped); ok {
				entry, exists := snapshot.byHash[hash]
				if !exists || entry.cfg == nil {
					_ = wrapped.Close()
					return userConfig{}, nil, nil, errors.New("Sudoku HTTPMask user hash is not configured")
				}
				cfg := *entry.cfg
				if tunnelCfg != nil {
					cfg = *tunnelCfg
				}
				cfg.DisableHTTPMask = true
				remaining := time.Until(deadline)
				if remaining <= 0 {
					_ = wrapped.Close()
					return userConfig{}, nil, nil, errors.New("Sudoku handshake timeout")
				}
				cfg.HandshakeTimeoutSeconds = int((remaining + time.Second - 1) / time.Second)
				conn, meta, err := transport.ServerHandshake(wrapped, &cfg)
				if err != nil || meta == nil || meta.UserHash != hash {
					_ = wrapped.Close()
					if err == nil {
						err = errors.New("Sudoku HTTPMask user hash mismatch")
					}
					return userConfig{}, nil, nil, err
				}
				return entry, conn, meta, nil
			}
			if transport.HTTPMaskRejected(wrapped) {
				_ = wrapped.Close()
				return userConfig{}, nil, nil, errors.New("Sudoku HTTPMask request rejected")
			}
			// A non-early connection is either a raw fallback or an older
			// HTTPMask tunnel that performs the Sudoku handshake after upgrade.
			// It has already been inspected by the tunnel, so do not call
			// WrapConn again below.
			passThroughReplay := newReplayConn(wrapped)
			for _, entry := range entries {
				passThroughReplay.Reset()
				cfg := *entry.cfg
				cfg.HandshakeTimeoutSeconds = maxInt(1, int((time.Until(deadline)+time.Second-1)/time.Second))
				conn, meta, err := transport.ServerHandshake(passThroughReplay, &cfg)
				if err == nil && meta != nil && meta.UserHash == transport.KIPUserHashHexFromKey(entry.user.Uuid) {
					return entry, conn, meta, nil
				}
				if conn != nil {
					_ = conn.Close()
				}
			}
			return userConfig{}, nil, nil, errors.New("Sudoku handshake rejected")
		}
	}
	for _, entry := range entries {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
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
		// ServerHandshake resets the socket deadline after each attempt. Use
		// the remaining connection-wide budget so a malformed stream cannot
		// make us wait for the per-user timeout once for every UUID.
		remaining = time.Until(deadline)
		if remaining <= 0 {
			break
		}
		attemptSeconds := int((remaining + time.Second - 1) / time.Second)
		if attemptSeconds < 1 {
			attemptSeconds = 1
		}
		cfg.HandshakeTimeoutSeconds = attemptSeconds
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
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
		deadline := time.Now().Add(runtimeStopTimeout)
		acceptTimedOut := false
		select {
		case err := <-s.done:
			s.closeErr = err
		case <-time.After(time.Until(deadline)):
			s.closeErr = fmt.Errorf("%w after %s", contract.ErrRuntimeStopTimeout, runtimeStopTimeout)
			acceptTimedOut = true
		}
		s.connMu.Lock()
		active := make([]net.Conn, 0, len(s.conns))
		for conn := range s.conns {
			active = append(active, conn)
		}
		s.connMu.Unlock()
		for _, conn := range active {
			_ = conn.Close()
		}
		if snapshot := s.users.Load(); snapshot != nil {
			closedTunnels := make(map[*transport.HTTPMaskTunnelServer]struct{})
			for _, entry := range snapshot.byHash {
				if entry.tunnel == nil {
					continue
				}
				if _, alreadyClosed := closedTunnels[entry.tunnel]; alreadyClosed {
					continue
				}
				closedTunnels[entry.tunnel] = struct{}{}
				_ = entry.tunnel.Close()
			}
		}
		done := make(chan struct{})
		go func() {
			s.handlers.Wait()
			close(done)
		}()
		if acceptTimedOut {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if s.closeErr == nil {
				s.closeErr = fmt.Errorf("%w after %s", contract.ErrRuntimeStopTimeout, runtimeStopTimeout)
			}
			return
		}
		select {
		case <-done:
		case <-time.After(remaining):
			if s.closeErr == nil {
				s.closeErr = fmt.Errorf("%w after %s", contract.ErrRuntimeStopTimeout, runtimeStopTimeout)
			}
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
