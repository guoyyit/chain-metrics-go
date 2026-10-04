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

	// unreachableParents records hashes of blocks whose ancestry is known to
	// run below the rollback limit (already rejected), keyed by hash with the
	// block height for bounded pruning. Descendants are rejected too instead
	// of being buffered as out-of-order intermediates.
	unreachableParents map[string]uint64

	// snapshots by block height, bounded to HistoryBlocks.
	snapshots map[uint64]Snapshot

	headNumber uint64
	headHash   string

	// checkpointHeight is the height captured by the durable state image.
	// The image is always published at or below the finalized boundary
	// (head minus HistoryBlocks), so every block the engine claims to support
	// rollback to stays outside the image.
	checkpointHeight uint64
	checkpointHash   string

	// envelopeHeights are block heights carried in the durable state image
	// envelope (e.g. a deep-reorganization fork point). Their snapshots stay
	// queryable even one height below the rolling retention window.
	envelopeHeights map[uint64]bool

	// crashHook, when set, is invoked at named points of the durable
	// switch protocol and allows tests to stop right after a given disk write,
	// emulating a crash in the middle of a reorganization.
	crashHook func(point string) error
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
		cfg:                cfg,
		store:              store,
		chain:              newChainTree(),
		pending:            make(map[uint64]map[string]map[string]map[uint32]Event),
		pendingBlocks:      make(map[string]Block),
		unreachableParents: make(map[string]uint64),
		envelopeHeights:    make(map[uint64]bool),
		snapshots:          make(map[uint64]Snapshot),
	}
	if err := e.recover(); err != nil {
		return nil, err
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
