package chainmetrics

import (
	"fmt"
	"math/big"
	"sync"
)

const defaultHistoryBlocks = 128

// Engine ingests blocks and serves consistent, block-scoped metric snapshots.
//
// A single RWMutex serializes ingestion against queries. A query always
// observes one immutable Snapshot value, so it can never see a mix of fields
// belonging to different blocks.
type Engine struct {
	mu    sync.RWMutex
	cfg   Config
	store *Store
	chain *chainTree

	// pending holds out-of-order loose events keyed by
	// block number -> block hash -> token id -> event index.
	pending map[uint64]map[string]map[string]map[uint32]Event

	// pendingBlocks holds blocks whose parent is not yet known.
	pendingBlocks map[string]Block

	// snapshots by block height, bounded to HistoryBlocks.
	snapshots map[uint64]Snapshot

	headNumber uint64
	headHash   string

	// checkpointHeight is the block whose full state is durably snapshotted.
	checkpointHeight uint64
}

// NewEngine creates an engine and replays durable state when a data dir is set.
func NewEngine(cfg Config) (*Engine, error) {
	cfg.Metrics = normalizeMetricConfig(cfg.Metrics)
	if cfg.HistoryBlocks == 0 {
		cfg.HistoryBlocks = defaultHistoryBlocks
	}
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		cfg:           cfg,
		store:         store,
		chain:         newChainTree(),
		pending:       make(map[uint64]map[string]map[string]map[uint32]Event),
		pendingBlocks: make(map[string]Block),
		snapshots:     make(map[uint64]Snapshot),
	}
	cpRaw, err := store.loadCheckpoint()
	if err != nil {
		return nil, err
	}
	if cpRaw != nil {
		node, _, _, derr := decodeCheckpoint(cpRaw)
		if derr != nil {
			return nil, derr
		}
		e.chain.add(node)
		e.publishNode(node)
		e.checkpointHeight = node.number
	}

	blocks, err := store.loadCanonicalChain()
	if err != nil {
		return nil, err
	}
	for _, b := range blocks {
		if e.headHash != "" && b.Number <= e.headNumber {
			continue
		}
		if err := e.applyCanonical(b); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// Close flushes durable state and releases resources.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store.Close()
}

// PushEvent buffers one event before its block arrives. Events may arrive in
// any order and may repeat; repeats are discarded via (block, index) identity.
// The event takes effect once the matching block is pushed on the canonical
// chain.
func (e *Engine) PushEvent(ev Event) error {
	if ev.Amount == nil {
		return fmt.Errorf("event amount is nil")
	}
	if ev.TokenID == "" {
		return fmt.Errorf("event token id is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bufferEvent(ev, "")
}

// QueryAt returns the complete immutable snapshot at the requested height.
func (e *Engine) QueryAt(height uint64) (Snapshot, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	snap, ok := e.snapshots[height]
	return cloneSnapshot(snap), ok
}

// QueryLatest returns the newest published snapshot.
func (e *Engine) QueryLatest() (Snapshot, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	snap, ok := e.snapshots[e.headNumber]
	return cloneSnapshot(snap), ok
}

// cloneSnapshot deep-copies the big.Int values so callers cannot mutate state.
func cloneSnapshot(s Snapshot) Snapshot {
	out := Snapshot{
		BlockNumber: s.BlockNumber,
		BlockHash:   s.BlockHash,
		Tokens:      make(map[string]TokenMetrics, len(s.Tokens)),
	}
	for tokenID, m := range s.Tokens {
		cp := m
		cp.CirculatingSupply = cpBig(m.CirculatingSupply)
		cp.TransferVolume = cpBig(m.TransferVolume)
		cp.TierCounts = make(map[string]int64, len(m.TierCounts))
		for k, v := range m.TierCounts {
			cp.TierCounts[k] = v
		}
		if m.TopHoldings != nil {
			cp.TopHoldings = make([]Holding, len(m.TopHoldings))
			for i, h := range m.TopHoldings {
				cp.TopHoldings[i] = Holding{Address: h.Address, Balance: cpBig(h.Balance)}
			}
		}
		out.Tokens[tokenID] = cp
	}
	return out
}

func cpBig(v *big.Int) *big.Int {
	if v == nil {
		return nil
	}
	return new(big.Int).Set(v)
}
