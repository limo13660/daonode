package sudoku

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/limo13660/daonode/core/sudoku/transport/obfs/httpmask"
)

type HTTPMaskTunnelServer struct {
	cfg        *ProtocolConfig
	ts         *httpmask.TunnelServer
	byUserHash map[string]*ProtocolConfig
}

// EarlyHandshakeUserHash exposes the UUID tag attached to a connection after
// an HTTPMask authorize/upgrade request has carried the encrypted Sudoku hello.
func EarlyHandshakeUserHash(conn net.Conn) (string, bool) {
	return httpmask.EarlyHandshakeUserHash(conn)
}

// HTTPMaskRejected reports whether a recognized tunnel request failed its
// mode, path, token or early-handshake validation.
func HTTPMaskRejected(conn net.Conn) bool {
	return httpmask.IsRejectedConnection(conn)
}

func newHTTPMaskEarlyCodecConfig(cfg *ProtocolConfig, psk string) EarlyCodecConfig {
	return EarlyCodecConfig{
		PSK:                psk,
		AEAD:               cfg.AEADMethod,
		EnablePureDownlink: cfg.EnablePureDownlink,
		PaddingMin:         cfg.PaddingMin,
		PaddingMax:         cfg.PaddingMax,
		ProbeLimiter:       cfg.HandshakeProbeLimiter,
		ProbeTimeout:       time.Duration(cfg.HandshakeTimeoutSeconds) * time.Second,
	}
}

func newClientHTTPMaskEarlyHandshake(cfg *ProtocolConfig) (*httpmask.ClientEarlyHandshake, error) {
	choice, err := pickClientTable(cfg)
	if err != nil {
		return nil, err
	}

	return NewHTTPMaskClientEarlyHandshake(
		newHTTPMaskEarlyCodecConfig(cfg, ClientAEADSeed(cfg.Key)),
		choice.Table,
		choice.Hint,
		choice.HasHint,
		kipUserHashFromKey(cfg.Key),
		KIPFeatAll,
	)
}

func NewHTTPMaskTunnelServer(cfg *ProtocolConfig) *HTTPMaskTunnelServer {
	return newHTTPMaskTunnelServer(cfg, false)
}

func NewHTTPMaskTunnelServerWithFallback(cfg *ProtocolConfig) *HTTPMaskTunnelServer {
	return newHTTPMaskTunnelServer(cfg, true)
}

// NewHTTPMaskMultiUserTunnelServerWithFallback creates one HTTP tunnel
// listener for a set of UUID-specific Sudoku configurations. The early
// handshake is tried against the keys in memory before a tunnel session is
// created, so a connection is never opened in every user's tunnel while the
// server searches for the matching UUID.
func NewHTTPMaskMultiUserTunnelServerWithFallback(configs []*ProtocolConfig) *HTTPMaskTunnelServer {
	return newHTTPMaskMultiUserTunnelServer(configs, true)
}

