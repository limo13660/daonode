package sudoku

import (
	"sync"
	"testing"

	obfssudoku "github.com/limo13660/daonode/core/sudoku/transport/obfs/sudoku"
)

func TestTableProviderBuildsOnceConcurrently(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	provider := NewTableProvider(func() ([]*obfssudoku.Table, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil, nil
	})
	// An empty result is rejected, but it must still be cached so a failed
	// malformed configuration cannot trigger an unbounded rebuild loop.
	if _, err := provider.Tables(); err == nil {
		t.Fatal("expected empty table result to fail")
	}
	if _, err := provider.Tables(); err == nil {
		t.Fatal("expected cached empty table result to fail")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider called %d times, want 1", calls)
	}
}
