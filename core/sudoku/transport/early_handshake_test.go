package sudoku

import (
	"testing"

	sudokuobfs "github.com/limo13660/daonode/core/sudoku/transport/obfs/sudoku"
)

func TestProcessEarlyClientPayloadRejectsUnexpectedUserHash(t *testing.T) {
	table := sudokuobfs.NewTable("early-handshake-test", "prefer_entropy")
	var userHash [kipHelloUserHashSize]byte
	userHash[0] = 1
	clientCfg := EarlyCodecConfig{PSK: "early-handshake-psk", AEAD: "none"}
	client, err := NewEarlyClientState(clientCfg, table, table.Hint(), true, userHash, KIPFeatAll)
	if err != nil {
		t.Fatalf("build early client state: %v", err)
	}

	serverCfg := clientCfg
	serverCfg.ExpectedUserHash = "0000000000000000"
	if _, err := ProcessEarlyClientPayload(serverCfg, []*sudokuobfs.Table{table}, client.RequestPayload, nil); err == nil {
		t.Fatal("unexpected user hash was accepted")
	}
}
