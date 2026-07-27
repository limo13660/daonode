package juicity

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/daeuniverse/outbound/netproxy"
	"github.com/daeuniverse/outbound/pool"
	juicityProtocol "github.com/daeuniverse/outbound/protocol/juicity"
	quic "github.com/daeuniverse/quic-go"
	"github.com/juicity/juicity/common/consts"

	"github.com/limo13660/daonode/core/shared"
)

type connectionHandle struct {
	conn quic.Connection

	mu        sync.Mutex
	endpoints map[io.Closer]struct{}
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

func newConnectionHandle(conn quic.Connection) *connectionHandle {
	return &connectionHandle{conn: conn, endpoints: make(map[io.Closer]struct{})}
}

func (h *connectionHandle) AddEndpoint(endpoint io.Closer) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.endpoints[endpoint] = struct{}{}
	return true
}

func (h *connectionHandle) RemoveEndpoint(endpoint io.Closer) {
	h.mu.Lock()
	delete(h.endpoints, endpoint)
	h.mu.Unlock()
}

func (h *connectionHandle) Close() error {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		endpoints := make([]io.Closer, 0, len(h.endpoints))
		for endpoint := range h.endpoints {
			endpoints = append(endpoints, endpoint)
		}
		h.endpoints = nil
		h.mu.Unlock()

		errs := make([]error, 0, len(endpoints)+1)
		if h.conn != nil {
			errs = append(errs, h.conn.CloseWithError(0, ""))
		}
		for _, endpoint := range endpoints {
			errs = append(errs, endpoint.Close())
		}
		h.closeErr = errors.Join(errs...)
	})
	return h.closeErr
}

type underlayAuthorization struct {
	auth    *juicityProtocol.UnderlayAuth
	session *shared.Session
	handle  *connectionHandle
}

type inFlightEntry struct {
	authorization *underlayAuthorization
	ready         chan struct{}
	readyOnce     sync.Once
	timer         *time.Timer
}

type inFlightUnderlayKey struct {
	ttl time.Duration

	mu     sync.Mutex
	items  map[[juicityProtocol.UnderlaySaltLen]byte]*inFlightEntry
	closed bool
}

func newInFlightUnderlayKey(ttl time.Duration) *inFlightUnderlayKey {
	return &inFlightUnderlayKey{
		ttl:   ttl,
		items: make(map[[juicityProtocol.UnderlaySaltLen]byte]*inFlightEntry),
	}
}

func (s *inFlightUnderlayKey) Store(key [juicityProtocol.UnderlaySaltLen]byte, authorization *underlayAuthorization) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	entry := s.items[key]
	if entry == nil {
		entry = &inFlightEntry{ready: make(chan struct{})}
		s.items[key] = entry
	}
	entry.authorization = authorization
	if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.timer = time.AfterFunc(s.ttl, func() { s.expire(key, entry) })
	entry.readyOnce.Do(func() { close(entry.ready) })
	s.mu.Unlock()
}

func (s *inFlightUnderlayKey) Evict(ctx context.Context, key [juicityProtocol.UnderlaySaltLen]byte) *underlayAuthorization {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	entry := s.items[key]
	if entry == nil {
		entry = &inFlightEntry{ready: make(chan struct{})}
		entry.timer = time.AfterFunc(s.ttl, func() { s.expire(key, entry) })
		s.items[key] = entry
	}
	ready := entry.ready
	s.mu.Unlock()

	timer := time.NewTimer(s.ttl)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
		return nil
	case <-ready:
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items[key] != entry {
		return nil
	}
	delete(s.items, key)
	if entry.timer != nil {
		entry.timer.Stop()
	}
	return entry.authorization
}

func (s *inFlightUnderlayKey) expire(key [juicityProtocol.UnderlaySaltLen]byte, entry *inFlightEntry) {
	s.mu.Lock()
	if s.items[key] == entry {
		delete(s.items, key)
		entry.readyOnce.Do(func() { close(entry.ready) })
	}
	s.mu.Unlock()
}

func (s *inFlightUnderlayKey) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	entries := make([]*inFlightEntry, 0, len(s.items))
	for key, entry := range s.items {
		delete(s.items, key)
		entries = append(entries, entry)
	}
	s.mu.Unlock()
	for _, entry := range entries {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		entry.readyOnce.Do(func() { close(entry.ready) })
	}
}

type underlayMetadata struct {
	masterKey []byte
	session   *shared.Session
}

type udpEndpoint struct {
	connection netproxy.PacketConn
	target     string
	metadata   *underlayMetadata
	handler    func([]byte, *underlayMetadata) error

	mu        sync.Mutex
	timer     *time.Timer
	onExpire  func()
	onClose   func()
	closeOnce sync.Once
	closeErr  error
}