func newHTTPMaskMultiUserTunnelServer(configs []*ProtocolConfig, passThroughOnReject bool) *HTTPMaskTunnelServer {
	if len(configs) == 0 {
		return &HTTPMaskTunnelServer{}
	}
	base := configs[0]
	if base == nil {
		return &HTTPMaskTunnelServer{}
	}
	byUserHash := make(map[string]*ProtocolConfig, len(configs))
	orderedConfigs := make([]*ProtocolConfig, 0, len(configs))
	for _, cfg := range configs {
		if cfg == nil || cfg.DisableHTTPMask || strings.EqualFold(strings.TrimSpace(cfg.HTTPMaskMode), "") || strings.EqualFold(strings.TrimSpace(cfg.HTTPMaskMode), "legacy") {
			continue
		}
		byUserHash[KIPUserHashHexFromKey(cfg.Key)] = cfg
		orderedConfigs = append(orderedConfigs, cfg)
	}
	if len(byUserHash) == 0 {
		return &HTTPMaskTunnelServer{cfg: base}
	}

	// Stream/poll do not need a second HTTP auth layer. WebSocket clients send
	// the per-key auth token, but the Sudoku early payload is the authoritative
	// credential when several UUIDs share one listener, so leave AuthKey empty.
	var preferredMu sync.Mutex
	preferredHash := ""
	early := &httpmask.TunnelServerEarlyHandshake{Prepare: func(payload []byte) (*httpmask.PreparedServerEarlyHandshake, error) {
		var firstErr error
		preferredMu.Lock()
		preferred := preferredHash
		preferredMu.Unlock()
		candidates := orderedConfigs
		if preferred != "" {
			if preferredCfg := byUserHash[preferred]; preferredCfg != nil {
				candidates = make([]*ProtocolConfig, 0, len(orderedConfigs))
				candidates = append(candidates, preferredCfg)
				for _, cfg := range orderedConfigs {
					if cfg != preferredCfg {
						candidates = append(candidates, cfg)
					}
				}
			}
		}
		failed := make([]*ProtocolConfig, 0)
		for _, cfg := range candidates {
			prepared, err := NewHTTPMaskServerEarlyHandshake(
				newHTTPMaskEarlyCodecConfig(cfg, ServerAEADSeed(cfg.Key)),
				cfg.tableCandidates(),
				globalHandshakeReplay.allow,
			).Prepare(payload)
			if err == nil {
				if prepared != nil && prepared.UserHash != "" {
					preferredMu.Lock()
					preferredHash = prepared.UserHash
					preferredMu.Unlock()
				}
				return prepared, nil
			}
			if cfg.TableFallbackProvider != nil {
				if cfg.TableProvider != nil {
					cfg.TableProvider.Release()
				}
				failed = append(failed, cfg)
			} else {
				cfg.ReleaseTableCandidates()
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		// Only build fallback layouts after all primary layouts have failed;
		// otherwise every wrong UUID would pay the compatibility-table cost.
		for _, cfg := range failed {
			fallback, err := cfg.TableFallbackProvider.Tables()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			prepared, err := NewHTTPMaskServerEarlyHandshake(
				newHTTPMaskEarlyCodecConfig(cfg, ServerAEADSeed(cfg.Key)),
				fallback,
				globalHandshakeReplay.allow,
			).Prepare(payload)
			if err == nil {
				if prepared != nil && prepared.UserHash != "" {
					preferredMu.Lock()
					preferredHash = prepared.UserHash
					preferredMu.Unlock()
				}
				return prepared, nil
			}
			cfg.TableFallbackProvider.Release()
			if firstErr == nil {
				firstErr = err
			}
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("no Sudoku user matched HTTPMask early handshake")
		}
		return nil, firstErr
	}}
	ts := httpmask.NewTunnelServer(httpmask.TunnelServerOptions{
		Mode:                base.HTTPMaskMode,
		PathRoot:            base.HTTPMaskPathRoot,
		EarlyHandshake:      early,
		PassThroughOnReject: passThroughOnReject,
	})
	return &HTTPMaskTunnelServer{cfg: base, ts: ts, byUserHash: byUserHash}
}

func newHTTPMaskTunnelServer(cfg *ProtocolConfig, passThroughOnReject bool) *HTTPMaskTunnelServer {
	if cfg == nil {
		return &HTTPMaskTunnelServer{}
	}

	var ts *httpmask.TunnelServer
	if !cfg.DisableHTTPMask {
		switch strings.ToLower(strings.TrimSpace(cfg.HTTPMaskMode)) {
		case "stream", "poll", "auto", "ws":
			ts = httpmask.NewTunnelServer(httpmask.TunnelServerOptions{
				Mode:     cfg.HTTPMaskMode,
				PathRoot: cfg.HTTPMaskPathRoot,
				AuthKey:  ServerAEADSeed(cfg.Key),
				EarlyHandshake: NewHTTPMaskServerEarlyHandshake(
					newHTTPMaskEarlyCodecConfig(cfg, ServerAEADSeed(cfg.Key)),
					cfg.tableCandidates(),
					globalHandshakeReplay.allow,
				),
				// When upstream fallback is enabled, preserve rejected HTTP requests for the caller.
				PassThroughOnReject: passThroughOnReject,
			})
		}
	}
	return &HTTPMaskTunnelServer{cfg: cfg, ts: ts}
}

// Close terminates active HTTPMask sessions owned by this runtime.
func (s *HTTPMaskTunnelServer) Close() error {
	if s == nil || s.ts == nil {
		return nil
	}
	return s.ts.Close()
}

// WrapConn inspects an accepted TCP connection and upgrades it to an HTTP tunnel stream when needed.
//
// Returns:
//   - done=true: this TCP connection has been fully handled (e.g., stream/poll control request), caller should return
//   - done=false: handshakeConn+cfg are ready for ServerHandshake
func (s *HTTPMaskTunnelServer) WrapConn(rawConn net.Conn) (handshakeConn net.Conn, cfg *ProtocolConfig, done bool, err error) {
	if rawConn == nil {
		return nil, nil, true, fmt.Errorf("nil conn")
	}
	if s == nil {
		return rawConn, nil, false, nil
	}
	if s.ts == nil {
		return rawConn, s.cfg, false, nil
	}

	res, c, err := s.ts.HandleConn(rawConn)
	if err != nil {
		return nil, nil, true, err
	}

	switch res {
	case httpmask.HandleDone:
		return nil, nil, true, nil
	case httpmask.HandlePassThrough:
		return c, s.cfg, false, nil
	case httpmask.HandleStartTunnel:
		selected := s.cfg
		if userHash, ok := httpmask.EarlyHandshakeUserHash(c); ok && s.byUserHash != nil {
			selected = s.byUserHash[userHash]
			if selected == nil {
				_ = c.Close()
				return nil, nil, true, fmt.Errorf("unknown Sudoku HTTPMask user hash")
			}
		}
		if selected == nil {
			_ = c.Close()
			return nil, nil, true, fmt.Errorf("missing Sudoku HTTPMask configuration")
		}
		inner := *selected
		inner.DisableHTTPMask = true
		// HTTPMask tunnel modes (stream/poll/auto/ws) add extra round trips before the first
		// handshake bytes can reach ServerHandshake, especially under high concurrency.
		// Bump the handshake timeout for tunneled conns to avoid flaky timeouts while keeping
		// the default strict for raw TCP handshakes.
		const minTunneledHandshakeTimeoutSeconds = 15
		if inner.HandshakeTimeoutSeconds <= 0 || inner.HandshakeTimeoutSeconds < minTunneledHandshakeTimeoutSeconds {
			inner.HandshakeTimeoutSeconds = minTunneledHandshakeTimeoutSeconds
		}
		return c, &inner, false, nil
	default:
		return nil, nil, true, nil
	}
}

type TunnelDialer func(ctx context.Context, network, addr string) (net.Conn, error)

// DialHTTPMaskTunnel dials a CDN-capable HTTP tunnel (stream/poll/auto/ws) and returns a stream carrying raw Sudoku bytes.
func DialHTTPMaskTunnel(ctx context.Context, serverAddress string, cfg *ProtocolConfig, dial TunnelDialer, upgrade func(net.Conn) (net.Conn, error)) (net.Conn, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	if cfg.DisableHTTPMask {
		return nil, fmt.Errorf("http mask is disabled")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.HTTPMaskMode)) {
	case "stream", "poll", "auto", "ws":
	default:
		return nil, fmt.Errorf("http-mask-mode=%q does not use http tunnel", cfg.HTTPMaskMode)
	}
	var (
		earlyHandshake *httpmask.ClientEarlyHandshake
		err            error
	)
	if upgrade != nil {
		earlyHandshake, err = newClientHTTPMaskEarlyHandshake(cfg)
		if err != nil {
			return nil, err
		}
	}
	return httpmask.DialTunnel(ctx, serverAddress, httpmask.TunnelDialOptions{
		Mode:           cfg.HTTPMaskMode,
		TLSEnabled:     cfg.HTTPMaskTLSEnabled,
		HostOverride:   cfg.HTTPMaskHost,
		PathRoot:       cfg.HTTPMaskPathRoot,
		AuthKey:        ClientAEADSeed(cfg.Key),
		EarlyHandshake: earlyHandshake,
		Upgrade:        upgrade,
		DialContext:    dial,
	})
}
