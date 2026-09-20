package sudoku

import (
	"bufio"
	"bytes"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/limo13660/daonode/core/sudoku/transport/crypto"
	"github.com/limo13660/daonode/core/sudoku/transport/obfs/sudoku"
)

type clientTableChoice struct {
	Table   *sudoku.Table
	Hint    uint32
	HasHint bool
}

// tableProbeChoice records both pieces of the obfuscation prelude. Sudoku
// clients use the same table with either the classic byte-oriented uplink or
// the packed 6-bit uplink; the server must detect both before decrypting KIP.
type tableProbeChoice struct {
	Table        *sudoku.Table
	PackedUplink bool
}

func pickClientTable(cfg *ProtocolConfig) (clientTableChoice, error) {
	candidates := cfg.tableCandidates()
	if len(candidates) == 0 {
		return clientTableChoice{}, fmt.Errorf("no table configured")
	}
	if len(candidates) == 1 {
		// Carry the hint even for a single directional table. Its uplink bytes
		// are indistinguishable from a symmetric table during server probing,
		// while the hint identifies the required downlink direction.
		return clientTableChoice{Table: candidates[0], Hint: candidates[0].Hint(), HasHint: true}, nil
	}
	var b [1]byte
	if _, err := crand.Read(b[:]); err != nil {
		return clientTableChoice{}, fmt.Errorf("random table pick failed: %w", err)
	}
	idx := int(b[0]) % len(candidates)
	return clientTableChoice{Table: candidates[idx], Hint: candidates[idx].Hint(), HasHint: true}, nil
}

type readOnlyConn struct {
	*bytes.Reader
}

func (c *readOnlyConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (c *readOnlyConn) Close() error                     { return nil }
func (c *readOnlyConn) LocalAddr() net.Addr              { return nil }
func (c *readOnlyConn) RemoteAddr() net.Addr             { return nil }
func (c *readOnlyConn) SetDeadline(time.Time) error      { return nil }
func (c *readOnlyConn) SetReadDeadline(time.Time) error  { return nil }
func (c *readOnlyConn) SetWriteDeadline(time.Time) error { return nil }

func drainBuffered(r *bufio.Reader) ([]byte, error) {
	n := r.Buffered()
	if n <= 0 {
		return nil, nil
	}
	out := make([]byte, n)
	_, err := io.ReadFull(r, out)
	return out, err
}

func probeHandshakeBytes(probe []byte, cfg *ProtocolConfig, table *sudoku.Table, packedUplink bool) error {
	rc := &readOnlyConn{Reader: bytes.NewReader(probe)}
	_, obfsConn := buildServerObfsConn(rc, cfg, table, false, packedUplink)
	seed := ServerAEADSeed(cfg.Key)
	pskC2S, pskS2C := derivePSKDirectionalBases(seed)
	// Server side: recv is client->server, send is server->client.
	cConn, err := crypto.NewRecordConn(obfsConn, cfg.AEADMethod, pskS2C, pskC2S)
	if err != nil {
		return err
	}

	msg, err := ReadKIPMessage(cConn)
	if err != nil {
		return err
	}
	if msg.Type != KIPTypeClientHello {
		return fmt.Errorf("unexpected handshake message: %d", msg.Type)
	}
	ch, err := DecodeKIPClientHelloPayload(msg.Payload)
	if err != nil {
		return err
	}
	if absInt64(time.Now().Unix()-ch.Timestamp.Unix()) > int64(kipHandshakeSkew.Seconds()) {
		return fmt.Errorf("time skew/replay")
	}

	return nil
}

func selectTableByProbe(r *bufio.Reader, cfg *ProtocolConfig, tables []*sudoku.Table) (tableProbeChoice, []byte, error) {
	const (
		maxProbeBytes = 64 * 1024
		readChunk     = 4 * 1024
		// A complete KIP ClientHello is substantially smaller than this on the
		// wire even with the packed uplink. Once this much input has been
		// received, a candidate that still reports EOF is a malformed or
		// wrong-key stream. Treat it as a definitive mismatch instead of
		// waiting for the socket deadline. This matters for multi-user raw TCP:
		// probing an incorrect UUID must not hold the listener for five seconds.
		minDefinitiveProbeBytes = 256
	)
	if len(tables) == 0 {
		return tableProbeChoice{}, nil, fmt.Errorf("no table candidates")
	}
	if len(tables) > 255 {
		return tableProbeChoice{}, nil, fmt.Errorf("too many table candidates: %d", len(tables))
	}

	// Copy so we can prune candidates without mutating the caller slice.
	candidates := make([]tableProbeChoice, 0, len(tables)*2)
	for _, t := range tables {
		if t != nil {
			candidates = append(candidates,
				tableProbeChoice{Table: t},
				tableProbeChoice{Table: t, PackedUplink: true},
			)
		}
	}
	if len(candidates) == 0 {
		return tableProbeChoice{}, nil, fmt.Errorf("no table candidates")
	}

	probe, err := drainBuffered(r)
	if err != nil {
		return tableProbeChoice{}, nil, fmt.Errorf("drain buffered bytes failed: %w", err)
	}

	tmp := make([]byte, readChunk)
	for {
		// Once only one candidate remains, let the full handshake validate the
		// key. A client may send only the first KIP record and wait for the
		// response; probing it again here can wait for bytes that will not be
		// sent until the server replies.
		if len(candidates) == 1 {
			tail, err := drainBuffered(r)
			if err != nil {
				return tableProbeChoice{}, nil, fmt.Errorf("drain buffered bytes failed: %w", err)
			}
			probe = append(probe, tail...)
			return tableProbeChoice{Table: candidates[0].Table, PackedUplink: candidates[0].PackedUplink}, probe, nil
		}

		needMore := false
		next := candidates[:0]
		for _, candidate := range candidates {
			err := probeHandshakeBytes(probe, cfg, candidate.Table, candidate.PackedUplink)
			if err == nil {
				tail, err := drainBuffered(r)
				if err != nil {
					return tableProbeChoice{}, nil, fmt.Errorf("drain buffered bytes failed: %w", err)
				}
				probe = append(probe, tail...)
				return tableProbeChoice{Table: candidate.Table, PackedUplink: candidate.PackedUplink}, probe, nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				if len(probe) >= minDefinitiveProbeBytes {
					continue
				}
				needMore = true
				next = append(next, candidate)
			}
			// Definitive mismatch: drop table.
		}
		candidates = next

		if len(candidates) == 0 || !needMore {
			return tableProbeChoice{}, probe, fmt.Errorf("handshake table selection failed")
		}
		if len(probe) >= maxProbeBytes {
			return tableProbeChoice{}, probe, fmt.Errorf("handshake probe exceeded %d bytes", maxProbeBytes)
		}

		n, err := r.Read(tmp)
		if n > 0 {
			probe = append(probe, tmp[:n]...)
		}
		if err != nil {
			return tableProbeChoice{}, probe, fmt.Errorf("handshake probe read failed: %w", err)
		}
	}
}
