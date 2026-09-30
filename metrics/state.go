package metrics

import (
	"encoding/json"
	"math/big"
	"sort"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// State is the complete incrementally maintained metric state across
// all tokens at the current canonical head.
type State struct {
	cfg    domain.Config
	tokens map[string]*tokenState
	window *rollingWindow
	head   domain.HeadPointer
}

// NewState creates empty state for the given configuration.
func NewState(cfg domain.Config) *State {
	return &State{
		cfg:    cfg,
		tokens: make(map[string]*tokenState),
		window: newRollingWindow(cfg),
	}
}

// Head returns the height/hash the state currently reflects.
func (s *State) Head() domain.HeadPointer { return s.head }

func (s *State) tokenState(token string) *tokenState {
	st := s.tokens[token]
	if st == nil {
		st = newTokenState(token, s.cfg)
		s.tokens[token] = st
	}
	return st
}

// Apply applies one block's events in input order and advances the
// rolling windows. Token snapshots must be evaluated after the whole
// block is applied, so the window push happens once per token here.
func (s *State) Apply(block *domain.Block) {
	aggs := make(map[string]blockTransferAgg, 0)
	for i := range block.Events {
		ev := block.Events[i]
		st := s.tokenState(ev.Token)
		agg := st.apply(ev)
		if ev.Type == domain.EventTransfer {
			cur := aggs[ev.Token]
			if cur.volume == nil {
				cur = newBlockTransferAgg()
			}
			cur.volume.Add(cur.volume, agg.volume)
			cur.count += agg.count
			for addr := range agg.addrs {
				cur.addrs[addr] = struct{}{}
			}
			aggs[ev.Token] = cur
		}
	}
	for token, agg := range aggs {
		s.window.push(block.Height, token, agg)
	}
	s.head = domain.HeadPointer{Height: block.Height, Hash: block.Hash}
}

// Rollback reverses one already applied block and retracts its window
// entries. Tokens that were touched only by this block disappear from
// the token map again.
func (s *State) Rollback(block *domain.Block) {
	// Reverse events so intermediate balance tiers and rankings pass
	// through the same states symmetrically.
	touched := make(map[string]struct{})
	for i := len(block.Events) - 1; i >= 0; i-- {
		ev := block.Events[i]
		st := s.tokens[ev.Token]
		if st == nil {
			continue
		}
		st.rollback(ev, newBlockTransferAgg())
		touched[ev.Token] = struct{}{}
	}
	for token := range touched {
		s.window.pop(block.Height, token)
	}
	s.head = domain.HeadPointer{Height: block.Height - 1}
}

// Snapshot emits the consistent per-block snapshot for every token.
// Token slices are built after all state for the block is settled, so
// a reader can never observe metrics from different blocks.
func (s *State) Snapshot(block *domain.Block) *domain.BlockSnapshot {
	snap := &domain.BlockSnapshot{
		Height:    block.Height,
		BlockHash: block.Hash,
		Timestamp: block.Timestamp,
		Tokens:    make(map[string]*domain.TokenSnapshot, len(s.tokens)),
	}
	for token, st := range s.tokens {
		snap.Tokens[token] = st.snapshot(block.Height, block.Hash, block.Timestamp, s.window.view(token))
	}
	return snap
}

// Checkpoint wraps serialized state with the pointer it reflects.
type Checkpoint struct {
	Height uint64
	Hash   string
	Data   []byte
}

// ---- JSON serialization for durable checkpoints ----

type jsonBig struct{ *big.Int }

func (j jsonBig) MarshalJSON() ([]byte, error) {
	if j.Int == nil {
		return []byte(`"0"`), nil
	}
	return []byte(`"` + j.String() + `"`), nil
}

func (j *jsonBig) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		j.Int = new(big.Int)
		return nil
	}
	if data[0] == '"' {
		data = data[1 : len(data)-1]
	}
	j.Int = new(big.Int)
	if len(data) == 0 {
		return nil
	}
	return j.Int.UnmarshalText(data)
}

type jsonHolder struct {
	Address string  `json:"a"`
	Balance jsonBig `json:"b"`
}

