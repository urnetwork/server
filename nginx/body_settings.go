package nginx

// Request body buffering for forwarded routes (owner: s-forward): D7's
// process budget, D19's split between memory and a temp file, and D21's spill
// caps, with nginx's body limit and timeout (warp/warpctl/config.go:1338-1363).

import (
	"time"
)

// How the front holds request bodies for one retry. Each body that isn't
// streamed keeps its first MemoryPrefixBytes in memory under the process's
// MemoryBudgetBytes and the rest in a temp file under the spill caps, and a
// retry replays both. Read-only once the front is built.
type BodySettings struct {
	// the largest request body in bytes: nginx's `client_max_body_size 16m`.
	// A declared length over it is a 413 before the body is read, and a body
	// that crosses it is a 413 at that point (D21). Non-positive means no
	// limit, as nginx's 0 does.
	MaxBodyBytes int64
	// the bytes of each buffered body held in memory: nginx's
	// `client_body_buffer_size 64k` (D19). A route's client_body_buffer_size
	// replaces it there. Non-positive holds nothing in memory, so every
	// buffered byte spills.
	MemoryPrefixBytes int64
	// the memory all buffered body prefixes of the process share: 256 MiB,
	// sized from M2 and confirmed by the owner (D7), room for 4,096
	// concurrent 64 KiB prefixes. A body that can't reserve its prefix, one
	// of unknown length included, is a local 503 before its first read,
	// counted apart from upstream 503s and never retried (D21). Non-positive
	// means no budget.
	MemoryBudgetBytes int64
	// the spilled bytes the process may hold at once: 4 GiB (D21). A body
	// that would spill past it is a local 503. Non-positive means no cap.
	SpillMaxBytes int64
	// the spill files the process may hold at once: 1,024 (D21). A body that
	// needs one more is a local 503. Non-positive means no cap.
	SpillMaxFiles int
	// the directory spill files go in. Empty means os.TempDir(). Each file
	// is unlinked as soon as it is created and freed when its request ends.
	SpillDir string
	// the longest wait between two reads of a request body: nginx's
	// `client_body_timeout 15s`. A route's client_body_timeout_seconds
	// replaces it there. A stall past it is a 408. Non-positive means no
	// bound beyond the request's own context.
	ClientBodyTimeout time.Duration
	// the scratch buffer one body read uses, in bytes, pooled per front; it
	// bounds a single read, not what is held. Values below 1 mean 1.
	ReadBufferBytes int
}

// A new instance on every call, with nginx's limits and the decided budget
// and caps.
func DefaultBodySettings() *BodySettings {
	return &BodySettings{
		MaxBodyBytes:      16 * 1024 * 1024,
		MemoryPrefixBytes: 64 * 1024,
		MemoryBudgetBytes: 256 * 1024 * 1024,
		SpillMaxBytes:     4 * 1024 * 1024 * 1024,
		SpillMaxFiles:     1024,
		SpillDir:          "",
		ClientBodyTimeout: 15 * time.Second,
		ReadBufferBytes:   32 * 1024,
	}
}
