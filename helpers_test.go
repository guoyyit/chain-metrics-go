package chainmetrics

import (
	"math/big"
	"testing"
)

func bi(s int64) *big.Int {
	return big.NewInt(s)
}

func testConfig() Config {
	return Config{
		DataDir:       "",
		HistoryBlocks: 64,
		Metrics: MetricConfig{
			ActiveWindowBlocks: 3,
			VolumeWindowBlocks: 3,
			TopN:               5,
			Tiers: []BalanceTier{
				{Name: "small", Min: bi(1), Max: bi(100)},
				{Name: "large", Min: bi(100), Max: nil},
			},
		},
	}
}

func ev(token string, typ EventType, from, to string, amount int64, block uint64, index uint32) Event {
	return Event{
		TokenID: token,
		Type:    typ,
		From:    from,
		To:      to,
		Amount:  bi(amount),
		Block:   block,
		Index:   index,
	}
}

func block(number uint64, hash, parent string, events ...Event) Block {
	return Block{Number: number, Hash: hash, ParentHash: parent, Events: events}
}

// genChain builds linked blocks start..end. Block n has hash prefix+itoa(n);
// the first block's parent is parentHash. Each block mints `mint` tokens to A
// and moves 1 token A->B->A so branches with different prefixes/amounts
// diverge in every metric.
func genChain(prefix string, start, end uint64, parentHash string, mint int64) []Block {
	var out []Block
	prev := parentHash
	for n := start; n <= end; n++ {
		h := prefix + uintToStr(n)
		out = append(out, block(n, h, prev,
			ev("TKN", Mint, "", "A", mint, n, 0),
			ev("TKN", Transfer, "A", "B", 1, n, 1),
			ev("TKN", Transfer, "B", "A", 1, n, 2),
		))
		prev = h
	}
	return out
}

// joinChains concatenates chain segments into one slice for reference rebuilds.
func joinChains(segs ...[]Block) []Block {
	var out []Block
	for _, s := range segs {
		out = append(out, s...)
	}
	return out
}

// assertWindowMatchesRef compares every snapshot queryable on e against the
// reference for the same height, and verifies no snapshot exists below
// lowestQueryable.
func assertWindowMatchesRef(t *testing.T, e *Engine, ref map[uint64]Snapshot, lowestQueryable, head uint64) {
	t.Helper()
	for h := lowestQueryable; h <= head; h++ {
		got, ok := e.QueryAt(h)
		if !ok {
			t.Fatalf("missing snapshot at height %d", h)
		}
		want, ok := ref[h]
		if !ok {
			t.Fatalf("reference missing height %d", h)
		}
		if !snapshotsEqual(got, want) {
			t.Fatalf("height %d mismatch:\n got=%+v\nwant=%+v", h, got, want)
		}
	}
	for h := uint64(1); h < lowestQueryable; h++ {
		if _, ok := e.QueryAt(h); ok {
			t.Fatalf("height %d should have been pruned", h)
		}
	}
}

func mustPush(t *testing.T, e *Engine, b Block) {
	t.Helper()
	if err := e.PushBlock(b); err != nil {
		t.Fatalf("push block %s: %v", hashLabel(b.Hash), err)
	}
}

func hashLabel(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func metricAt(t *testing.T, e *Engine, height uint64, token string) TokenMetrics {
	t.Helper()
	snap, ok := e.QueryAt(height)
	if !ok {
		t.Fatalf("no snapshot at height %d", height)
	}
	m, ok := snap.Tokens[token]
	if !ok {
		t.Fatalf("no metric for token %s at height %d", token, height)
	}
	return m
}
