package chainmetrics

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
)

const defaultHistoryBlocks = 128

// ErrReorgTooDeep is returned when a fork would require rolling back below
// the retained history window. It is distinguishable via errors.Is.
var ErrReorgTooDeep = errors.New("reorg fork point below retained history")

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

	// rejectedForks tracks hashes of blocks rejected as too-deep forks so
	// their descendants are rejected as well instead of buffered forever.
	rejectedForks map[string]uint64

	// snapshots by block height, bounded to HistoryBlocks.
	snapshots map[uint64]Snapshot

	headNumber uint64
	headHash   string

	// checkpointHeight is the block whose full state is durably snapshotted.
	checkpointHeight uint64

	// diskHeadHash is the canonical tip last committed to head.json.
	diskHeadHash string

	// hook, when set (tests only), is invoked at durable-state stages.
	hook func(stage string)
}

func (e *Engine) emitHook(stage string) {
	if e.hook != nil {
		e.hook(stage)
	}
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
		rejectedForks: make(map[string]uint64),
		snapshots:     make(map[uint64]Snapshot),
	}
	cpRaw, err := store.loadCheckpoint()
	if err != nil {
		return nil, err
	}
	var cpHash string
	if cpRaw != nil {
		node, _, _, derr := decodeCheckpoint(cpRaw)
		if derr != nil {
			return nil, derr
		}
		e.chain.add(node)
		e.publishNode(node)
		e.checkpointHeight = node.number
		cpHash = node.hash
	}

	headNumber, headHash, hasHead, err := store.loadHead()
	if err != nil {
		return nil, err
	}
	if !hasHead {
		// No committed head: anything in the WAL was never committed and is
		// ignored. A checkpoint without a head is corrupted durable state.
		if cpRaw != nil {
			return nil, fmt.Errorf("corrupt durable state: checkpoint at %d without head", e.checkpointHeight)
		}
		return e, nil
	}
	walked, err := store.walkChain(headHash)
	if err != nil {
		return nil, err
	}
	above, err := validateRecoveredChain(walked, headNumber, headHash, e.checkpointHeight, cpHash)
	if err != nil {
		return nil, err
	}
	e.diskHeadHash = headHash
	for _, b := range above {
		if err := e.applyCanonical(b); err != nil {
			return nil, err
		}
	}
	// Best-effort cleanup of unreachable records left by a crash.
	_ = e.store.compactWal(e.diskHeadHash, e.checkpointHeight)
	return e, nil
}

// validateRecoveredChain checks that the blocks reachable from the durable
// head connect contiguously to the checkpoint image (or to genesis when no
// image exists) and returns exactly the blocks above the checkpoint, in
// ascending order. Any gap means corrupted durable state.
func validateRecoveredChain(walked []Block, headNumber uint64, headHash string, cpHeight uint64, cpHash string) ([]Block, error) {
	var above []Block
	for _, b := range walked {
		if cpHash != "" && b.Number <= cpHeight {
			continue
		}
		above = append(above, b)
	}
	if cpHash != "" {
		if headNumber < cpHeight {
			return nil, fmt.Errorf("corrupt durable state: head %d below checkpoint %d", headNumber, cpHeight)
		}
		if headNumber == cpHeight {
			if headHash != cpHash {
				return nil, fmt.Errorf("corrupt durable state: head %s is not checkpoint %s", headHash, cpHash)
			}
			if len(above) != 0 {
				return nil, fmt.Errorf("corrupt durable state: %d blocks above checkpointed head %d", len(above), cpHeight)
			}
			return nil, nil
		}
		if uint64(len(above)) != headNumber-cpHeight {
			return nil, fmt.Errorf("corrupt durable state: head %d unreachable from checkpoint %d", headNumber, cpHeight)
		}
		if above[0].ParentHash != cpHash {
			return nil, fmt.Errorf("corrupt durable state: block %d does not extend checkpoint %s", above[0].Number, cpHash)
		}
	} else if len(above) == 0 {
		return nil, fmt.Errorf("corrupt durable state: head %s missing from WAL", headHash)
	} else if above[0].ParentHash != "" {
		return nil, fmt.Errorf("corrupt durable state: chain bottom %d has unknown parent", above[0].Number)
	}
	for i := 1; i < len(above); i++ {
		if above[i].Number != above[i-1].Number+1 || above[i].ParentHash != above[i-1].Hash {
			return nil, fmt.Errorf("corrupt durable state: gap before block %d", above[i].Number)
		}
	}
	if len(above) > 0 && above[len(above)-1].Number != headNumber {
		return nil, fmt.Errorf("corrupt durable state: head height %d, walked top %d", headNumber, above[len(above)-1].Number)
	}
	return above, nil
}

// floorHeight is the lowest canonical block height still retained in memory;
// it is the deepest block a reorganization may roll back to.
func (e *Engine) floorHeight() uint64 {
	n := e.chain.get(e.headHash)
	if n == nil {
		return 0
	}
	for {
		parent := e.chain.get(n.parentHash)
		if parent == nil {
			return n.number
		}
		n = parent
	}
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