type jsonToken struct {
	CirculatingSupply jsonBig            `json:"cs"`
	TotalMinted       jsonBig            `json:"tm"`
	TotalBurned       jsonBig            `json:"tb"`
	TransferCount     uint64             `json:"tc"`
	TransferVolume    jsonBig            `json:"tv"`
	Balances          map[string]jsonBig `json:"b"`
	TierCounts        []uint64           `json:"tiers"`
	SumSquares        jsonBig            `json:"ss"`
}

type jsonWindowEntry struct {
	Height uint64   `json:"h"`
	Volume jsonBig  `json:"v"`
	Count  uint64   `json:"c"`
	Addrs  []string `json:"a"`
}

type jsonState struct {
	Head   domain.HeadPointer           `json:"head"`
	Tokens map[string]jsonToken         `json:"tokens"`
	Window map[string][]jsonWindowEntry `json:"window"`
}

// Marshal serializes the full state. Window entries are included so a
// checkpointed resume restores rolling metrics without replay.
func (s *State) Marshal() ([]byte, error) {
	js := jsonState{
		Head:   s.head,
		Tokens: make(map[string]jsonToken, len(s.tokens)),
		Window: make(map[string][]jsonWindowEntry, len(s.window.w)),
	}
	for token, st := range s.tokens {
		jt := jsonToken{
			CirculatingSupply: jsonBig{st.circulatingSupply},
			TotalMinted:       jsonBig{st.totalMinted},
			TotalBurned:       jsonBig{st.totalBurned},
			TransferCount:     st.transferCount,
			TransferVolume:    jsonBig{st.totalTransferVolume},
			Balances:          make(map[string]jsonBig, len(st.balances)),
			TierCounts:        append([]uint64(nil), st.tierCounts...),
			SumSquares:        jsonBig{st.sumSquares},
		}
		for addr, bal := range st.balances {
			jt.Balances[addr] = jsonBig{new(big.Int).Set(bal)}
		}
		js.Tokens[token] = jt
	}
	for token, tw := range s.window.w {
		entries := make([]jsonWindowEntry, 0, len(tw.heights))
		for i, h := range tw.heights {
			agg := tw.entries[i]
			addrs := make([]string, 0, len(agg.addrs))
			for addr := range agg.addrs {
				addrs = append(addrs, addr)
			}
			sort.Strings(addrs)
			entries = append(entries, jsonWindowEntry{
				Height: h,
				Volume: jsonBig{agg.volume},
				Count:  agg.count,
				Addrs:  addrs,
			})
		}
		js.Window[token] = entries
	}
	return json.Marshal(js)
}

// UnmarshalState restores state produced by Marshal.
func UnmarshalState(cfg domain.Config, data []byte) (*State, error) {
	var js jsonState
	if err := json.Unmarshal(data, &js); err != nil {
		return nil, err
	}
	s := NewState(cfg)
	s.head = js.Head
	for token, jt := range js.Tokens {
		st := newTokenState(token, cfg)
		st.circulatingSupply = bigOrZero(jt.CirculatingSupply.Int)
		st.totalMinted = bigOrZero(jt.TotalMinted.Int)
		st.totalBurned = bigOrZero(jt.TotalBurned.Int)
		st.transferCount = jt.TransferCount
		st.totalTransferVolume = bigOrZero(jt.TransferVolume.Int)
		st.sumSquares = bigOrZero(jt.SumSquares.Int)
		if len(jt.TierCounts) == len(st.tierCounts) {
			copy(st.tierCounts, jt.TierCounts)
		}
		for addr, bal := range jt.Balances {
			v := bigOrZero(bal.Int)
			if v.Sign() == 0 {
				continue
			}
			st.balances[addr] = v
			st.holders.observe(addr, v)
		}
		s.tokens[token] = st
	}
	for token, entries := range js.Window {
		tw := &tokenWindow{
			heights: make([]uint64, 0, len(entries)),
			entries: make([]blockTransferAgg, 0, len(entries)),
		}
		for _, e := range entries {
			agg := newBlockTransferAgg()
			agg.volume = bigOrZero(e.Volume.Int)
			agg.count = e.Count
			for _, addr := range e.Addrs {
				agg.addrs[addr] = struct{}{}
			}
			tw.heights = append(tw.heights, e.Height)
			tw.entries = append(tw.entries, agg)
		}
		s.window.w[token] = tw
	}
	return s, nil
}

func bigOrZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}
