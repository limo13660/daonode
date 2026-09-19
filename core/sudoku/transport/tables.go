package sudoku

import (
	"strings"

	"github.com/limo13660/daonode/core/sudoku/transport/obfs/sudoku"
)

func normalizeCustomPatterns(customTable string, customTables []string) []string {
	patterns := customTables
	if len(patterns) == 0 && strings.TrimSpace(customTable) != "" {
		patterns = []string{customTable}
	}
	if len(patterns) == 0 {
		patterns = []string{""}
	}
	return patterns
}

func normalizeTablePatterns(tableType string, customTable string, customTables []string) ([]string, error) {
	patterns := normalizeCustomPatterns(customTable, customTables)
	if _, err := sudoku.ParseASCIIMode(tableType); err != nil {
		return nil, err
	}
	return patterns, nil
}

// NewTablesWithCustomPatterns builds one or more obfuscation tables from x/v/p custom patterns.
// When customTables is non-empty it overrides customTable (matching upstream Sudoku behavior).
//
// Deprecated-ish: prefer NewClientTablesWithCustomPatterns / NewServerTablesWithCustomPatterns.
func NewTablesWithCustomPatterns(key string, tableType string, customTable string, customTables []string) ([]*sudoku.Table, error) {
	patterns, err := normalizeTablePatterns(tableType, customTable, customTables)
	if err != nil {
		return nil, err
	}
	tables := make([]*sudoku.Table, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		t, err := NewTableWithCustom(key, tableType, pattern)
		if err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	return tables, nil
}

func NewClientTablesWithCustomPatterns(key string, tableType string, customTable string, customTables []string) ([]*sudoku.Table, error) {
	return NewTablesWithCustomPatterns(key, tableType, customTable, customTables)
}

// NewServerTablesWithCustomPatterns matches upstream server behavior: when probeable custom table
// rotation is enabled, also accept the default table to avoid forcing clients to update in lockstep.
func NewServerTablesWithCustomPatterns(key string, tableType string, customTable string, customTables []string) ([]*sudoku.Table, error) {
	// A legacy panel may contain a stale value such as "wwww". Do not make
	// every incoming handshake rebuild and reject an invalid table set; the
	// server can safely fall back to its built-in table and the defaults used
	// by Shadowrocket's sudoku:// importer.
	customTable = normalizeServerCustomPattern(customTable)
	customTables = normalizeServerCustomPatterns(customTables)
	patterns, err := normalizeTablePatterns(tableType, customTable, customTables)
	if err != nil {
		return nil, err
	}
	asciiMode, err := sudoku.ParseASCIIMode(tableType)
	if err != nil {
		return nil, err
	}
	if asciiMode.Uplink == "entropy" && len(patterns) > 0 && strings.TrimSpace(patterns[0]) != "" {
		patterns = append([]string{""}, patterns...)
	}
	tables, err := NewTablesWithCustomPatterns(key, tableType, "", patterns)
	if err != nil {
		return nil, err
	}

	// Do not silently expand an unconfigured server into several table modes.
	// A table contains a large DecodeMap; doing this for every UUID during a
	// multi-user probe multiplies the allocation by the number of panel users
	// and can exhaust a small node. Official Sudoku clients carry their table
	// preference in the configuration/URI (and default to prefer_entropy), so
	// the panel's selected mode is the only implicit candidate. Explicit
	// custom_tables remain supported above and are intentionally probeable.
	return tables, nil
}

func normalizeServerCustomPattern(pattern string) string {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if !isValidCustomPattern(pattern) {
		return ""
	}
	return pattern
}

func normalizeServerCustomPatterns(patterns []string) []string {
	result := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		if normalized := normalizeServerCustomPattern(pattern); normalized != "" {
			result = append(result, normalized)
		}
	}
	return result
}

func isValidCustomPattern(pattern string) bool {
	if len(pattern) != 8 {
		return false
	}
	return strings.Count(pattern, "x") == 2 &&
		strings.Count(pattern, "p") == 2 &&
		strings.Count(pattern, "v") == 4 &&
		strings.Trim(pattern, "xpv") == ""
}
