package chainmetrics

import "sort"

// recover rebuilds the in-memory canonical chain from the durable state
// image (if any) plus the WAL. Every on-disk state the engine can leave
// behind is self-consistent:
//
//  1. the state image captures the finalized boundary and embeds the
//     canonical blocks up to that boundary;
//  2. surviving WAL records contain every canonical block from the image up
//     to the head pointer;
//  3. head.json names the last fully committed block.
//
// A missing parent link while descending from the head marks a torn,
// uncommitted WAL append: the disconnected segment is discarded and the head
// is re-anchored at the latest fully connected block, so startup always
// succeeds in a consistent state.
func (e *Engine) recover() error {
	var cpNode *blockNode
	allBlocks := make(map[string]persistedBlock)
	var cpEmbedded []*blockNode
	embeddedHashes := make(map[string]bool)

	cpRaw, err := e.store.loadCheckpoint()
	if err != nil {
		return err
	}
	if cpRaw != nil {
		node, embedded, derr := decodeCheckpoint(cpRaw)
		if derr != nil {
			return derr
		}
		cpNode = node
		e.chain.add(node)
		e.checkpointHeight = node.number
		e.checkpointHash = node.hash
		cpEmbedded = embedded
		for _, en := range embedded {
			e.chain.add(en)
			allBlocks[en.hash] = blockToPersisted(en.block)
			embeddedHashes[en.hash] = true
			e.envelopeHeights[en.number] = true
		}
	}

	walBlocks, err := e.store.walBlocks()
	if err != nil {
		return err
	}
	for i := range walBlocks {
		pb := walBlocks[i]
		// The image node is authoritative for its own height. Records below
		// it are stale (old-branch garbage from the switch window) and are
		// dropped; records at the image height are dropped unless they name a
		// different hash (the carried reorganization fork block, which lives
		// one height below the image after a deep reorg).
		if cpNode != nil && pb.Number < cpNode.number {
			continue
		}
		if cpNode != nil && pb.Number == cpNode.number && pb.Hash == cpNode.hash {
			continue
		}
		allBlocks[pb.Hash] = pb
	}

	headNumber, headHash, hasHead, err := e.store.loadHead()
	if err != nil {
		return err
	}

	if cpNode != nil {
		// Anchor the tip pointer at the image so replay builds forward; the
		// image snapshot itself is installed after we know the final head, so
		// retention pruning cannot discard it.
		e.headNumber = cpNode.number
		e.headHash = cpNode.hash
	}
	// Publish snapshots carried in the image envelope. They are the only
	// durable copy of some retained heights (notably a deep-reorganization
	// fork point whose own parent was finalized), and each carries full state
	// so no parent replay is needed.
	if cpNode != nil {
		for _, en := range cpEmbedded {
			e.publishSnapshotEntry(en)
		}
	}
	ordered, err := e.connectedCanonicalBlocks(allBlocks, cpNode, embeddedHashes, headHash, hasHead)
	if err != nil {
		return err
	}
	for i := range ordered {
		b, err := ordered[i].toBlock()
		if err != nil {
			return err
		}
		if e.chain.has(b.Hash) {
			continue
		}
		if err := e.applyCanonical(b); err != nil {
			return err
		}
	}
	if cpNode != nil {
		// Publish the image's own snapshot. The replayed head (above the
		// image) stays the canonical tip; enforceRetention exempts this entry.
		e.publishSnapshotEntry(cpNode)
	}
	e.enforceRetention(e.headNumber)

	// Re-anchor the durable head when torn records were discarded, then remove
	// uncommitted/dead-branch WAL garbage so durable data stays bounded.
	if hasHead && (headHash != e.headHash || headNumber != e.headNumber) {
		if err := e.store.saveHead(e.headNumber, e.headHash); err != nil {
			return err
		}
	}
	return e.gcWAL()
}

// connectedCanonicalBlocks returns the canonical blocks above the state image
// in ascending order that are connected to the committed head.
func (e *Engine) connectedCanonicalBlocks(
	allBlocks map[string]persistedBlock,
	cpNode *blockNode,
	embeddedHashes map[string]bool,
	headHash string,
	hasHead bool,
) ([]persistedBlock, error) {
	if !hasHead {
		// Nothing was ever committed: replay every retained record by height
		// and hash (defensive ordering; canonical tie-breaks do not apply
		// without a head anchor).
		out := make([]persistedBlock, 0, len(allBlocks))
		for _, pb := range allBlocks {
			out = append(out, pb)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Number != out[j].Number {
				return out[i].Number < out[j].Number
			}
			return out[i].Hash < out[j].Hash
		})
		return out, nil
	}

	// Descend from the head toward the state image. The walk terminates when
	// the next parent is the image node (already loaded into the tree) or, in
	// WAL-only mode, when records run out at a genesis with an empty parent.
	// A genuinely missing parent splits off an uncommitted (torn) segment:
	// discard it and continue descending from the named parent hash.
	var desc []persistedBlock
	cursor := headHash
	for cursor != "" {
		node, ok := allBlocks[cursor]
		if !ok {
			break
		}
		if cpNode != nil && node.Hash == cpNode.hash {
			break
		}
		desc = append(desc, node)
		next := node.ParentHash
		if cpNode != nil && next == cpNode.hash {
			break // segment connects to the image node loaded in the tree
		}
		if embeddedHashes[next] {
			break // connects to a state-bearing embedded node
		}
		if next == "" {
			break // reached the genesis record: complete committed prefix
		}
		if _, parentKnown := allBlocks[next]; !parentKnown {
			break // disconnected from the image / from genesis: the whole
			// collected prefix belongs to an uncommitted WAL append.
		}
		cursor = next
	}

	out := make([]persistedBlock, len(desc))
	for i := range desc {
		out[len(desc)-1-i] = desc[i]
	}
	return out, nil
}

// gcWAL rewrites the WAL keeping only canonical blocks above the state image
// needed to reconstruct the chain up to the head.
func (e *Engine) gcWAL() error {
	if e.cfg.DataDir == "" {
		return nil
	}
	live := make(map[string]bool)
	for node := e.chain.get(e.headHash); node != nil; node = e.chain.get(node.parentHash) {
		if node.number <= e.checkpointHeight {
			break
		}
		live[node.hash] = true
	}
	return e.store.rewriteWAL(func(pb persistedBlock) bool {
		if pb.Number <= e.checkpointHeight {
			return false
		}
		return live[pb.Hash]
	})
}
