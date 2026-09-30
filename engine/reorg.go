package engine

import (
	"github.com/guoyyit/chain-metrics-go/domain"
)

// reorgToLocked switches the canonical chain from the current head to
// candidate (same height, different hash). The resulting state is
// identical to having replayed the candidate branch from the common
// ancestor, because rollback is the exact inverse of apply.
func (e *Engine) reorgToLocked(candidate *domain.Block) error {
	return e.runReorgLocked(candidate)
}

// runReorgLocked performs the crash-safe switch:
//
//  1. walk candidate's parent chain to the common ancestor with the
//     current chain (all blocks are content-addressed on disk);
//  2. durably record the intent (new tip);
//  3. invert state back to the ancestor, block by block;
//  4. replay the new branch, collecting fresh snapshots;
//  5. atomically rewrite the snapshot archive at the ancestor;
//  6. atomically publish the new head pointer and clear the intent.
//
// A crash anywhere before step 6 leaves the durable head on the old
// chain; Recover sees the intent and resumes the switch idempotently
// using the stored blocks.
func (e *Engine) runReorgLocked(candidate *domain.Block) error {
	// Index every hash on the current canonical chain by height,
	// walking parent pointers from the head. Blocks are never deleted,
	// so ancestors at any depth are reachable.
	currentHashes := make(map[uint64]string)
	curHash := e.head.Hash
	for h := e.head.Height; h > 0; h-- {
		cb, err := e.st.LoadBlockAt(h, curHash)
		if err != nil {
			return err
		}
		currentHashes[h] = cb.Hash
		curHash = cb.ParentHash
	}

	// Build the new branch tip-down: [tip, ..., childOfAncestor].
	branchTipDown := make([]*domain.Block, 0)
	cur := candidate
	ancestorHeight := uint64(0)
	ancestorHash := ""
	for {
		if cur.Height == 0 {
			break
		}
		if h, ok := currentHashes[cur.Height]; ok && h == cur.Hash {
			ancestorHeight = cur.Height
			ancestorHash = cur.Hash
			break
		}
		branchTipDown = append(branchTipDown, cur)
		if cur.ParentHash == "" {
			break
		}
		parent, err := e.st.LoadBlockAt(cur.Height-1, cur.ParentHash)
		if err != nil {
			return err
		}
		cur = parent
	}

	// Resolve ancestor pointer (genesis = 0 / "").
	var ancestor domain.HeadPointer
	ancestor.Height = ancestorHeight
	ancestor.Hash = ancestorHash

	// New branch in ascending order.
	branchAsc := make([]*domain.Block, len(branchTipDown))
	for i, b := range branchTipDown {
		branchAsc[len(branchTipDown)-1-i] = b
	}

	tip := domain.HeadPointer{Height: candidate.Height, Hash: candidate.Hash}
	if err := e.st.SaveReorgIntent(tip); err != nil {
		return err
	}

	// Roll in-memory state back to the ancestor.
	if err := e.rollbackStateLocked(ancestor); err != nil {
		return err
	}

	// Replay branch, producing fresh snapshots.
	newSnaps := make([]*domain.BlockSnapshot, 0, len(branchAsc))
	for _, b := range branchAsc {
		e.state.Apply(b)
		newSnaps = append(newSnaps, e.state.Snapshot(b))
	}

	// Archive: keep the old branch strictly below the divergence point
	// (the ancestor stays), then append the new branch. The ancestor's
	// snapshot is identical on both branches because its state is the
	// common prefix.
	if err := e.st.RewriteArchiveDuringReorg(ancestor.Height, newSnaps); err != nil {
		return err
	}

	// In-memory ring: drop old-branch entries above the ancestor and
	// install new ones.
	e.history.truncateAbove(ancestor.Height)
	for _, snap := range newSnaps {
		e.history.put(snap)
	}

	// Head commit + intent clear are the publication boundary.
	if err := e.st.SaveHead(tip); err != nil {
		return err
	}
	e.head = tip
	if err := e.st.ClearReorgIntent(); err != nil {
		return err
	}

	e.maybeCheckpointLocked()
	e.pruneHistoryLocked()

	// A new branch may un-buffer previously disconnected blocks.
	e.drainLocked()
	return nil
}

// rollbackStateLocked walks the current chain from head to the target
// ancestor, loading blocks by hash and applying exact inverses.
func (e *Engine) rollbackStateLocked(target domain.HeadPointer) error {
	hash := e.head.Hash
	for h := e.head.Height; h > target.Height; h-- {
		b, err := e.st.LoadBlockAt(h, hash)
		if err != nil {
			return err
		}
		e.state.Rollback(b)
		hash = b.ParentHash
	}
	if target.Height > 0 && hash != target.Hash {
		return domain.ErrInvalidBlock
	}
	e.head = domain.HeadPointer{Height: target.Height, Hash: target.Hash}
	return nil
}

// pruneHistoryLocked bounds on-disk snapshot history by
// HistoryRetainBlocks. Blocks themselves remain for reorg/replay.
func (e *Engine) pruneHistoryLocked() error {
	retain := e.cfg.HistoryRetainBlocks
	if retain == 0 || e.head.Height <= retain {
		return nil
	}
	floor := e.head.Height - retain + 1
	if floor <= e.archiveFloor {
		return nil
	}
	e.archiveFloor = floor
	// Batch physical rewrites: at most once per retain/4 blocks.
	batch := retain / 4
	if batch == 0 {
		batch = 1
	}
	if e.archiveRewrittenAt != 0 && e.head.Height-e.archiveRewrittenAt < batch {
		return nil
	}
	e.archiveRewrittenAt = e.head.Height
	return e.st.PruneSnapshots(floor)
}
