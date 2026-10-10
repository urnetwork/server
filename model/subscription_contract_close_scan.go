// The operator's synchronous scan owns every close through its terminal read.
// It publishes no tasks and releases the page connection before settlement.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// At fixes the scan boundary and eligibility clock. Missing historical
// deadlines use this explicit operator retirement time, never a fabricated age.
type ContractClosureScanOptions struct {
	At       time.Time
	PageSize int
	Limit    int
}

// Closed counts acknowledged commits followed by an independent terminal read.
type ContractClosureScanResult struct {
	Visited       int `json:"visited"`
	Closed        int `json:"closed"`
	AlreadyClosed int `json:"already_closed"`
	Missing       int `json:"missing"`
	Deferred      int `json:"deferred"`
	Failed        int `json:"failed"`
}

// Continue past individual failures, report each one, and return failure if
// any close failed. Cancellation ends the loop before another contract starts.
// The optional observer runs synchronously after verification; its error stops
// the scan without pretending subsequent records have been handled.
func CloseOpenContractsSynchronously(ctx context.Context, options ContractClosureScanOptions,
	observe func(*ContractDeadlineReconciliation, error) error,
) (result *ContractClosureScanResult, returnErr error) {
	if options.At.IsZero() || options.PageSize < 1 || options.PageSize > 10000 || options.Limit < 0 {
		return nil, fmt.Errorf("invalid synchronous contract scan options")
	}
	result = &ContractClosureScanResult{}
	server.HandleError(func() {
		var after server.Id
		for options.Limit == 0 || result.Visited < options.Limit {
			server.Raise(ctx.Err())
			type candidate struct {
				id         server.Id
				expiration *time.Time
			}
			candidates := []candidate{}
			count := options.PageSize
			if options.Limit > 0 {
				count = min(count, options.Limit-result.Visited)
			}
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT contract_id,expiration_time FROM transfer_contract
					WHERE outcome IS NULL AND contract_id>$1 AND create_time<=$2
					ORDER BY contract_id LIMIT $3`, after, options.At, count)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var value candidate
						server.Raise(rows.Scan(&value.id, &value.expiration))
						candidates = append(candidates, value)
					}
				})
			}, server.OptNoRetry())
			if len(candidates) == 0 {
				break
			}
			for _, candidate := range candidates {
				server.Raise(ctx.Err())
				after = candidate.id
				result.Visited++
				deadline := options.At
				if candidate.expiration != nil {
					deadline = *candidate.expiration
					if options.At.Before(deadline) {
						result.Deferred++
						continue
					}
				}
				closed, err := func() (*ContractDeadlineReconciliation, error) {
					bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()
					closed, err := ReconcileContractAtDeadline(bounded, candidate.id, deadline)
					if err != nil || closed.Missing {
						return closed, err
					}
					server.HandleError(func() {
						server.Db(bounded, func(conn server.PgConn) {
							var open bool
							server.Raise(conn.QueryRow(bounded, `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1 AND outcome IS NULL)`, candidate.id).Scan(&open))
							if open {
								server.Raise(fmt.Errorf("acknowledged contract remains open"))
							}
						}, server.OptNoRetry())
					}, func(verifyErr error) { err = verifyErr })
					return closed, err
				}()
				if err != nil {
					result.Failed++
				} else if closed.Missing {
					result.Missing++
				} else if closed.AlreadyClosed {
					result.AlreadyClosed++
				} else {
					result.Closed++
				}
				if closed == nil {
					closed = &ContractDeadlineReconciliation{ContractId: candidate.id}
				}
				if observe != nil {
					server.Raise(observe(closed, err))
				}
				server.Raise(ctx.Err())
			}
		}
		if result.Failed > 0 {
			returnErr = fmt.Errorf("%d contract closures failed", result.Failed)
		}
	}, func(err error) { returnErr = err })
	return
}
