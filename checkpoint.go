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
}

func encodeCheckpoint(node *blockNode) ([]byte, error) {
	cp := checkpoint{
		Number:     node.number,
		Hash:       node.hash,
		ParentHash: node.parentHash,
		Tokens:     make(map[string]cpToken, len(node.tokenStates)),
	}
	for tokenID, st := range node.tokenStates {
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
		cp.Tokens[tokenID] = t
	}
	return json.MarshalIndent(cp, "", "  ")
}

func decodeCheckpoint(raw []byte) (*blockNode, map[string]*tokenState, map[string]*rollingWindow, error) {
	var cp checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, nil, nil, err
	}
	states := make(map[string]*tokenState, len(cp.Tokens))
	windows := make(map[string]*rollingWindow, len(cp.Tokens))
	node := &blockNode{
		number:      cp.Number,
		hash:        cp.Hash,
		parentHash:  cp.ParentHash,
		tokenStates: states,
		windows:     windows,
	}
	for tokenID, t := range cp.Tokens {
		supply, ok := new(big.Int).SetString(t.Supply, 10)
		if !ok {
			return nil, nil, nil, errBadAmount(t.Supply)
		}
		var tiers []BalanceTier
		for _, ct := range t.Tiers {
			min, ok := new(big.Int).SetString(ct.Min, 10)
			if !ok {
				return nil, nil, nil, errBadAmount(ct.Min)
			}
			tier := BalanceTier{Name: ct.Name, Min: min}
			if ct.Max != "" {
				max, ok := new(big.Int).SetString(ct.Max, 10)
				if !ok {
					return nil, nil, nil, errBadAmount(ct.Max)
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
				return nil, nil, nil, errBadAmount(cb.Balance)
			}
			st.balances[cb.Address] = bal
		}
		states[tokenID] = st

		win := &rollingWindow{
			tokenID:    tokenID,
			activeSize: t.ActiveSize,
			volumeSize: t.VolumeSize,
		}
		for _, ce := range t.Entries {
			vol, ok := new(big.Int).SetString(ce.Volume, 10)
			if !ok {
				return nil, nil, nil, errBadAmount(ce.Volume)
			}
			entry := windowEntry{block: ce.Block, active: make(map[string]struct{}, len(ce.Active)), volume: vol}
			for _, a := range ce.Active {
				entry.active[a] = struct{}{}
			}
			win.entries = append(win.entries, entry)
		}
		windows[tokenID] = win
	}
	node.tokenStates = states
	node.windows = windows
	return node, states, windows, nil
}

type badAmountError string

func (e badAmountError) Error() string { return "invalid persisted amount: " + string(e) }

func errBadAmount(s string) error { return badAmountError(s) }
