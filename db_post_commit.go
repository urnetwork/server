package server

// Transaction-owned optional projections run only after a confirmed commit and
// after releasing the database connection. Retries and rollbacks discard them.

import "time"

// Each confirmed transaction owns one bounded post lifetime, including queueing.
const TxPostCommitTimeout = 5 * time.Second

// The transaction owner alone registers posts; registration is not concurrent.
type postCommitPgTx struct {
	PgTx
	posts              []PostFunction
	postKeyIndexes     map[string]int
	committedAt        time.Time
	commitObservations *txCommitObservations
}

// Registers one optional projection per key on a server-owned transaction. A
// later registration replaces that key's earlier action. Raw pgx transactions
// and savepoints return false; their external owner must arrange its own post.
// Posts may be lost on process failure or cancellation and must be repairable.
func AddTxPostCommit(tx PgTx, key string, post PostFunction) bool {
	owner, ok := tx.(*postCommitPgTx)
	if !ok {
		return false
	}
	if owner.postKeyIndexes == nil {
		owner.postKeyIndexes = map[string]int{}
	}
	if index, exists := owner.postKeyIndexes[key]; exists {
		owner.posts[index] = post
	} else {
		owner.postKeyIndexes[key] = len(owner.posts)
		owner.posts = append(owner.posts, post)
	}
	return true
}

// Supplies the confirmed commit-reply time, never registration or eventual
// callback start time. Coalescing, rollback and connection release are unchanged.
func AddTxPostCommitAt(tx PgTx, key string, post func(time.Time) any) bool {
	owner, ok := tx.(*postCommitPgTx)
	if !ok {
		return false
	}
	return AddTxPostCommit(tx, key, func() any { return post(owner.committedAt) })
}
