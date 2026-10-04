package chainmetrics

import (
	"errors"
	"fmt"
)

// ErrReorgTooDeep is returned when an incoming block forms a canonical
// reorganization whose fork point is below the engine's supported rollback
// boundary (older than HistoryBlocks blocks behind the head). The
// reorganization is rejected atomically: no published snapshot changes and no
// data belonging to the rejected branch is persisted.
var ErrReorgTooDeep = errors.New("reorganization rejected: fork point is beyond the supported rollback depth")

// ErrNoCommonAncestor is returned when an incoming block cannot be connected
// to any block the engine retains. Callers can distinguish it from
// ErrReorgTooDeep, although both leave the canonical chain untouched.
var ErrNoCommonAncestor = errors.New("reorganization rejected: block has no common ancestor with the canonical chain")

// reorgTooDeepf wraps ErrReorgTooDeep with the concrete boundary details.
func reorgTooDeepError(fork, limit, head uint64) error {
	return fmt.Errorf("%w: fork height %d, oldest supported %d, head %d",
		ErrReorgTooDeep, fork, limit, head)
}
