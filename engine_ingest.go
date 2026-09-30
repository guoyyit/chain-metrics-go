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
	node, err := e.buildNode(b)
	if err != nil {
		// Parent unknown: buffer the block until its ancestor arrives.
		if e.chain.get(b.ParentHash) == nil {
			e.pendingBlocks[b.Hash] = b
			return nil
		}
		return err
	}
	e.chain.add(node)

	// Genesis or the direct next tip: activate immediately.
	if e.headHash == "" && (b.Number == 0 || b.ParentHash == "") {
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
	// Otherwise it belongs to a fork (or arrived out of order); attempt a
	// reorganization only if it connects to a known ancestor.
	if e.chain.get(b.ParentHash) != nil {
		if err := e.maybeReorgTo(node); err != nil {
			return err
		}
	} else {
		e.pendingBlocks[b.Hash] = b
		return nil
	}
	return e.flushPending()
}

// flushPending activates buffered blocks whose parents have since arrived.
func (e *Engine) flushPending() error {
	for {
		progress := false
		for hash, b := range e.pendingBlocks {
			if e.chain.has(hash) {
				delete(e.pendingBlocks, hash)
				progress = true
				continue
			}
			if e.chain.get(b.ParentHash) == nil {
				continue
			}
			delete(e.pendingBlocks, hash)
			progress = true
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
			// The new node may now directly extend the freshly advanced tip.
			if b.ParentHash == e.headHash && b.Number == e.headNumber+1 {
				if err := e.activate(node); err != nil {
					return err
				}
				continue
			}
			if err := e.maybeReorgTo(node); err != nil {
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
