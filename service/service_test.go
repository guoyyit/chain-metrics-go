package service

import (
	"math/big"
	"path/filepath"
	"testing"

	"github.com/guoyyit/chain-metrics-go/domain"
)

func testConfig(t *testing.T) domain.Config {
	t.Helper()
	cfg := domain.DefaultConfig()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.ActiveWindowBlocks = 4
	cfg.TopHolderCount = 5
	cfg.HistoryRetainBlocks = 50
	cfg.CheckpointInterval = 3
	return cfg
}

func bi(s string) *big.Int {
	v, _ := new(big.Int).SetString(s, 10)
	return v
}

// chainBuilder builds a linear chain of blocks with helpers to fork.
type chainBuilder struct {
	blocks  map[string]*domain.Block
	tipHash string
}

func newChainBuilder() *chainBuilder {
	return &chainBuilder{blocks: make(map[string]*domain.Block)}
}

func (c *chainBuilder) next(height uint64, events ...domain.Event) *domain.Block {
	b := &domain.Block{
		Height:     height,
		Hash:       hashFor(height, c.tipHash, events),
		ParentHash: c.tipHash,
		Timestamp:  int64(height),
		Events:     events,
	}
	c.blocks[b.Hash] = b
	c.tipHash = b.Hash
	return b
}

// forkAt extends from a different parent, independent of c.tipHash.
func (c *chainBuilder) forkFrom(parent *domain.Block, height uint64, events ...domain.Event) *domain.Block {
	b := &domain.Block{
		Height:     height,
		Hash:       hashFor(height, parent.Hash, events) + "-fork",
		ParentHash: parent.Hash,
		Timestamp:  int64(height),
		Events:     events,
	}
	c.blocks[b.Hash] = b
	return b
}

func hashFor(height uint64, parent string, events []domain.Event) string {
	parentShort := parent
	if len(parentShort) > 12 {
		parentShort = parentShort[len(parentShort)-12:]
	}
	h := "h" + itoa(height) + "-p" + parentShort
	for _, ev := range events {
		h += ":" + ev.Token + ev.TxHash
	}
	return h
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func transfer(token, from, to, amount, tx string, idx int) domain.Event {
	return domain.Event{
		Type:     domain.EventTransfer,
		Token:    token,
		From:     from,
		To:       to,
		Amount:   bi(amount),
		TxHash:   tx,
		LogIndex: idx,
	}
}

func mint(token, to, amount, tx string, idx int) domain.Event {
	return domain.Event{
		Type:     domain.EventMint,
		Token:    token,
		From:     domain.ZeroAddress,
		To:       to,
		Amount:   bi(amount),
		TxHash:   tx,
		LogIndex: idx,
	}
}

func burn(token, from, amount, tx string, idx int) domain.Event {
	return domain.Event{
		Type:     domain.EventBurn,
		Token:    token,
		From:     from,
		To:       domain.ZeroAddress,
		Amount:   bi(amount),
		TxHash:   tx,
		LogIndex: idx,
	}
}
