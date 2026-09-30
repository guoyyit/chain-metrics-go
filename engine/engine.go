// Package engine ingests blocks under out-of-order, duplicate and
// reorg conditions, maintains incremental token metrics and publishes
// one atomic snapshot per block.
package engine

import (
	"sync"

	"github.com/guoyyit/chain-metrics-go/domain"
	"github.com/guoyyit/chain-metrics-go/metrics"
	"github.com/guoyyit/chain-metrics-go/rebuild"
	"github.com/guoyyit/chain-metrics-go/store"
)

// Engine ingests blocks, maintains metric state, publishes snapshots
// and answers historical queries. All public methods are safe for
// concurrent use; publication is serialized under the write lock while
// queries hold the read lock.
type Engine struct {
	mu    sync.RWMutex
	cfg   domain.Config
	st    store.Store
	state *metrics.State

	// pending stores delivered-but-not-yet-canonical blocks by hash.
	pending map[string]*domain.Block
	// byHeight indexes pending hashes per height (forks keep several).
	byHeight map[uint64]map[string]struct{}

	history *snapshotRing
	head    domain.HeadPointer

	// archiveFloor is the prune watermark; archiveRewrittenAt is the
	// last head height at which a physical archive rewrite happened.
	archiveFloor       uint64
	archiveRewrittenAt uint64
}

// New creates an engine over an opened store. Call Recover before
// serving requests.
func New(cfg domain.Config, st store.Store) *Engine {
	capacity := cfg.HistoryRetainBlocks
	if capacity == 0 {
		capacity = 1
	}
	return &Engine{
		cfg:      cfg,
		st:       st,
		state:    metrics.NewState(cfg),
		pending:  make(map[string]*domain.Block),
		byHeight: make(map[uint64]map[string]struct{}),
		history:  newSnapshotRing(capacity),
	}
}

// Head returns the latest published canonical pointer.
func (e *Engine) Head() domain.HeadPointer {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.head
}

// Recover restores state after a restart.
//
// The durable head pointer is the commit record: it is only advanced
// after the block file and snapshot line are fsynced, so no event at
// or below the head is applied twice and no committed event is lost.
// Recovery either resumes from a checkpoint on the canonical chain or
// performs a full replay; a recorded reorg intent is finished first.
func (e *Engine) Recover() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	head, err := e.st.LoadHead()
	if err == domain.ErrNotFound {
		// No committed block yet, but a crash may have left synced,
		// never-published block files. Buffer them so a later delivery
		// (or a replay of genesis) can still connect the chain.
		if err := e.st.StreamBlocks(func(b *domain.Block) error {
			e.bufferLocked(b)
			return nil
		}); err != nil {
			return err
		}
		return e.drainLocked()
	}
	if err != nil {
		return err
	}

	chain, err := rebuild.CanonicalChain(e.st, head.Hash)
	if err != nil {
		return err
	}
	chainAt := make(map[uint64]string, len(chain))
	for _, b := range chain {
		chainAt[b.Height] = b.Hash
	}

	state := metrics.NewState(e.cfg)
	startHeight := uint64(0)
	if cp, cpErr := e.st.LatestCheckpoint(); cpErr == nil && cp.Height <= head.Height {
		if chainAt[cp.Height] == cp.Hash {
			if restored, err := metrics.UnmarshalState(e.cfg, cp.Data); err == nil {
				if rp := restored.Head(); rp.Height == cp.Height && rp.Hash == cp.Hash {
					state = restored
					startHeight = cp.Height
				}
			}
		}
	}

	for _, b := range chain {
		if b.Height <= startHeight {
			// Seed the ring even when resuming from checkpoint.
			if b.Height > head.Height-e.cfg.HistoryRetainBlocks || e.cfg.HistoryRetainBlocks == 0 {
				if snap, err := e.st.SnapshotAtHeight(b.Height); err == nil {
					e.history.put(snap)
				}
			}
			continue
		}
		state.Apply(b)
		snap := state.Snapshot(b)
		e.history.put(snap)
	}

	e.state = state
	e.head = head

	// Finish a reorg interrupted before the new head commit: state is
	// restored to the old head first, then the switch is resumed
	// idempotently (archive rewrite and head commit are atomic).
	if intent, ierr := e.st.LoadReorgIntent(); ierr == nil && intent.Hash != "" {
		if tipBlock, lerr := e.st.LoadBlock(intent.Hash); lerr == nil {
			if err := e.runReorgLocked(tipBlock); err != nil {
				return err
			}
			head = e.head
		}
	}

	e.pending = make(map[string]*domain.Block)
	e.byHeight = make(map[uint64]map[string]struct{})

	// Re-buffer stored blocks that are off the canonical chain or lie
	// above the head, so delivery order across restart does not matter.
	return e.st.StreamBlocks(func(b *domain.Block) error {
		if b.Height > head.Height || chainAt[b.Height] != b.Hash {
			e.bufferLocked(b)
		}
		return nil
	})
}
