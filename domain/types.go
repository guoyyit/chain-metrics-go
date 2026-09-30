package domain

import "math/big"
import "strconv"

// EventType identifies the kind of token balance change.
type EventType int

const (
	// EventMint issues new supply: From is the zero address.
	EventMint EventType = iota
	// EventTransfer moves value between two non-zero addresses.
	EventTransfer
	// EventBurn destroys supply: To is the zero address.
	EventBurn
)

// ZeroAddress denotes mint/burn side of an event.
const ZeroAddress = ""

// Event is a single token balance change observed on chain.
type Event struct {
	Type     EventType
	Token    string
	From     string
	To       string
	Amount   *big.Int
	TxHash   string
	LogIndex int
}

// Key returns the unique identifier of an event within its block,
// used for de-duplication when the same event is delivered more than
// once. It is the transaction hash plus the log index; synthetic
// events without a transaction hash use a sentinel key.
func (e Event) Key() string {
	return e.TxHash + "#" + strconv.Itoa(e.LogIndex)
}

// Block is a canonical-or-pending block carrying an ordered event list.
type Block struct {
	Height     uint64
	Hash       string
	ParentHash string
	Timestamp  int64
	Events     []Event
}

// HolderBalance is one entry of a top-holder ranking.
type HolderBalance struct {
	Address string
	Balance *big.Int
}

// TokenSnapshot is a per-token, per-block immutable metric snapshot.
type TokenSnapshot struct {
	Token     string
	Height    uint64
	BlockHash string
	Timestamp int64

	CirculatingSupply *big.Int
	TotalMinted       *big.Int
	TotalBurned       *big.Int

	TransferCount        uint64
	TotalTransferVolume  *big.Int
	WindowTransferVolume *big.Int
	WindowTransferCount  uint64
	ActiveAddresses      uint64

	BalanceTiers []uint64

	TopHolders      []HolderBalance
	TopHolderShare  float64
	HerfindahlIndex float64
}

// BlockSnapshot bundles every token's snapshot at one published block,
// guaranteeing readers never observe metrics mixed across blocks.
type BlockSnapshot struct {
	Height    uint64
	BlockHash string
	Timestamp int64
	Tokens    map[string]*TokenSnapshot
}

// HeadPointer is the durable pointer to the last published canonical block.
type HeadPointer struct {
	Height uint64
	Hash   string
}
