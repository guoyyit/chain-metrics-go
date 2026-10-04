package chainmetrics

import "fmt"

// activate publishes a node that directly extends the canonical head.
func (e *Engine) activate(node *blockNode) error {
	if e.headHash != "" && node.number != e.headNumber+1 {
		return nil
	}
	if err := e.store.persistBlock(node.block); err != nil {
		return err
	}
	e.emitHook("activate.wal")
	if err := e.store.saveHead(node.number, node.hash); err != nil {
		return err
	}
	e.diskHeadHash = node.hash
	e.emitHook("activate.head")
	e.publishNode(node)
	return e.maybeCheckpoint(node)
}

// maybeReorgTo switches the canonical chain when node forms a branch forking
// below the current tip. The durable state is committed before any in-memory
// snapshot changes, so a crash at any point recovers either the complete old
// chain or the complete new chain, never a mix. State is rebuilt from the
// fork point, so results are identical to a fresh replay from that ancestor.
func (e *Engine) maybeReorgTo(node *blockNode) error {
	// Longest-chain rule: adopt the branch only once it overtakes the current
	// canonical tip. An equal-height sibling is retained but not activated.
	if node.number <= e.headNumber {
		return nil
	}
	fork := e.findFork(node)
	if fork == nil {
		// The branch would overtake but shares no ancestor inside the
		// retained window: the rollback target is gone for good.
		return fmt.Errorf("cannot reorg head %d to block %s(%d): %w",
			e.headNumber, node.hash, node.number, ErrReorgTooDeep)
	}
	// Collect the new branch in ascending order (fork excluded).
	chain := e.chain.ancestors(node.hash)
	var seq []*blockNode
	for _, n := range chain {
		if n.number <= fork.number {
			break
		}
		seq = append(seq, n)
	}

	// Durable phase: commit the new branch to disk first. If anything fails
	// here, no in-memory snapshot has been touched and the durable head
	// still points at a fully recoverable old chain.
	if err := e.persistReorg(fork, seq); err != nil {
		return err
	}

	// In-memory phase: rewind and republish along the new branch.
	for h := e.headNumber; h > fork.number; h-- {
		delete(e.snapshots, h)
	}
	e.headNumber = fork.number
	e.headHash = fork.hash
	for i := len(seq) - 1; i >= 0; i-- {
		n := seq[i]
		e.publishNode(n)
		if err := e.maybeCheckpoint(n); err != nil {
			return err
		}
	}
	return nil
}

// persistReorg commits a reorganization to durable storage. The head pointer
// update is the single atomic commit point; every step before it keeps the
// old head fully recoverable.
func (e *Engine) persistReorg(fork *blockNode, seq []*blockNode) error {
	if e.cfg.DataDir == "" {
		return nil
	}
	if fork.number < e.checkpointHeight {
		// Unreachable by construction: the durable image is always written
		// at the retention floor, so any fork point inside the retained
		// window is at or above the image. Guard against invariant breakage.
		return fmt.Errorf("internal: fork %d below checkpoint %d", fork.number, e.checkpointHeight)
	}
	// Append the new branch, then atomically move the head pointer.
	for i := len(seq) - 1; i >= 0; i-- {
		if err := e.store.persistBlock(seq[i].block); err != nil {
			return err
		}
	}
	e.emitHook("reorg.branch")
	tip := seq[0] // seq is ordered descending from the branch tip.
	if err := e.store.saveHead(tip.number, tip.hash); err != nil {
		return err
	}
	e.diskHeadHash = tip.hash
	e.emitHook("reorg.head")
	// Post-commit maintenance: drop abandoned-branch records. Best effort:
	// a failure only leaves unreachable garbage that a later compaction or
	// restart removes, so it must not fail the committed reorg.
	_ = e.store.compactWal(e.diskHeadHash, e.checkpointHeight)
	e.emitHook("reorg.compact")
	return nil
}

// findFork returns the common ancestor of node and the current head.
func (e *Engine) findFork(node *blockNode) *blockNode {
	head := e.chain.get(e.headHash)
	if head == nil {
		return nil
	}
	ancestors := make(map[string]bool)
	for h := head; h != nil; h = e.chain.get(h.parentHash) {
		ancestors[h.hash] = true
	}
	for n := node; n != nil; n = e.chain.get(n.parentHash) {
		if ancestors[n.hash] {
			return n
		}
	}
	return nil
}

// publishNode advances the canonical tip and installs one atomic snapshot.
func (e *Engine) publishNode(node *blockNode) {
	snap := Snapshot{
		BlockNumber: node.number,
		BlockHash:   node.hash,
		Tokens:      make(map[string]TokenMetrics, len(node.tokenStates)),
	}
	for tokenID, st := range node.tokenStates {
		win := node.windows[tokenID]
		snap.Tokens[tokenID] = st.snapshot(win.activeCount(), win.volumeSum())
	}
	e.snapshots[node.number] = snap
	e.headNumber = node.number
	e.headHash = node.hash
	delete(e.pending, node.number)

	keep := e.cfg.HistoryBlocks
	if node.number > keep {
		cutoff := node.number - keep
		for h := range e.snapshots {
			if h < cutoff {
				delete(e.snapshots, h)
			}
		}
		e.chain.pruneBelow(cutoff)
		// Buffered blocks/events below the window can never connect anymore.
		for h := range e.pending {
			if h < cutoff {
				delete(e.pending, h)
			}
		}
		for hash, b := range e.pendingBlocks {
			if b.Number < cutoff {
				delete(e.pendingBlocks, hash)
			}
		}
		for hash, num := range e.rejectedForks {
			if num < cutoff {
				delete(e.rejectedForks, hash)
			}
		}
	}
}

// applyCanonical re-applies one recovered durable block during startup.
func (e *Engine) applyCanonical(b Block) error {
	if e.chain.has(b.Hash) {
		return nil
	}
	node, err := e.buildNode(b)
	if err != nil {
		return err
	}
	e.chain.add(node)
	e.publishNode(node)
	return nil
}

// maybeCheckpoint durably snapshots the oldest retained block (the
// retention floor at tip-HistoryBlocks) once it advances past the last
// checkpoint, then drops WAL blocks the image covers. Imaging the floor
// rather than the tip guarantees the image never overtakes the retained
// window: recovery restores every snapshot that was queryable before a
// crash, and any reorg inside the window stays above the image.
func (e *Engine) maybeCheckpoint(tip *blockNode) error {
	if e.cfg.DataDir == "" {
		return nil
	}
	keep := e.cfg.HistoryBlocks
	if tip.number < keep {
		return nil
	}
	imageHeight := tip.number - keep
	if imageHeight == 0 || imageHeight < e.checkpointHeight+keep {
		return nil
	}
	image := tip
	for i := uint64(0); i < keep; i++ {
		image = e.chain.get(image.parentHash)
		if image == nil {
			return nil
		}
	}
	raw, err := encodeCheckpoint(image)
	if err != nil {
		return err
	}
	if err := e.store.saveCheckpoint(raw); err != nil {
		return err
	}
	e.checkpointHeight = image.number
	e.emitHook("checkpoint.image")
	// The image covers everything up to tip; keep only newer WAL blocks
	// reachable from the durable head (relevant when a reorg committed a
	// head beyond this checkpoint in the same replay).
	if err := e.store.compactWal(e.diskHeadHash, image.number); err != nil {
		return err
	}
	e.emitHook("checkpoint.compact")
	return nil
}
