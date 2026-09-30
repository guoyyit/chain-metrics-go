package store

import (
	"encoding/json"
	"math/big"

	"github.com/guoyyit/chain-metrics-go/domain"
)

// JSON DTOs keep domain types free of wire-format concerns while
// persisting *big.Int amounts as exact decimal strings.

type dtoEvent struct {
	Type     int    `json:"t"`
	Token    string `json:"tok"`
	From     string `json:"from"`
	To       string `json:"to"`
	Amount   string `json:"amt"`
	TxHash   string `json:"tx"`
	LogIndex int    `json:"li"`
}

type dtoBlock struct {
	Height     uint64     `json:"h"`
	Hash       string     `json:"hash"`
	ParentHash string     `json:"ph"`
	Timestamp  int64      `json:"ts"`
	Events     []dtoEvent `json:"ev"`
}

type dtoHolder struct {
	Address string `json:"a"`
	Balance string `json:"b"`
}

type dtoTokenSnapshot struct {
	Token                string      `json:"tok"`
	Height               uint64      `json:"h"`
	BlockHash            string      `json:"bh"`
	Timestamp            int64       `json:"ts"`
	CirculatingSupply    string      `json:"cs"`
	TotalMinted          string      `json:"tm"`
	TotalBurned          string      `json:"tb"`
	TransferCount        uint64      `json:"tc"`
	TotalTransferVolume  string      `json:"ttv"`
	WindowTransferVolume string      `json:"wtv"`
	WindowTransferCount  uint64      `json:"wtc"`
	ActiveAddresses      uint64      `json:"aa"`
	BalanceTiers         []uint64    `json:"tiers"`
	TopHolders           []dtoHolder `json:"top"`
	TopHolderShare       float64     `json:"tsh"`
	HerfindahlIndex      float64     `json:"hhi"`
}

type dtoBlockSnapshot struct {
	Height    uint64                      `json:"h"`
	BlockHash string                      `json:"bh"`
	Timestamp int64                       `json:"ts"`
	Tokens    map[string]dtoTokenSnapshot `json:"tokens"`
}

func bigToJSON(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

func bigFromJSON(s string) *big.Int {
	v := new(big.Int)
	if s == "" {
		return v
	}
	if _, ok := v.SetString(s, 10); !ok {
		return new(big.Int)
	}
	return v
}

func encodeBlock(b *domain.Block) ([]byte, error) {
	d := dtoBlock{
		Height:     b.Height,
		Hash:       b.Hash,
		ParentHash: b.ParentHash,
		Timestamp:  b.Timestamp,
		Events:     make([]dtoEvent, 0, len(b.Events)),
	}
	for _, ev := range b.Events {
		d.Events = append(d.Events, dtoEvent{
			Type:     int(ev.Type),
			Token:    ev.Token,
			From:     ev.From,
			To:       ev.To,
			Amount:   bigToJSON(ev.Amount),
			TxHash:   ev.TxHash,
			LogIndex: ev.LogIndex,
		})
	}
	return json.Marshal(d)
}

func decodeBlock(data []byte) (*domain.Block, error) {
	var d dtoBlock
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	b := &domain.Block{
		Height:     d.Height,
		Hash:       d.Hash,
		ParentHash: d.ParentHash,
		Timestamp:  d.Timestamp,
		Events:     make([]domain.Event, 0, len(d.Events)),
	}
	for _, de := range d.Events {
		b.Events = append(b.Events, domain.Event{
			Type:     domain.EventType(de.Type),
			Token:    de.Token,
			From:     de.From,
			To:       de.To,
			Amount:   bigFromJSON(de.Amount),
			TxHash:   de.TxHash,
			LogIndex: de.LogIndex,
		})
	}
	return b, nil
}

func encodeSnapshot(snap *domain.BlockSnapshot) ([]byte, error) {
	d := dtoBlockSnapshot{
		Height:    snap.Height,
		BlockHash: snap.BlockHash,
		Timestamp: snap.Timestamp,
		Tokens:    make(map[string]dtoTokenSnapshot, len(snap.Tokens)),
	}
	for token, t := range snap.Tokens {
		dt := dtoTokenSnapshot{
			Token:                t.Token,
			Height:               t.Height,
			BlockHash:            t.BlockHash,
			Timestamp:            t.Timestamp,
			CirculatingSupply:    bigToJSON(t.CirculatingSupply),
			TotalMinted:          bigToJSON(t.TotalMinted),
			TotalBurned:          bigToJSON(t.TotalBurned),
			TransferCount:        t.TransferCount,
			TotalTransferVolume:  bigToJSON(t.TotalTransferVolume),
			WindowTransferVolume: bigToJSON(t.WindowTransferVolume),
			WindowTransferCount:  t.WindowTransferCount,
			ActiveAddresses:      t.ActiveAddresses,
			BalanceTiers:         append([]uint64(nil), t.BalanceTiers...),
			TopHolderShare:       t.TopHolderShare,
			HerfindahlIndex:      t.HerfindahlIndex,
			TopHolders:           make([]dtoHolder, 0, len(t.TopHolders)),
		}
		for _, h := range t.TopHolders {
			dt.TopHolders = append(dt.TopHolders, dtoHolder{
				Address: h.Address,
				Balance: bigToJSON(h.Balance),
			})
		}
		d.Tokens[token] = dt
	}
	return json.Marshal(d)
}

func decodeSnapshot(data []byte) (*domain.BlockSnapshot, error) {
	var d dtoBlockSnapshot
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	snap := &domain.BlockSnapshot{
		Height:    d.Height,
		BlockHash: d.BlockHash,
		Timestamp: d.Timestamp,
		Tokens:    make(map[string]*domain.TokenSnapshot, len(d.Tokens)),
	}
	for token, dt := range d.Tokens {
		t := &domain.TokenSnapshot{
			Token:                dt.Token,
			Height:               dt.Height,
			BlockHash:            dt.BlockHash,
			Timestamp:            dt.Timestamp,
			CirculatingSupply:    bigFromJSON(dt.CirculatingSupply),
			TotalMinted:          bigFromJSON(dt.TotalMinted),
			TotalBurned:          bigFromJSON(dt.TotalBurned),
			TransferCount:        dt.TransferCount,
			TotalTransferVolume:  bigFromJSON(dt.TotalTransferVolume),
			WindowTransferVolume: bigFromJSON(dt.WindowTransferVolume),
			WindowTransferCount:  dt.WindowTransferCount,
			ActiveAddresses:      dt.ActiveAddresses,
			BalanceTiers:         append([]uint64(nil), dt.BalanceTiers...),
			TopHolderShare:       dt.TopHolderShare,
			HerfindahlIndex:      dt.HerfindahlIndex,
			TopHolders:           make([]domain.HolderBalance, 0, len(dt.TopHolders)),
		}
		for _, dh := range dt.TopHolders {
			t.TopHolders = append(t.TopHolders, domain.HolderBalance{
				Address: dh.Address,
				Balance: bigFromJSON(dh.Balance),
			})
		}
		snap.Tokens[token] = t
	}
	return snap, nil
}
