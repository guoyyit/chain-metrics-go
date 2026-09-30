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
