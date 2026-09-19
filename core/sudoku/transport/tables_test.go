package sudoku

import "testing"

func TestServerTablesAcceptShadowrocketDefaultsAndIgnoreInvalidPattern(t *testing.T) {
	tables, err := NewServerTablesWithCustomPatterns(
		"shadowrocket-key",
		"prefer_entropy",
		"wwww",
		nil,
	)
	if err != nil {
		t.Fatalf("NewServerTablesWithCustomPatterns() error = %v", err)
	}
	if len(tables) != 3 {
		t.Fatalf("got %d server tables, want symmetric and directional compatibility candidates", len(tables))
	}
}

func TestServerTablesAcceptsURIWithoutTableType(t *testing.T) {
	server, err := NewServerTablesWithCustomPatterns("uri-key", "prefer_entropy", "", nil)
	if err != nil {
		t.Fatalf("build server tables: %v", err)
	}
	client, err := NewClientTablesWithCustomPatterns("uri-key", "prefer_ascii", "", nil)
	if err != nil {
		t.Fatalf("build client tables: %v", err)
	}
	for _, want := range client {
		found := false
		for _, candidate := range server {
			if candidate.Hint() == want.Hint() {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("server did not accept URI client table hint %d", want.Hint())
		}
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
