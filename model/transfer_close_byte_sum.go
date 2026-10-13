package model

// Destination-close byte sums over closed contracts, read in bounded keyset
// chunks.
//
// The daily transfer audit rollup and the block-9 clock backfill both sum the
// destination party's contract_close bytes over the transfer_contract rows
// whose close_time falls in a window. One UTC day on Main is about seven
// million contracts. Read by one statement, that sum kept one snapshot open
// for 5 to 58 minutes, and the rollup ran it inside its write transaction.
// The open snapshot pinned the cluster xmin horizon, so vacuum could not
// remove the dead tuples of the high-churn contract, escrow and task tables.
//
// Each chunk here is one autocommit statement over at most
// transferCloseByteChunkSize contracts in (close_time, contract_id) order. It
// reads the close-time partial indexes onward from the last key, so both its
// work and its snapshot are bounded by the chunk rather than the window. The
// state between chunks is a plain value that a task checkpoints in its args.
// Each caller marks its chunks with its own comment for pg_stat_activity.
//
// Consistency: the sum equals the single-statement sum for every contract
// whose close key and destination close do not change during the walk. The
// difference is when a contract is read: the single statement read all of
// them as of one snapshot, the walk reads each as of the chunk that reaches
// it. Writers stamp close_time with the current time, so the contracts of a
// window that ended before the walk began change only by a retention delete,
// by a re-stamp of an unresolved contract, or by a transaction that took its
// time before the window end and commits during the walk. A change that
// commits behind the cursor is missed by this walk, and an unresolved contract
// re-stamped from behind the cursor to ahead of it inside the window is
// counted twice. Terminal outcomes are claimed with a fresh close_time and are
// not re-stamped, so a terminal-only walk never counts a contract twice.

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/urnetwork/server"
)

// One chunk reads at most this many contracts and their destination closes: a
// few thousand index and heap pages, about a second at the slowest daily rate
// observed on Main (3,469 s for about seven million contracts).
const transferCloseByteChunkSize = 2000

// The resumable state of a destination-close byte sum over the contracts with
// Start <= close_time < End. The json form is a task checkpoint. Advanced by
// AdvanceTransferAuditRollupDay and ClockBackfill. Not safe for concurrent use.
type TransferCloseByteSum struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// only contracts with a final outcome
	TerminalOnly bool `json:"terminal_only,omitempty"`
	// the last key read, valid once ContractCount is positive
	AfterCloseTime  time.Time `json:"after_close_time,omitzero"`
	AfterContractId server.Id `json:"after_contract_id,omitzero"`
	ContractCount   int64     `json:"contract_count"`
	ByteCount       ByteCount `json:"byte_count"`
	Complete        bool      `json:"complete,omitempty"`
}

// Reads up to chunkCount more chunks, each in its own autocommit statement
// marked with the caller's constant marker, and reports whether the sum is
// complete.
func (self *TransferCloseByteSum) advance(ctx context.Context, marker string, chunkCount int) bool {
	for i := 0; i < chunkCount && !self.Complete; i += 1 {
		resume := 0 < self.ContractCount
		afterCloseTime := self.Start
		// the minimum id, so the first chunk includes every contract at Start
		afterContractId := server.Id{}
		if resume {
			afterCloseTime = self.AfterCloseTime
			afterContractId = self.AfterContractId
		}

		found := false
		var lastCloseTime time.Time
		var lastContractId server.Id
		var contractCount int64
		var byteCount ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				transferCloseByteChunkSql(marker, self.TerminalOnly, resume),
				afterCloseTime,
				afterContractId,
				self.End,
				ContractPartyDestination,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&lastCloseTime, &lastContractId, &contractCount, &byteCount))
					found = true
				}
			})
		})

		if !found {
			// no contract remains after the cursor
			self.Complete = true
			break
		}
		if byteCount < 0 || ByteCount(math.MaxInt64)-self.ByteCount < byteCount {
			server.Raise(fmt.Errorf("invalid transfer close byte sum"))
		}
		self.AfterCloseTime = lastCloseTime
		self.AfterContractId = lastContractId
		self.ContractCount += contractCount
		self.ByteCount += byteCount
		// a short chunk read every remaining key at its snapshot
		self.Complete = contractCount < transferCloseByteChunkSize
	}
	return self.Complete
}

// One keyset chunk: the closed contracts after the cursor ($1, $2) and before
// $3, with their summed destination closes ($4 is the destination party). A
// resumed chunk starts strictly after the last key read; the first starts at
// the window start with the minimum id, so it includes every contract closed
// exactly at the start.
//
// Every branch reads one close-time partial index in key order and stops at
// the chunk size, which PostgreSQL keeps as a bounded index scan in both
// custom and generic plans. The chunk size is a literal so a generic plan
// knows the limit. An all-outcome sum merges the terminal index with the
// closed NULL-outcome index into one key order, so an outcome claim during
// the walk does not move a contract across the cursor.
//
// The marker is a package constant that names the caller. Anything but plain
// lowercase words is refused, so it can never close the comment.
func transferCloseByteChunkSql(marker string, terminalOnly bool, resume bool) string {
	for _, c := range marker {
		if (c < 'a' || 'z' < c) && c != '_' {
			panic(fmt.Errorf("invalid statement marker"))
		}
	}
	afterOp := ">="
	if resume {
		afterOp = ">"
	}
	branch := func(outcomePredicate string) string {
		return fmt.Sprintf(`
                SELECT close_time, contract_id
                FROM transfer_contract
                WHERE
                    %s AND
                    (close_time, contract_id) %s ($1, $2) AND
                    close_time < $3
                ORDER BY close_time, contract_id
                LIMIT %d
            `, outcomePredicate, afterOp, transferCloseByteChunkSize)
	}

	keys := branch("outcome IS NOT NULL")
	if !terminalOnly {
		keys = fmt.Sprintf(`
            SELECT close_time, contract_id
            FROM (
                (%s)
                UNION ALL
                (%s)
            ) closed
            ORDER BY close_time, contract_id
            LIMIT %d
        `, keys, branch("outcome IS NULL AND close_time IS NOT NULL"), transferCloseByteChunkSize)
	}

	return fmt.Sprintf(`
        /* %s */
        WITH chunk AS MATERIALIZED (%s)
        SELECT
            last_key.close_time,
            last_key.contract_id,
            (SELECT COUNT(*) FROM chunk),
            (
                SELECT COALESCE(SUM(contract_close.used_transfer_byte_count), 0)::bigint
                FROM chunk
                INNER JOIN contract_close ON
                    contract_close.contract_id = chunk.contract_id AND
                    contract_close.party = $4
            )
        FROM (
            SELECT close_time, contract_id
            FROM chunk
            ORDER BY close_time DESC, contract_id DESC
            LIMIT 1
        ) last_key
    `, marker, keys)
}
