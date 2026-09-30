package engine

import (
	"math/big"

	"github.com/guoyyit/chain-metrics-go/metrics"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// IngestBlock accepts one block.
//
//   - Same-hash redelivery is ignored (events are content-addressed
//     with (tx hash, log index) inside their block, so duplicate events
//     can never be counted twice).
//   - Blocks above head+1 are buffered and auto-applied once their
//     parent becomes canonical.
//   - A block at head height with a different hash starts a reorg.
//   - Blocks below head height cannot change published history and are
//     rejected (rollback to an arbitrary ancestor is an explicit
//     reorg driven by a competing block at head height).
func (e *Engine) IngestBlock(b *domain.Block) error {
	if err := e.validate(b); err != nil {
		return err
	}

	// Durability first: a synced content-addressed file means a crash
	// immediately after intake still replays the block exactly once.
	if !e.st.HasBlockAt(b.Height, b.Hash) {
		if err := e.st.SaveBlock(b); err != nil {
			return err
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.pending[b.Hash]; exists {
		return nil
	}
	if b.Hash == e.head.Hash {
		return nil
	}
	if b.Height < e.head.Height {
		return e.handleCompetingBlockLocked(b)
	}

	if b.Height == e.head.Height {
		return e.handleCompetingBlockLocked(b)
	}

	e.bufferLocked(b)
	wasConnected := b.ParentHash == e.head.Hash
	if err := e.drainLocked(); err != nil {
		return err
	}
	// drainLocked appends connected blocks; if delivery completes a
	// competing branch whose base is below the head, the freshly
	// buffered block can trigger that fork now.
	if !wasConnected {
		return e.handleCompetingBlockLocked(b)
	}
	return nil
}

// handleCompetingBlockLocked decides what to do with a delivered block
// that does not extend the current canonical chain. It walks the
// block's ancestry through stored and buffered blocks to locate the
// common ancestor and the highest buffered descendant (the competing
// branch tip):
//
//   - the branch tip below current head: just buffer it, the shorter
//     fork cannot become canonical yet;
//   - the branch tip at head height with a different hash: reorg now;
//   - the branch tip above head: reorg to the longer tip directly.
func (e *Engine) handleCompetingBlockLocked(b *domain.Block) error {
	tip, tipHeight := e.competingTipLocked(b)
	if tip == nil {
		return nil
	}
	switch {
	case tipHeight < e.head.Height:
		return nil
	case tipHeight == e.head.Height:
		if tip.Hash == e.head.Hash {
			return nil
		}
		return e.runReorgLocked(tip)
	default:
		return e.runReorgLocked(tip)
	}
}

// competingTipLocked returns the highest buffered descendant of start
// whose branch is complete back to the current canonical chain, plus
// its height. It returns nil when no such branch exists yet.
func (e *Engine) competingTipLocked(start *domain.Block) (*domain.Block, uint64) {
	// Ensure start is buffered before the search.
	e.bufferLocked(start)

	// Find the highest buffered descendant of start whose whole branch
	// back to the common ancestor is available (buffered or stored).
	var tip *domain.Block
	tipHeight := uint64(0)
	if e.branchCompleteLocked(start) {
		tip, tipHeight = start, start.Height
	}
	for _, cand := range e.pending {
		if cand.Height < start.Height {
			continue
		}
		if e.ancestryContainsLocked(cand, start.Hash) && e.branchCompleteLocked(cand) {
			if cand.Height > tipHeight {
				tip, tipHeight = cand, cand.Height
			}
		}
	}
	return tip, tipHeight
}

// branchCompleteLocked reports that every block from cand down to a
// block already on the canonical chain is retrievable.
func (e *Engine) branchCompleteLocked(cand *domain.Block) bool {
	cur := cand
	for cur != nil {
		if cur.Height <= e.head.Height {
			if cur.Hash == e.head.Hash {
				return true
			}
			if snap, ok := e.history.get(cur.Height); ok && snap.BlockHash == cur.Hash {
				return true
			}
			if _, err := e.st.SnapshotAtHeight(cur.Height); err == nil {
				return true
			}
		}
		if cur.ParentHash == "" {
			return false
		}
		if parent := e.pending[cur.ParentHash]; parent != nil {
			cur = parent
			continue
		}
		parent, err := e.st.LoadBlockAt(cur.Height-1, cur.ParentHash)
		if err != nil {
			return false
		}
		cur = parent
	}
	return false
}

// ancestryContainsLocked reports whether ancestorHash is on block's
// parent chain (searching buffer and store, bounded by head height).
func (e *Engine) ancestryContainsLocked(block *domain.Block, ancestorHash string) bool {
	cur := block
	seen := make(map[string]struct{})
	for cur != nil {
		if cur.Hash == ancestorHash {
			return true
		}
		if _, ok := seen[cur.Hash]; ok {
			return false
		}
		seen[cur.Hash] = struct{}{}
		if cur.ParentHash == "" {
			return false
		}
		if parent := e.pending[cur.ParentHash]; parent != nil {
			cur = parent
			continue
		}
		parent, err := e.st.LoadBlockAt(cur.Height-1, cur.ParentHash)
		if err != nil {
			return false
		}
		cur = parent
	}
	return false
}

func (e *Engine) validate(b *domain.Block) error {
	if b == nil || b.Hash == "" {
		return domain.ErrInvalidBlock
	}
	if b.Height == 0 {
		return domain.ErrInvalidBlock
	}
	seen := make(map[string]struct{}, len(b.Events))
	for i := range b.Events {
		ev := &b.Events[i]
		if ev.Amount == nil {
			ev.Amount = new(big.Int)
		}
		if ev.Amount.Sign() < 0 {
			return domain.ErrInvalidBlock
		}
		switch ev.Type {
		case domain.EventMint:
			if ev.From != domain.ZeroAddress {
				return domain.ErrInvalidBlock
			}
		case domain.EventBurn:
			if ev.To != domain.ZeroAddress {
				return domain.ErrInvalidBlock
			}
		case domain.EventTransfer:
			if ev.From == domain.ZeroAddress || ev.To == domain.ZeroAddress {
				return domain.ErrInvalidBlock
			}
		default:
			return domain.ErrInvalidBlock
		}
		key := ev.Token + "|" + ev.Key()
		if _, dup := seen[key]; dup {
			return domain.ErrInvalidBlock
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (e *Engine) bufferLocked(b *domain.Block) {
	if _, exists := e.pending[b.Hash]; exists {
		return
	}
	e.pending[b.Hash] = b
	set := e.byHeight[b.Height]
	if set == nil {
		set = make(map[string]struct{})
		e.byHeight[b.Height] = set
	}
	set[b.Hash] = struct{}{}
}

func (e *Engine) unbufferLocked(hash string) {
	b := e.pending[hash]
	if b == nil {
		return
	}
	delete(e.pending, hash)
	if set := e.byHeight[b.Height]; set != nil {
		delete(set, hash)
		if len(set) == 0 {
			delete(e.byHeight, b.Height)
		}
	}
}

// drainLocked appends every buffered block that extends the current
// chain, in ascending height order, repeating until fixed point.
func (e *Engine) drainLocked() error {
	for {
		nextHeight := e.head.Height + 1
		set := e.byHeight[nextHeight]
		var chosen *domain.Block
		var chosenHash string
		// Deterministic tie-break on hash: canonical choice only
		// matters before a competing branch proves equal height.
		for hash := range set {
			b := e.pending[hash]
			if b.ParentHash != e.head.Hash {
				continue
			}
			if chosen == nil || b.Hash < chosenHash {
				chosen, chosenHash = b, b.Hash
			}
		}
		if chosen == nil {
			return nil
		}
		e.unbufferLocked(chosen.Hash)
		if err := e.appendLocked(chosen); err != nil {
			// Re-buffer so a later retry is possible; in-memory state
			// changes made by this block were not committed to head.
			e.bufferLocked(chosen)
			return err
		}
	}
}

// appendLocked applies a direct child of the current head and commits
// the publication atomically (state -> snapshot durability -> head).
func (e *Engine) appendLocked(b *domain.Block) error {
	e.state.Apply(b)
	snap := e.state.Snapshot(b)
	if err := e.st.SaveSnapshot(snap); err != nil {
		e.state.Rollback(b)
		return err
	}
	pointer := domain.HeadPointer{Height: b.Height, Hash: b.Hash}
	if err := e.st.SaveHead(pointer); err != nil {
		_ = e.st.TruncateSnapshotsAbove(e.head.Height)
		return err
	}
	e.history.put(snap)
	e.head = pointer
	e.maybeCheckpointLocked()
	return e.pruneHistoryLocked()
}

// maybeCheckpointLocked rotates a durable state checkpoint at the
// configured interval so recovery replay stays bounded. A checkpoint
// failure never invalidates the commit: the next interval simply
// replays a bit more on recovery.
func (e *Engine) maybeCheckpointLocked() {
	interval := e.cfg.CheckpointInterval
	if interval == 0 || e.head.Height == 0 || e.head.Height%interval != 0 {
		return
	}
	data, err := e.state.Marshal()
	if err != nil {
		return
	}
	if err := e.st.SaveCheckpoint(&metrics.Checkpoint{
		Height: e.head.Height,
		Hash:   e.head.Hash,
		Data:   data,
	}); err != nil {
		return
	}
}