func newUDPEndpoint(
	ctx context.Context,
	target string,
	dialer netproxy.Dialer,
	metadata *underlayMetadata,
	handler func([]byte, *underlayMetadata) error,
) (*udpEndpoint, error) {
	dialCtx, cancel := context.WithTimeout(ctx, consts.DefaultDialTimeout)
	defer cancel()
	connection, err := dialer.DialContext(dialCtx, "udp", target)
	if err != nil {
		return nil, err
	}
	packet, ok := connection.(netproxy.PacketConn)
	if !ok {
		_ = connection.Close()
		return nil, errors.New("Juicity underlay outbound does not support UDP")
	}
	endpoint := &udpEndpoint{
		connection: packet,
		target:     target,
		metadata:   metadata,
		handler:    handler,
	}
	go endpoint.readLoop()
	return endpoint, nil
}

func (e *udpEndpoint) setExpiration(callback func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onExpire = callback
	e.resetTimerLocked()
}

func (e *udpEndpoint) setCloseCallback(callback func()) {
	e.mu.Lock()
	e.onClose = callback
	e.mu.Unlock()
}

func (e *udpEndpoint) resetTimerLocked() {
	if e.timer == nil {
		e.timer = time.AfterFunc(consts.DefaultNatTimeout, func() {
			e.mu.Lock()
			callback := e.onExpire
			e.mu.Unlock()
			if callback != nil {
				callback()
			}
		})
		return
	}
	e.timer.Reset(consts.DefaultNatTimeout)
}

func (e *udpEndpoint) touch() {
	e.mu.Lock()
	e.resetTimerLocked()
	e.mu.Unlock()
}

func (e *udpEndpoint) Write(buffer []byte) (int, error) {
	e.touch()
	return e.connection.WriteTo(buffer, e.target)
}

func (e *udpEndpoint) readLoop() {
	buffer := pool.GetFullCap(consts.EthernetMtu)
	defer buffer.Put()
	for {
		n, _, err := e.connection.ReadFrom(buffer)
		if err != nil {
			return
		}
		e.touch()
		if err := e.handler(buffer[:n], e.metadata); err != nil {
			return
		}
	}
}

func (e *udpEndpoint) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		if e.timer != nil {
			e.timer.Stop()
		}
		e.onExpire = nil
		callback := e.onClose
		e.onClose = nil
		e.mu.Unlock()
		e.closeErr = e.connection.Close()
		if callback != nil {
			callback()
		}
	})
	return e.closeErr
}

type udpEndpointPool struct {
	mu       sync.Mutex
	items    map[netip.AddrPort]*udpEndpoint
	createMu map[netip.AddrPort]*sync.Mutex
	closed   bool
}

func newUDPEndpointPool() *udpEndpointPool {
	return &udpEndpointPool{
		items:    make(map[netip.AddrPort]*udpEndpoint),
		createMu: make(map[netip.AddrPort]*sync.Mutex),
	}
}

func (p *udpEndpointPool) GetOrCreate(source netip.AddrPort, create func() (*udpEndpoint, error)) (*udpEndpoint, bool, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, false, net.ErrClosed
	}
	if endpoint := p.items[source]; endpoint != nil {
		p.mu.Unlock()
		endpoint.touch()
		return endpoint, false, nil
	}
	lock := p.createMu[source]
	if lock == nil {
		lock = &sync.Mutex{}
		p.createMu[source] = lock
	}
	p.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()
	p.mu.Lock()
	defer func() {
		delete(p.createMu, source)
		p.mu.Unlock()
	}()
	if p.closed {
		return nil, false, net.ErrClosed
	}
	if endpoint := p.items[source]; endpoint != nil {
		return endpoint, false, nil
	}
	p.mu.Unlock()
	endpoint, err := create()
	p.mu.Lock()
	if err != nil {
		return nil, false, err
	}
	if p.closed {
		_ = endpoint.Close()
		return nil, false, net.ErrClosed
	}
	p.items[source] = endpoint
	endpoint.setExpiration(func() { p.Remove(source, endpoint) })
	return endpoint, true, nil
}

func (p *udpEndpointPool) Remove(source netip.AddrPort, endpoint *udpEndpoint) {
	p.mu.Lock()
	if p.items[source] == endpoint {
		delete(p.items, source)
	}
	p.mu.Unlock()
	_ = endpoint.Close()
}

func (p *udpEndpointPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	endpoints := make([]*udpEndpoint, 0, len(p.items))
	for source, endpoint := range p.items {
		delete(p.items, source)
		endpoints = append(endpoints, endpoint)
	}
	p.mu.Unlock()
	for _, endpoint := range endpoints {
		_ = endpoint.Close()
	}
}

var _ io.Closer = (*connectionHandle)(nil)
var _ io.Closer = (*udpEndpoint)(nil)
