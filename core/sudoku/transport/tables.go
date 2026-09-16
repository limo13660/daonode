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

	// Shadowrocket's sudoku:// URI does not carry the ASCII/table preference.
	// When the panel has no custom table, accept the two built-in defaults so
	// existing nodes configured as prefer_ascii can also accept URI clients
	// whose implementation defaults to prefer_entropy (and vice versa).
	if strings.TrimSpace(customTable) == "" && len(customTables) == 0 {
		fallbackType := ""
		switch strings.ToLower(strings.TrimSpace(tableType)) {
		case "prefer_ascii":
			fallbackType = "prefer_entropy"
		case "prefer_entropy":
			fallbackType = "prefer_ascii"
		}
		if fallbackType != "" {
			fallback, fallbackErr := NewTableWithCustom(key, fallbackType, "")
			if fallbackErr != nil {
				return nil, fallbackErr
			}
			tables = append(tables, fallback)
		}
	}
	return tables, nil
}
