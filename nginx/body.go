package nginx

// Request body storage for buffered forwarding (owner: s-forward; D7, D19,
// D21). Each buffered body keeps its first bytes in memory under the
// process's budget and the rest in a temp file under the spill caps, so a
// retry replays it byte for byte. Every exit releases the body's bytes,
// descriptor and file, and a local capacity refusal is a 503 counted apart
// from upstream 503s.

import (
	"errors"
)

// The process's body storage, one per front and shared by all its
// services. Safe for concurrent use.
type bodyStore struct {
}

// Builds an empty store under settings' budget and caps.
func newBodyStore(settings *BodySettings) *bodyStore {
	panic("lbalt: not implemented")
}

// Removes every spill file still held and releases the budget. A body still
// in use fails its next read.
func (self *bodyStore) close() error {
	return errors.New("lbalt: body store not implemented")
}
