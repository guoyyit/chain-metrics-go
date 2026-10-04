package chainmetrics

import (
	"fmt"
	"math/big"
	"sort"
)

// PushBlock ingests a complete block. It is idempotent by block hash and
// transparently performs a reorganization when the block extends a fork whose
// tip overtakes the canonical chain.
func (e *Engine) PushBlock(b Block) error {
	if b.Hash == "" {
		return fmt.Errorf("block hash is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// Merge any previously buffered loose events into the block.
	if bucket := e.pending[b.Number]; bucket != nil {
		for hash, byToken := range bucket {
			if hash != "" && hash != b.Hash {
				continue
			}
			for tokenID, byIndex := range byToken {
				index := indexOf(b, tokenID)
				for idx, ev := range byIndex {
					if _, exists := index[idx]; exists {
						continue
					}
					b.Events = append(b.Events, ev)
					index[idx] = ev
				}
			}
		}
		delete(bucket, "")
		delete(bucket, b.Hash)
		if len(bucket) == 0 {
			delete(e.pending, b.Number)
		}
	}

	// Deduplicate within the block by (token,index) and apply canonical order.
	deduped := make([]Event, 0, len(b.Events))
	seen := make(map[[2]string]struct{})
	for _, ev := range b.Events {
		if ev.TokenID == "" || ev.Amount == nil {
			continue
		}
		ev.Block = b.Number
		key := [2]string{ev.TokenID, fmt.Sprintf("%d", ev.Index)}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, ev)
	}
	sort.SliceStable(deduped, func(i, j int) bool {
		return deduped[i].Index < deduped[j].Index
	})
	b.Events = deduped

	if e.chain.has(b.Hash) {
		return nil
	}
	isGenesis := e.headHash == "" && (b.Number == 0 || b.ParentHash == "")
	if !isGenesis && e.chain.get(b.ParentHash) == nil {
		// Parent unknown: either an out-of-order intermediate or a fork whose
		// base has already left the retained window. Buffer only blocks that
		// could still become part of a supported reorganization.
		if err := e.bufferOrRejectOrphan(b); err != nil {
			return err
		}
		return nil
	}
	node, err := e.buildNode(b)
	if err != nil {
		return err
	}
	e.chain.add(node)

	// Genesis or the direct next tip: activate immediately.
	if isGenesis {
		if err := e.activate(node); err != nil {
			return err
		}
		return e.flushPending()
	}
	if b.ParentHash == e.headHash && b.Number == e.headNumber+1 {
		if err := e.activate(node); err != nil {
			return err
		}
		return e.flushPending()
	}
	// Otherwise it belongs to a fork; the longest-chain rule and the rollback
	// boundary are enforced inside considerNode.
	if err := e.considerNode(node); err != nil {
		return err
	}
	return e.flushPending()
}

// bufferOrRejectOrphan stores a parent-unknown block for later cascading when
// its height could still participate in a supported reorganization. A block
// whose height is already below the rollback limit can never connect to a
// retained ancestor, so it is rejected with a distinguishable error instead
// of being silently held forever.
func (e *Engine) bufferOrRejectOrphan(b Block) error {
	// Walk the known ancestry of b through the tree and already-buffered
	// orphans to locate the eventual fork point. A block is buffered only when
	// the partial branch reaches a retained ancestor (fork height >= rollback
	// limit). A branch whose chain runs out below the limit -- even when its
	// later blocks have high heights -- is rejected, because it can never
	// produce a supported reorganization.
	limit := e.rollbackLimit()
	cursor := b
	for {
		if _, bad := e.unreachableParents[cursor.Hash]; bad {
			return reorgTooDeepError(cursor.Number, limit, e.headNumber)
		}
		if _, bad := e.unreachableParents[cursor.ParentHash]; bad {
			e.unreachableParents[cursor.Hash] = cursor.Number
			return reorgTooDeepError(cursor.Number-1, limit, e.headNumber)
		}
		// A retained canonical ancestor within the rollback window makes the
		// branch connectable. A retained *fork* node just continues the walk.
		if parent := e.chain.get(cursor.ParentHash); parent != nil {
			if e.isCanonical(parent.hash) {
				if parent.number < limit {
					e.unreachableParents[cursor.Hash] = cursor.Number
					return reorgTooDeepError(parent.number, limit, e.headNumber)
				}
				e.pendingBlocks[b.Hash] = b
				return nil
			}
			cursor = parent.block
			continue
		}
		if parent, ok := e.pendingBlocks[cursor.ParentHash]; ok {
			cursor = parent
			continue
		}
		// The base of the known chain is this cursor; its missing parent is
		// the effective fork point.
		forkHeight := cursor.Number - 1
		if forkHeight < limit {
			e.unreachableParents[cursor.Hash] = cursor.Number
			return reorgTooDeepError(forkHeight, limit, e.headNumber)
		}
		// Missing parent but the eventual fork would still be inside the
		// window: a genuinely out-of-order intermediate block.
		e.pendingBlocks[b.Hash] = b
		return nil
	}
}

// isCanonical reports whether hash is an ancestor of (or is) the head.
func (e *Engine) isCanonical(hash string) bool {
	for n := e.chain.get(e.headHash); n != nil; n = e.chain.get(n.parentHash) {
		if n.hash == hash {
			return true
		}
	}
	return false
}

// considerNode activates or reorganizes to a freshly connected node under the
// longest-chain rule. Forks that fork below the supported rollback boundary
// are rejected with ErrReorgTooDeep without touching any published state.
func (e *Engine) considerNode(node *blockNode) error {
	if e.headHash == "" {
		return nil
	}
	// Direct canonical extension is handled by the caller before this point.
	if node.number <= e.headNumber {
		// Equal-height or shorter sibling: retain but do not activate.
		return nil
	}
	fork, err := e.locateFork(node)
	if err != nil {
		return err
	}
	if fork == nil {
		return ErrNoCommonAncestor
	}
	return e.reorgTo(node, fork)
}

// locateFork returns the common ancestor of node and the canonical head, or
// an error when the fork point is outside the retained window.
func (e *Engine) locateFork(node *blockNode) (*blockNode, error) {
	limit := e.rollbackLimit()
	headAncestors := make(map[string]bool)
	for h := e.chain.get(e.headHash); h != nil; h = e.chain.get(h.parentHash) {
		headAncestors[h.hash] = true
	}
	for n := node; n != nil; n = e.chain.get(n.parentHash) {
		if headAncestors[n.hash] {
			if n.number < limit {
				return nil, reorgTooDeepError(n.number, limit, e.headNumber)
			}
			return n, nil
		}
	}
	// The branch runs off the bottom of the retained tree. If its base height
	// is already below the boundary the fork cannot possibly be supported.
	base := node
	for p := e.chain.get(base.parentHash); p != nil; p = e.chain.get(p.parentHash) {
		base = p
	}
	if base.number < limit {
		return nil, reorgTooDeepError(base.number-1, limit, e.headNumber)
	}
	return nil, ErrNoCommonAncestor
}

// effectiveFork walks the ancestry of a block (whose parent is known to be in
// the tree) and returns the first ancestor that lies on the canonical chain,
// or nil when the branch does not connect to a retained canonical node.
func (e *Engine) effectiveFork(b Block) *blockNode {
	headAncestors := make(map[string]bool)
	for h := e.chain.get(e.headHash); h != nil; h = e.chain.get(h.parentHash) {
		headAncestors[h.hash] = true
	}
	for n := e.chain.get(b.ParentHash); n != nil; n = e.chain.get(n.parentHash) {
		if headAncestors[n.hash] {
			return n
		}
	}
	return nil
}

// flushPending activates buffered blocks whose parents have since arrived.
func (e *Engine) flushPending() error {
	for {
		progress := false
		type candidate struct {
			hash string
			b    Block
		}
		var ready []candidate
		for hash, b := range e.pendingBlocks {
			if e.chain.has(hash) {
				delete(e.pendingBlocks, hash)
				progress = true
				continue
			}
			if e.chain.get(b.ParentHash) == nil {
				if b.Number < e.rollbackLimit() {
					ready = append(ready, candidate{hash, b})
				}
				continue
			}
			ready = append(ready, candidate{hash, b})
		}
		for _, c := range ready {
			hash, b := c.hash, c.b
			delete(e.pendingBlocks, hash)
			progress = true
			if e.chain.get(b.ParentHash) == nil {
				// Became unreachable through retention: reject explicitly.
				return reorgTooDeepError(b.Number-1, e.rollbackLimit(), e.headNumber)
			}
			// The retained ancestor may be a finalized node below the rollback
			// limit that was only discoverable now that intermediates joined:
			// re-check the effective fork depth before building the branch.
			if fork := e.effectiveFork(b); fork != nil && fork.number < e.rollbackLimit() {
				e.unreachableParents[b.ParentHash] = fork.number
				return reorgTooDeepError(fork.number, e.rollbackLimit(), e.headNumber)
			}
			node, err := e.buildNode(b)
			if err != nil {
				return err
			}
			e.chain.add(node)
			if b.ParentHash == e.headHash && b.Number == e.headNumber+1 {
				if err := e.activate(node); err != nil {
					return err
				}
				continue
			}
			if err := e.considerNode(node); err != nil {
				return err
			}
		}
		if !progress {
			return nil
		}
	}
}

// bufferEvent stores a loose event by (height, hash, token, index).
func (e *Engine) bufferEvent(ev Event, hash string) error {
	byHash, ok := e.pending[ev.Block]
	if !ok {
		byHash = make(map[string]map[string]map[uint32]Event)
		e.pending[ev.Block] = byHash
	}
	byToken, ok := byHash[hash]
	if !ok {
		byToken = make(map[string]map[uint32]Event)
		byHash[hash] = byToken
	}
	byIndex, ok := byToken[ev.TokenID]
	if !ok {
		byIndex = make(map[uint32]Event)
		byToken[ev.TokenID] = byIndex
	}
	byIndex[ev.Index] = ev
	return nil
}

// indexOf returns the (token -> index -> event) map already in block b.
func indexOf(b Block, tokenID string) map[uint32]Event {
	out := make(map[uint32]Event)
	for _, ev := range b.Events {
		if ev.TokenID == tokenID {
			out[ev.Index] = ev
		}
	}
	return out
}

// buildNode constructs a node by cloning parent state and applying its events.
func (e *Engine) buildNode(b Block) (*blockNode, error) {
	node := &blockNode{
		number:      b.Number,
		hash:        b.Hash,
		parentHash:  b.ParentHash,
		block:       b,
		tokenStates: make(map[string]*tokenState),
		windows:     make(map[string]*rollingWindow),
	}
	var parent *blockNode
	if b.ParentHash != "" {
		parent = e.chain.get(b.ParentHash)
		if parent == nil {
			return nil, fmt.Errorf("unknown parent %s for block %s", b.ParentHash, b.Hash)
		}
	}

	tokens := groupedTokenIDs(b)
	for _, tokenID := range tokens {
		st, win := e.forkState(parent, tokenID)
		var active []string
		volume := new(big.Int)
		for _, ev := range b.Events {
			if ev.TokenID != tokenID {
				continue
			}
			st.apply(ev)
			if ev.From != zeroAddress {
				active = append(active, ev.From)
			}
			if ev.To != zeroAddress {
				active = append(active, ev.To)
			}
			if ev.Type == Transfer {
				volume.Add(volume, ev.Amount)
			}
		}
		win.push(b.Number, active, volume)
		win.truncate(b.Number)
		node.tokenStates[tokenID] = st
		node.windows[tokenID] = win
	}
	if parent != nil {
		for tokenID, st := range parent.tokenStates {
			if _, exists := node.tokenStates[tokenID]; exists {
				continue
			}
			node.tokenStates[tokenID] = st.clone()
			node.windows[tokenID] = parent.windows[tokenID].clone()
		}
	}
	return node, nil
}

// forkState returns state/window for a token inherited from parent.
func (e *Engine) forkState(parent *blockNode, tokenID string) (*tokenState, *rollingWindow) {
	if parent != nil {
		if st, ok := parent.tokenStates[tokenID]; ok {
			return st.clone(), parent.windows[tokenID].clone()
		}
	}
	cfg := e.cfg.Metrics
	return newTokenState(tokenID, cfg),
		newRollingWindow(tokenID, cfg.ActiveWindowBlocks, cfg.VolumeWindowBlocks)
}

// groupedTokenIDs returns the sorted distinct token ids present in a block.
func groupedTokenIDs(b Block) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, ev := range b.Events {
		if _, ok := seen[ev.TokenID]; !ok {
			seen[ev.TokenID] = struct{}{}
			out = append(out, ev.TokenID)
		}
	}
	sort.Strings(out)
	return out
}
