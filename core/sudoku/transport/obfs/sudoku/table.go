package sudoku

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/rand"
	"strings"
	"sync"
)

var (
	ErrInvalidSudokuMapMiss = errors.New("INVALID_SUDOKU_MAP_MISS")
)

type Table struct {
	EncodeTable [256][][4]byte
	DecodeMap   map[uint32]byte
	PaddingPool []byte
	IsASCII     bool // 标记当前模式
	layout      *byteLayout
	opposite    *Table
	hint        uint32
}

// The set of valid 4x4 grids and the clue combinations that uniquely identify
// each grid are independent of the user key and byte layout. Building them for
// every UUID made node startup/reload CPU grow linearly with the user count.
// Build the immutable data once per process and reuse it for all tables.
var sudokuTableData struct {
	once            sync.Once
	grids           []Grid
	uniquePositions map[Grid][][]int
}

func sharedSudokuTableData() ([]Grid, map[Grid][][]int) {
	sudokuTableData.once.Do(func() {
		grids := GenerateAllGrids()
		combinations := make([][]int, 0, 1820)
		var combine func(int, int, []int)
		combine = func(start, left int, current []int) {
			if left == 0 {
				positions := make([]int, len(current))
				copy(positions, current)
				combinations = append(combinations, positions)
				return
			}
			for i := start; i <= 16-left; i++ {
				combine(i+1, left-1, append(current, i))
			}
		}
		combine(0, 4, nil)

		unique := make(map[Grid][][]int, len(grids))
		for _, target := range grids {
			positions := make([][]int, 0, len(combinations))
			for _, candidate := range combinations {
				matchCount := 0
				for _, grid := range grids {
					match := true
					for _, pos := range candidate {
						if grid[pos] != target[pos] {
							match = false
							break
						}
					}
					if match {
						matchCount++
						if matchCount > 1 {
							break
						}
					}
				}
				if matchCount == 1 {
					positions = append(positions, candidate)
				}
			}
			unique[target] = positions
		}
		sudokuTableData.grids = grids
		sudokuTableData.uniquePositions = unique
	})
	return sudokuTableData.grids, sudokuTableData.uniquePositions
}

// NewTable initializes the obfuscation tables with built-in layouts.
// Equivalent to calling NewTableWithCustom(key, mode, "").
func NewTable(key string, mode string) *Table {
	t, err := NewTableWithCustom(key, mode, "")
	if err != nil {
		panic(err)
	}
	return t
}

// NewTableWithCustom initializes the uplink/probe Sudoku table using either predefined
// or directional layouts. Directional modes such as "up_ascii_down_entropy" return the
// client->server table and internally attach the opposite direction table for runtime use.
// The customPattern must contain 8 characters with exactly 2 x, 2 p, and 4 v (case-insensitive).
func NewTableWithCustom(key string, mode string, customPattern string) (*Table, error) {
	asciiMode, err := ParseASCIIMode(mode)
	if err != nil {
		return nil, err
	}

	uplinkPattern := customPatternForToken(asciiMode.Uplink, customPattern)
	downlinkPattern := customPatternForToken(asciiMode.Downlink, customPattern)
	hint := tableHintFingerprint(key, asciiMode.Canonical(), uplinkPattern, downlinkPattern)

	uplink, err := newSingleDirectionTable(key, asciiMode.uplinkPreference(), uplinkPattern)
	if err != nil {
		return nil, err
	}
	uplink.hint = hint
	if asciiMode.Uplink == asciiMode.Downlink {
		uplink.opposite = uplink
		return uplink, nil
	}

	downlink, err := newSingleDirectionTable(key, asciiMode.downlinkPreference(), downlinkPattern)
	if err != nil {
		return nil, err
	}
	downlink.hint = hint
	uplink.opposite = downlink
	downlink.opposite = uplink
	return uplink, nil
}

func newSingleDirectionTable(key string, mode string, customPattern string) (*Table, error) {
	layout, err := resolveLayout(mode, customPattern)
	if err != nil {
		return nil, err
	}

	t := &Table{
		DecodeMap: make(map[uint32]byte),
		IsASCII:   layout.name == "ascii",
		layout:    layout,
	}
	t.PaddingPool = append(t.PaddingPool, layout.paddingPool...)

	// 生成数独网格及唯一线索组合。它们与 key/layout 无关，进程内复用。
	allGrids, uniquePositions := sharedSudokuTableData()
	h := sha256.New()
	h.Write([]byte(key))
	seed := int64(binary.BigEndian.Uint64(h.Sum(nil)[:8]))
	rng := rand.New(rand.NewSource(seed))

	shuffledGrids := make([]Grid, 288)
	copy(shuffledGrids, allGrids)
	rng.Shuffle(len(shuffledGrids), func(i, j int) {
		shuffledGrids[i], shuffledGrids[j] = shuffledGrids[j], shuffledGrids[i]
	})

	// 构建映射表
	for byteVal := 0; byteVal < 256; byteVal++ {
		targetGrid := shuffledGrids[byteVal]
		for _, positions := range uniquePositions[targetGrid] {
			var currentHints [4]byte

			// 1. 计算抽象提示 (Abstract Hints)
			// 我们先计算出 val 和 pos，后面再根据模式编码成 byte
			var rawParts [4]struct{ val, pos byte }

			for i, pos := range positions {
				val := targetGrid[pos] // 1..4
				rawParts[i] = struct{ val, pos byte }{val, uint8(pos)}
			}

			// 这些 positions 已在 sharedSudokuTableData 中确认唯一，直接生成编码。
			for i, p := range rawParts {
				currentHints[i] = t.layout.hintByte(p.val-1, p.pos)
			}

			t.EncodeTable[byteVal] = append(t.EncodeTable[byteVal], currentHints)
			// 生成解码键 (需要对 Hints 进行排序以忽略传输顺序)
			key := packHintsToKey(currentHints)
			t.DecodeMap[key] = byte(byteVal)
		}
	}
	return t, nil
}

func customPatternForToken(token string, customPattern string) string {
	if token == asciiModeTokenEntropy {
		return customPattern
	}
	return ""
}

func (t *Table) OppositeDirection() *Table {
	if t == nil || t.opposite == nil {
		return t
	}
	return t.opposite
}

func (t *Table) Hint() uint32 {
	if t == nil {
		return 0
	}
	return t.hint
}

func tableHintFingerprint(key string, mode string, uplinkPattern string, downlinkPattern string) uint32 {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"sudoku-table-hint",
		key,
		mode,
		strings.ToLower(strings.TrimSpace(uplinkPattern)),
		strings.ToLower(strings.TrimSpace(downlinkPattern)),
	}, "\x00")))
	return binary.BigEndian.Uint32(sum[:4])
}

func packHintsToKey(hints [4]byte) uint32 {
	return packHintBytes(hints[0], hints[1], hints[2], hints[3])
}

func packHintBytes(h0, h1, h2, h3 byte) uint32 {
	// Sorting network for 4 elements (Bubble sort unrolled)
	// Swap if a > b
	if h0 > h1 {
		h0, h1 = h1, h0
	}
	if h2 > h3 {
		h2, h3 = h3, h2
	}
	if h0 > h2 {
		h0, h2 = h2, h0
	}
	if h1 > h3 {
		h1, h3 = h3, h1
	}
	if h1 > h2 {
		h1, h2 = h2, h1
	}

	return uint32(h0)<<24 | uint32(h1)<<16 | uint32(h2)<<8 | uint32(h3)
}
