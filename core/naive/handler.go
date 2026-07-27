package naive

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	forwardproxy "github.com/caddyserver/forwardproxy"

	panel "github.com/limo13660/daonode/api/v2board"
	"github.com/limo13660/daonode/core/shared"
)

type authSnapshot struct {
	handler *forwardproxy.Handler
	users   map[string]panel.UserInfo
	routes  *routePolicy
}

func newAuthSnapshot(info *panel.NodeInfo, users map[int]panel.UserInfo) (*authSnapshot, error) {
	if err := validateNodeInfo(info); err != nil {
		return nil, err
	}
	routes, acl, err := compileRoutePolicy(info.Common.Routes)
	if err != nil {
		return nil, fmt.Errorf("configure Naive routes: %w", err)
	}
	handler := &forwardproxy.Handler{
		HideIP:          true,
		HideVia:         true,
		ProbeResistance: &forwardproxy.ProbeResistance{},
		ACL:             acl,
	}
	byCredential := make(map[string]panel.UserInfo, len(users))
	for _, user := range users {
		username := panel.BuildPanelUserName(info.Common.EffectivePanelIdentifier(info.Id), user.Id)
		credential := forwardproxy.EncodeAuthCredentials(username, user.Uuid)
		handler.AuthCredentials = append(handler.AuthCredentials, credential)
		byCredential[string(credential)] = user
	}
	base := caddy.Context{Context: context.Background()}
	ctx, cancel := caddy.NewContext(base)
	defer cancel()
	if err := handler.Provision(ctx); err != nil {
		return nil, fmt.Errorf("provision official Naive forward proxy: %w", err)
	}
	return &authSnapshot{handler: handler, users: byCredential, routes: routes}, nil
}

type proxyHandler struct {
	services *shared.RuntimeServices
	snapshot atomic.Pointer[authSnapshot]
}

func (h *proxyHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	snapshot := h.snapshot.Load()
	if snapshot == nil {
		http.NotFound(writer, request)
		return
	}
	request = caddyhttp.PrepareRequest(request, caddy.NewReplacer(), writer, nil)
	user, authenticated := snapshot.authenticate(request)
	if !authenticated {
		h.serveOfficial(snapshot, writer, request)
		return
	}
	if snapshot.routes.blocked(request) {
		http.Error(writer, "destination is blocked", http.StatusForbidden)
		return
	}

	closers := newCloserGroup(request.Body)
	session, accepted := h.services.OpenSession(user, closers, request.RemoteAddr, true)
	if !accepted {
		http.NotFound(writer, request)
		return
	}
	defer session.Release()
	request.Body = &accountedBody{ReadCloser: request.Body, session: session}
	accountedWriter := &accountedResponseWriter{
		ResponseWriter: writer,
		session:        session,
		closers:        closers,
	}
	h.serveOfficial(snapshot, accountedWriter, request)
}

func (h *proxyHandler) serveOfficial(snapshot *authSnapshot, writer http.ResponseWriter, request *http.Request) {
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		http.NotFound(w, r)
		return nil
	})
	err := snapshot.handler.ServeHTTP(writer, request, next)
	if err == nil {
		return
	}
	var handlerErr caddyhttp.HandlerError
	if errors.As(err, &handlerErr) {
		http.Error(writer, handlerErr.Error(), handlerErr.StatusCode)
		return
	}
	http.Error(writer, err.Error(), http.StatusBadGateway)
}

func (s *authSnapshot) authenticate(request *http.Request) (panel.UserInfo, bool) {
	parts := strings.Fields(request.Header.Get("Proxy-Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "basic") {
		return panel.UserInfo{}, false
	}
	user, ok := s.users[parts[1]]
	return user, ok
}

type accountedBody struct {
	io.ReadCloser
	session *shared.Session
}

func (r *accountedBody) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	r.session.WaitUpload(int64(n))
	r.session.RecordUpload(int64(n))
	return n, err
}

type accountedResponseWriter struct {
	http.ResponseWriter
	session *shared.Session
	closers *closerGroup
}

func (w *accountedResponseWriter) Write(buffer []byte) (int, error) {
	w.session.WaitDownload(int64(len(buffer)))
	n, err := w.ResponseWriter.Write(buffer)
	w.session.RecordDownload(int64(n))
	return n, err
}

func (w *accountedResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *accountedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffered, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.closers.Add(conn)
	return &accountedClientConn{Conn: conn, session: w.session}, buffered, nil
}

type accountedClientConn struct {
	net.Conn
	session *shared.Session
}

func (c *accountedClientConn) Read(buffer []byte) (int, error) {
	n, err := c.Conn.Read(buffer)
	c.session.WaitUpload(int64(n))
	c.session.RecordUpload(int64(n))
	return n, err
}

func (c *accountedClientConn) Write(buffer []byte) (int, error) {
	c.session.WaitDownload(int64(len(buffer)))
	n, err := c.Conn.Write(buffer)
	c.session.RecordDownload(int64(n))
	return n, err
}

type closerGroup struct {
	mu      sync.Mutex
	closers []io.Closer
	closed  bool
}

func newCloserGroup(closers ...io.Closer) *closerGroup {
	g := &closerGroup{}
	for _, closer := range closers {
		g.Add(closer)
	}
	return g
}

func (g *closerGroup) Add(closer io.Closer) {
	if closer == nil {
		return
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		_ = closer.Close()
		return
	}
	g.closers = append(g.closers, closer)
	g.mu.Unlock()
}

func (g *closerGroup) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	closers := append([]io.Closer(nil), g.closers...)
	g.closers = nil
	g.mu.Unlock()
	var err error
	for _, closer := range closers {
		err = errors.Join(err, closer.Close())
	}
	return err
}

var _ http.Handler = (*proxyHandler)(nil)
var _ http.Hijacker = (*accountedResponseWriter)(nil)
var _ net.Conn = (*accountedClientConn)(nil)
