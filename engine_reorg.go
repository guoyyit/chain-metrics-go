package chainmetrics

import "sort"

// activate publishes a node that directly extends the canonical head.
func (e *Engine) activate(node *blockNode) error {
	if e.headHash != "" && node.number != e.headNumber+1 {
		return nil
	}
	// Durable first: WAL append (fsync) then the atomic head flip. Only then
	// is the block published in memory, so a crash can never leave memory
	// ahead of disk.
	if err := e.store.persistBlock(node.block); err != nil {
		return err
	}
	if err := e.store.saveHead(node.number, node.hash); err != nil {
		return err
	}
	e.publishNode(node)
	e.pruneDeadForks()
	return e.maybeCheckpoint(node, nil)
}

// reorgTo atomically switches the canonical tip from the current head to node,
// which shares the retained ancestor fork with the old chain and is strictly
// longer. The durable switch order makes a crash at every point recover to a
// state identical to an uninterrupted run:
//
//  1. append new-branch blocks to the WAL (fsync each; head still names the
//     old tip, so the new records are uncommitted and ignored on restart);
//  2. rewrite the WAL dropping the old branch above the fork (the fork itself
//     and everything below stay; both branches are in the same file up to the
//     switch, and either rename outcome is recoverable);
//  3. flip head.json to the new tip;
//  4. install the new snapshots in memory (queries hold the engine lock and
//     therefore see either the complete old or the complete new chain);
//  5. advance the state image / compact the WAL following the same rule as the
//     linear path.
func (e *Engine) reorgTo(node, fork *blockNode) error {
	// New branch above the fork, ascending (fork excluded).
	var seq []*blockNode
	for n := node; n != nil && n.number > fork.number; n = e.chain.get(n.parentHash) {
		seq = append(seq, n)
	}
	for i, j := 0, len(seq)-1; i < j; i, j = i+1, j-1 {
		seq[i], seq[j] = seq[j], seq[i]
	}

	// Phase 1: durably stage the new branch; head still anchors the old tip.
	for _, n := range seq {
		if err := e.store.persistBlock(n.block); err != nil {
			return err
		}
		if e.crashHook != nil {
			if err := e.crashHook("reorg_wal_append"); err != nil {
				return err
			}
		}
	}

	// Phase 2: drop old-branch records above the fork from the WAL. Records
	// shared with the new branch (height <= fork.number) are kept.
	forkHeight := fork.number
	keepNew := make(map[string]bool, len(seq))
	for _, n := range seq {
		keepNew[n.hash] = true
	}
	if err := e.store.rewriteWAL(func(pb persistedBlock) bool {
		if pb.Number <= forkHeight {
			return true
		}
		return keepNew[pb.Hash]
	}); err != nil {
		return err
	}
	if e.crashHook != nil {
		if err := e.crashHook("reorg_wal_rewrite"); err != nil {
			return err
		}
	}

	// Phase 3: commit the switch with an atomic head pointer flip.
	if err := e.store.saveHead(node.number, node.hash); err != nil {
		return err
	}
	if e.crashHook != nil {
		if err := e.crashHook("reorg_head_flip"); err != nil {
			return err
		}
	}

	// Phase 4: publish the new branch in memory as one locked critical
	// section; concurrent queries can never observe a mixed chain.
	for h := e.headNumber; h > forkHeight; h-- {
		delete(e.snapshots, h)
	}
	e.headNumber = forkHeight
	e.headHash = fork.hash
	for _, n := range seq {
		e.publishNode(n)
	}
	e.pruneDeadForks()

	// Phase 5: image advancement / WAL compaction under the new tip.
	if err := e.maybeCheckpoint(node, []*blockNode{fork}); err != nil {
		return err
	}
	if e.crashHook != nil {
		if err := e.crashHook("reorg_after_checkpoint"); err != nil {
			return err
		}
	}
	return nil
}

// pruneDeadForks removes in-memory nodes that belong neither to the canonical
// chain nor to a still-buffered orphan branch, bounding tree growth across
// repeated reorganizations.
func (e *Engine) pruneDeadForks() {
	keep := make(map[string]bool)
	for h := e.chain.get(e.headHash); h != nil; h = e.chain.get(h.parentHash) {
		keep[h.hash] = true
	}
	for _, b := range e.pendingBlocks {
		for n := e.chain.get(b.ParentHash); n != nil; n = e.chain.get(n.parentHash) {
			keep[n.hash] = true
		}
	}
	for hash, n := range e.chain.nodes {
		if !keep[hash] && n.number <= e.headNumber {
			delete(e.chain.nodes, hash)
		}
	}
}

// publishNode advances the canonical tip and installs one atomic snapshot.
func (e *Engine) publishNode(node *blockNode) {
	e.installSnapshot(node)
	e.deletePendingBelow(node.number)
	e.enforceRetention(node.number)
}

// installSnapshot advances the tip pointer and stores the snapshot map entry.
// It performs no retention pruning, which recovery defers until all records
// have been replayed.
func (e *Engine) installSnapshot(node *blockNode) {
	e.publishSnapshotEntry(node)
	e.headNumber = node.number
	e.headHash = node.hash
}

// publishSnapshotEntry stores the node's snapshot without touching the head.
func (e *Engine) publishSnapshotEntry(node *blockNode) {
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
}

func (e *Engine) deletePendingBelow(head uint64) {
	delete(e.pending, head)
}

