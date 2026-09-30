package metrics

import (
	"math/big"
	"math/rand/v2"
)

// holderEntry is one ranked holder.
type holderEntry struct {
	addr    string
	balance *big.Int
}

// holderNode is a randomized treap node. The BST key orders holders by
// ascending balance, ties broken by descending address; therefore the
// right subtree always outranks its parent. An address-index map makes
// every balance change a delete+insert in expected O(log N), so the
// top-K ranking stays exactly correct even when a current leader's
// balance drops (which a capped K-heap cannot handle).
type holderNode struct {
	entry holderEntry
	left  *holderNode
	right *holderNode
	prio  uint32
}

// holderIndex is an exact, incrementally maintained ranking of all
// live holders. Memory is O(live holders), the same order as the
// balances map; zero-balance addresses are removed.
type holderIndex struct {
	root  *holderNode
	nodes map[string]*holderNode
}

func newHolderIndex() *holderIndex {
	return &holderIndex{nodes: make(map[string]*holderNode)}
}

// ranksBelow reports whether a is ranked below b.
func ranksBelow(a, b holderEntry) bool {
	c := a.balance.Cmp(b.balance)
	if c != 0 {
		return c < 0
	}
	return a.addr > b.addr
}

func rotateRight(n *holderNode) *holderNode {
	child := n.left
	n.left = child.right
	child.right = n
	return child
}

func rotateLeft(n *holderNode) *holderNode {
	child := n.right
	n.right = child.left
	child.left = n
	return child
}

func insertNode(n, node *holderNode) *holderNode {
	if n == nil {
		return node
	}
	if ranksBelow(node.entry, n.entry) {
		n.left = insertNode(n.left, node)
		if n.left.prio < n.prio {
			n = rotateRight(n)
		}
	} else {
		n.right = insertNode(n.right, node)
		if n.right.prio < n.prio {
			n = rotateLeft(n)
		}
	}
	return n
}

func eraseNode(n *holderNode, entry holderEntry) *holderNode {
	if n == nil {
		return nil
	}
	switch {
	case ranksBelow(entry, n.entry):
		n.left = eraseNode(n.left, entry)
	case ranksBelow(n.entry, entry):
		n.right = eraseNode(n.right, entry)
	default:
		switch {
		case n.left == nil:
			return n.right
		case n.right == nil:
			return n.left
		default:
			if n.left.prio < n.right.prio {
				n = rotateRight(n)
				n.right = eraseNode(n.right, entry)
			} else {
				n = rotateLeft(n)
				n.left = eraseNode(n.left, entry)
			}
		}
	}
	return n
}

// observe sets addr to the given non-zero balance, or removes it when
// the balance is nil/zero. Expected O(log N).
func (h *holderIndex) observe(addr string, balance *big.Int) {
	if old, ok := h.nodes[addr]; ok {
		h.root = eraseNode(h.root, old.entry)
		delete(h.nodes, addr)
	}
	if balance == nil || balance.Sign() == 0 {
		return
	}
	node := &holderNode{
		entry: holderEntry{addr: addr, balance: new(big.Int).Set(balance)},
		prio:  rand.Uint32(),
	}
	h.root = insertNode(h.root, node)
	h.nodes[addr] = node
}

func (h *holderIndex) len() int { return len(h.nodes) }

// top returns at most k holders from rank 1 downward. O(k + log N).
func (h *holderIndex) top(k int) []holderEntry {
	if k <= 0 || h.root == nil {
		return nil
	}
	out := make([]holderEntry, 0, k)
	var walk func(*holderNode)
	walk = func(n *holderNode) {
		if n == nil || len(out) >= k {
			return
		}
		// Larger balances live in the right subtree; visit it first.
		walk(n.right)
		if len(out) < k {
			out = append(out, holderEntry{
				addr:    n.entry.addr,
				balance: new(big.Int).Set(n.entry.balance),
			})
		}
		walk(n.left)
	}
	walk(h.root)
	return out
}
