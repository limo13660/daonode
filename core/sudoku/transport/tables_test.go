package sudoku

import "testing"

func TestServerTablesIgnoreInvalidPatternAndKeepConfiguredMode(t *testing.T) {
	tables, err := NewServerTablesWithCustomPatterns(
		"shadowrocket-key",
		"prefer_entropy",
		"wwww",
		nil,
	)
	if err != nil {
		t.Fatalf("NewServerTablesWithCustomPatterns() error = %v", err)
	}
	if len(tables) != 1 {
		t.Fatalf("got %d server tables, want one configured candidate", len(tables))
	}
}

func TestServerTablesRequireMatchingURIWithoutExplicitTableType(t *testing.T) {
	server, err := NewServerTablesWithCustomPatterns("uri-key", "prefer_entropy", "", nil)
	if err != nil {
		t.Fatalf("build server tables: %v", err)
	}
	client, err := NewClientTablesWithCustomPatterns("uri-key", "prefer_entropy", "", nil)
	if err != nil {
		t.Fatalf("build client tables: %v", err)
	}
	if len(server) != 1 || len(client) != 1 {
		t.Fatalf("server tables=%d client tables=%d, want one each", len(server), len(client))
	}
	if server[0].Hint() != client[0].Hint() {
		t.Fatalf("server and client selected different table hints: %d != %d", server[0].Hint(), client[0].Hint())
	}
}

func TestServerTablesKeepExplicitCustomPatterns(t *testing.T) {
	tables, err := NewServerTablesWithCustomPatterns(
		"explicit-key",
		"prefer_entropy",
		"xpxvvpvv",
		[]string{"xpxvvpvv", "vxpvxvvp", "invalid"},
	)
	if err != nil {
		t.Fatalf("NewServerTablesWithCustomPatterns() error = %v", err)
	}
	// The server adds the built-in table before explicit entropy patterns so
	// clients can rotate tables without a lock-step panel update.
	if len(tables) != 3 {
		t.Fatalf("got %d server tables, want built-in plus two explicit patterns", len(tables))
	}
}