// rollbackLimit is the oldest canonical height still available for forking:
// head - HistoryBlocks (clamped at 1). A fork point at this exact height
// performs a reorganization exactly HistoryBlocks blocks deep and must
// succeed; only strictly older fork points are rejected.
func (e *Engine) rollbackLimit() uint64 {
	r := e.cfg.HistoryBlocks
	if e.headNumber <= r {
		return 1
	}
	return e.headNumber - r
}

// enforceRetention bounds snapshots, loose events, orphan blocks and the block
// tree to the supported rollback window below head.
func (e *Engine) enforceRetention(head uint64) {
	r := e.cfg.HistoryBlocks
	if head < r {
		return
	}
	// Retained snapshot window is [head-r, head]: HistoryBlocks+1 heights,
	// i.e. the rollback boundary block plus everything rewritable above it.
	// The state image snapshot at checkpointHeight may sit one height lower
	// right after a deep reorganization advanced the finalized boundary; keep
	// it so the image's published block stays queryable until the tip advances.
	snapshotCutoff := head - r
	for h := range e.snapshots {
		if h < snapshotCutoff && h != e.checkpointHeight && !e.envelopeHeights[h] {
			delete(e.snapshots, h)
		}
	}
	for h := range e.pending {
		if h < snapshotCutoff {
			delete(e.pending, h)
		}
	}
	for hash, b := range e.pendingBlocks {
		if b.Number < snapshotCutoff {
			delete(e.pendingBlocks, hash)
		}
	}
	for hash, h := range e.unreachableParents {
		if h < e.rollbackLimit() {
			delete(e.unreachableParents, hash)
		}
	}
	// Non-canonical nodes below the rollback limit are dead; canonical
	// ancestors and fork siblings at the boundary height must survive so
	// boundary-depth reorganizations stay detectable and executable.
	limit := e.rollbackLimit()
	canonical := make(map[string]bool)
	for n := e.chain.get(e.headHash); n != nil; n = e.chain.get(n.parentHash) {
		if n.number < limit {
			break
		}
		canonical[n.hash] = true
	}
	for hash, n := range e.chain.nodes {
		if n.number < limit && !canonical[hash] {
			delete(e.chain.nodes, hash)
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

// maybeCheckpoint advances the durable state image once the head reaches
// head-HistoryBlocks. The image sits exactly at the oldest supported rollback
// boundary; canonical nodes between the previous image and the new one are
// embedded (with full state) in the image envelope so their snapshots stay
// recoverable -- in particular a reorganization fork point whose own parent
// has already been finalized. The image switch precedes WAL compaction, so
// either crash point leaves a fully recoverable on-disk state.
func (e *Engine) maybeCheckpoint(tip *blockNode, extraEmbedded []*blockNode) error {
	if e.cfg.DataDir == "" {
		return nil
	}
	r := e.cfg.HistoryBlocks
	if tip.number < r {
		return nil
	}
	// Image boundary = head - HistoryBlocks. Everything at or below it is
	// finalized; the envelope embeds the canonical nodes the recovery window
	// must still be able to publish.
	finalizedHeight := tip.number - r
	finalizedNode := tip
	for finalizedNode != nil && finalizedNode.number > finalizedHeight {
		finalizedNode = e.chain.get(finalizedNode.parentHash)
	}
	if finalizedNode == nil || finalizedNode.number <= e.checkpointHeight {
		return nil
	}

	// Embedded canonical nodes:
	//   - every canonical node in (oldImageHeight, newImageHeight) (these
	//     currently live in the in-memory tree and the WAL, but the WAL for
	//     them will be compacted, so carry full state in the envelope);
	//   - extraEmbedded: the reorganization fork node when the new image
	//     advances past it, since its parent left the tree.
	embeddedByHash := make(map[string]*blockNode)
	for n := finalizedNode; n != nil && n.number > e.checkpointHeight; n = e.chain.get(n.parentHash) {
		if n.number < finalizedNode.number {
			embeddedByHash[n.hash] = n
		}
	}
	for _, n := range extraEmbedded {
		if n.number < finalizedNode.number {
			embeddedByHash[n.hash] = n
		}
	}
	var embedded []*blockNode
	for _, n := range embeddedByHash {
		embedded = append(embedded, n)
	}
	sort.Slice(embedded, func(i, j int) bool { return embedded[i].number < embedded[j].number })

	raw, err := encodeCheckpoint(finalizedNode, embedded)
	if err != nil {
		return err
	}
	if err := e.store.saveCheckpoint(raw); err != nil {
		return err
	}
	if e.crashHook != nil {
		if err := e.crashHook("checkpoint_saved"); err != nil {
			return err
		}
	}
	// The image now durably contains every block at or below its height; the
	// WAL keeps only strictly higher blocks. Crash before rename: old image +
	// old WAL. Crash after: new image + shortened WAL. Both recover fully.
	if err := e.store.rewriteWAL(func(pb persistedBlock) bool {
		// The new envelope embeds every canonical node below the image; the
		// WAL only needs strictly higher blocks plus same-height carry records.
		if pb.Number < finalizedNode.number {
			return false
		}
		return true
	}); err != nil {
		return err
	}
	e.checkpointHeight = finalizedNode.number
	e.checkpointHash = finalizedNode.hash
	e.envelopeHeights = make(map[uint64]bool)
	for _, n := range embedded {
		e.envelopeHeights[n.number] = true
	}
	return nil
}
