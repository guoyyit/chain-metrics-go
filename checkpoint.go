package chainmetrics

import (
	"encoding/json"
	"math/big"
)

const checkpointName = "checkpoint.json"

// cpBalance is one persisted holder balance.
type cpBalance struct {
	Address string
	Balance string
}

// cpTier is one persisted balance tier definition.
type cpTier struct {
	Name string
	Min  string
	Max  string // empty means +infinity
}

// cpToken is the persisted metric state of one token.
type cpToken struct {
	TokenID     string
	Supply      string
	HolderCount int64
	TransferCnt int64
	TopN        int
	Tiers       []cpTier
	TierCounts  map[string]int64
	Balances    []cpBalance

	ActiveSize uint64
	VolumeSize uint64
	Entries    []cpWindowEntry
}

// cpWindowEntry is one persisted rolling-window entry.
type cpWindowEntry struct {
	Block  uint64
	Active []string
	Volume string
}

// checkpoint is a fully self-contained state image at one block.
type checkpoint struct {
	Number     uint64
	Hash       string
	ParentHash string
	Tokens     map[string]cpToken

	// FinalizedBlocks are canonical blocks embedded in the envelope when the
	// WAL was compacted. Each carries its full per-token state image, so
	// recovery can publish every retained snapshot -- including a
	// reorganization fork point whose parent left the durable window --
	// without replaying the (no longer stored) parent. The image and these
	// blocks are published atomically.
	FinalizedBlocks []cpFinalizedBlock
}

// cpFinalizedBlock is one embedded canonical block plus its complete state.
type cpFinalizedBlock struct {
	Block  persistedBlock
	Tokens map[string]cpToken
}

// encodeTokenState serializes one token's state and rolling window.
func encodeTokenState(node *blockNode, tokenID string, st *tokenState) cpToken {
	t := cpToken{
		TokenID:     st.tokenID,
		Supply:      st.supply.String(),
		HolderCount: st.holderCount,
		TransferCnt: st.transferCnt,
		TopN:        st.topN,
		TierCounts:  st.tierCounts,
	}
	for _, tier := range st.tiers {
		ct := cpTier{Name: tier.Name, Min: tier.Min.String()}
		if tier.Max != nil {
			ct.Max = tier.Max.String()
		}
		t.Tiers = append(t.Tiers, ct)
	}
	for addr, bal := range st.balances {
		t.Balances = append(t.Balances, cpBalance{Address: addr, Balance: bal.String()})
	}
	if win := node.windows[tokenID]; win != nil {
		t.ActiveSize = win.activeSize
		t.VolumeSize = win.volumeSize
		for _, e := range win.entries {
			we := cpWindowEntry{Block: e.block, Volume: e.volume.String()}
			for addr := range e.active {
				we.Active = append(we.Active, addr)
			}
			t.Entries = append(t.Entries, we)
		}
	}
	return t
}

// encodeCheckpoint serializes the full state image at node together with the
// embedded canonical nodes (each carrying its own state) that must survive the
// WAL compaction performed in the same atomic switch.
func encodeCheckpoint(node *blockNode, embedded []*blockNode) ([]byte, error) {
	cp := checkpoint{
		Number:     node.number,
		Hash:       node.hash,
		ParentHash: node.parentHash,
		Tokens:     make(map[string]cpToken, len(node.tokenStates)),
	}
	for tokenID, st := range node.tokenStates {
		cp.Tokens[tokenID] = encodeTokenState(node, tokenID, st)
	}
	for _, en := range embedded {
		fe := cpFinalizedBlock{
			Block:  blockToPersisted(en.block),
			Tokens: make(map[string]cpToken, len(en.tokenStates)),
		}
		for tokenID, st := range en.tokenStates {
			fe.Tokens[tokenID] = encodeTokenState(en, tokenID, st)
		}
		cp.FinalizedBlocks = append(cp.FinalizedBlocks, fe)
	}
	return json.MarshalIndent(cp, "", "  ")
}

// decodeTokenState reconstructs one token's state and rolling window.
func decodeTokenState(tokenID string, t cpToken) (*tokenState, *rollingWindow, error) {
	supply, ok := new(big.Int).SetString(t.Supply, 10)
	if !ok {
		return nil, nil, errBadAmount(t.Supply)
	}
	var tiers []BalanceTier
	for _, ct := range t.Tiers {
		min, ok := new(big.Int).SetString(ct.Min, 10)
		if !ok {
			return nil, nil, errBadAmount(ct.Min)
		}
		tier := BalanceTier{Name: ct.Name, Min: min}
		if ct.Max != "" {
			max, ok := new(big.Int).SetString(ct.Max, 10)
			if !ok {
				return nil, nil, errBadAmount(ct.Max)
			}
			tier.Max = max
		}
		tiers = append(tiers, tier)
	}
	st := &tokenState{
		tokenID:     t.TokenID,
		balances:    make(map[string]*big.Int, len(t.Balances)),
		supply:      supply,
		holderCount: t.HolderCount,
		transferCnt: t.TransferCnt,
		tierCounts:  t.TierCounts,
		topN:        t.TopN,
		tiers:       tiers,
	}
	for _, cb := range t.Balances {
		bal, ok := new(big.Int).SetString(cb.Balance, 10)
		if !ok {
			return nil, nil, errBadAmount(cb.Balance)
		}
		st.balances[cb.Address] = bal
	}
	win := &rollingWindow{
		tokenID:    tokenID,
		activeSize: t.ActiveSize,
		volumeSize: t.VolumeSize,
	}
	for _, ce := range t.Entries {
		vol, ok := new(big.Int).SetString(ce.Volume, 10)
		if !ok {
			return nil, nil, errBadAmount(ce.Volume)
		}
		entry := windowEntry{block: ce.Block, active: make(map[string]struct{}, len(ce.Active)), volume: vol}
		for _, a := range ce.Active {
			entry.active[a] = struct{}{}
		}
		win.entries = append(win.entries, entry)
	}
	return st, win, nil
}

// decodeCheckpoint reconstructs the image node and every embedded node.
func decodeCheckpoint(raw []byte) (*blockNode, []*blockNode, error) {
	var cp checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, nil, err
	}
	buildNode := func(number uint64, hash, parent string, tokens map[string]cpToken, b Block) (*blockNode, error) {
		states := make(map[string]*tokenState, len(tokens))
		windows := make(map[string]*rollingWindow, len(tokens))
		for tokenID, ct := range tokens {
			st, win, err := decodeTokenState(tokenID, ct)
			if err != nil {
				return nil, err
			}
			states[tokenID] = st
			windows[tokenID] = win
		}
		return &blockNode{
			number:      number,
			hash:        hash,
			parentHash:  parent,
			block:       b,
			tokenStates: states,
			windows:     windows,
		}, nil
	}

	root, err := buildNode(cp.Number, cp.Hash, cp.ParentHash, cp.Tokens, Block{})
	if err != nil {
		return nil, nil, err
	}
	var embedded []*blockNode
	for i := range cp.FinalizedBlocks {
		fe := cp.FinalizedBlocks[i]
		b, err := fe.Block.toBlock()
		if err != nil {
			return nil, nil, err
		}
		n, err := buildNode(fe.Block.Number, fe.Block.Hash, fe.Block.ParentHash, fe.Tokens, b)
		if err != nil {
			return nil, nil, err
		}
		embedded = append(embedded, n)
	}
	return root, embedded, nil
}

type badAmountError string

func (e badAmountError) Error() string { return "invalid persisted amount: " + string(e) }

func errBadAmount(s string) error { return badAmountError(s) }
