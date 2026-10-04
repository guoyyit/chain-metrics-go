package chainmetrics

// blockNode is one committed block and its per-token state delta.
type blockNode struct {
	number     uint64
	hash       string
	parentHash string
	block      Block

	// tokenStates are the states after applying this block, keyed by token id.
	tokenStates map[string]*tokenState
	// windows are the rolling windows after applying this block.
	windows map[string]*rollingWindow
}

// chainTree stores blocks by hash and tracks the canonical head.
type chainTree struct {
	nodes map[string]*blockNode
	head  *blockNode
}

func newChainTree() *chainTree {
	return &chainTree{nodes: make(map[string]*blockNode)}
}

// add inserts a node (which must have been built from its parent's state).
func (t *chainTree) add(node *blockNode) {
	t.nodes[node.hash] = node
}

// has reports whether a block hash is known.
func (t *chainTree) has(hash string) bool {
	_, ok := t.nodes[hash]
	return ok
}

// get returns the node for a hash.
func (t *chainTree) get(hash string) *blockNode {
	return t.nodes[hash]
}

// ancestors returns hash and its ancestors, ordered from hash down to (and
// including) the oldest node still present in the tree.
func (t *chainTree) ancestors(hash string) []*blockNode {
	var out []*blockNode
	node := t.nodes[hash]
	for node != nil {
		out = append(out, node)
		node = t.nodes[node.parentHash]
	}
	return out
}
