package chainmetrics

// activate publishes a node that directly extends the canonical head.
func (e *Engine) activate(node *blockNode) error {
	if e.headHash != "" && node.number != e.headNumber+1 {
		return nil
	}
	if err := e.store.persistBlock(node.block); err != nil {
		return err
	}
	e.publishNode(node)
	if err := e.store.saveHead(node.number, node.hash); err != nil {
		return err
	}
	return e.maybeCheckpoint(node)
}

// maybeReorgTo switches the canonical chain when node forms a branch forking
// below the current tip. State is rebuilt from the fork point, so results are
// identical to a fresh replay from that ancestor.
func (e *Engine) maybeReorgTo(node *blockNode) error {
	fork := e.findFork(node)
	if fork == nil {
		return nil
	}
	// Longest-chain rule: adopt the branch only once it overtakes the current
	// canonical tip. An equal-height sibling is retained but not activated.
	if node.number <= e.headNumber {
		return nil
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

	rewindFrom := fork.number + 1
	if err := e.store.truncateFrom(rewindFrom); err != nil {
		return err
	}
	for h := e.headNumber; h >= rewindFrom; h-- {
		delete(e.snapshots, h)
	}
	e.headNumber = fork.number
	e.headHash = fork.hash

	for i := len(seq) - 1; i >= 0; i-- {
		n := seq[i]
		if err := e.store.persistBlock(n.block); err != nil {
			return err
		}
		e.publishNode(n)
		if err := e.store.saveHead(n.number, n.hash); err != nil {
			return err
		}
		if err := e.maybeCheckpoint(n); err != nil {
			return err
		}
	}
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

// maybeCheckpoint durably snapshots the tip and truncates older WAL blocks
// once the tip is more than HistoryBlocks beyond the last checkpoint. This
// bounds durable storage while keeping the full recoverable chain on disk.
func (e *Engine) maybeCheckpoint(tip *blockNode) error {
	if e.cfg.DataDir == "" {
		return nil
	}
	if tip.number < e.checkpointHeight+e.cfg.HistoryBlocks {
		return nil
	}
	raw, err := encodeCheckpoint(tip)
	if err != nil {
		return err
	}
	if err := e.store.saveCheckpoint(raw); err != nil {
		return err
	}
	// The checkpoint captures all state up to and including tip, so the WAL
	// can be emptied entirely (recovery restarts from the checkpoint image).
	if err := e.store.truncateFrom(0); err != nil {
		return err
	}
	if err := e.store.saveHead(tip.number, tip.hash); err != nil {
		return err
	}
	e.checkpointHeight = tip.number
	return nil
}
