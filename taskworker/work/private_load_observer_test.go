// Test-owned activity sampling has one connection and one joined lifetime.
package work

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// Results are immutable after done closes; Close may run repeatedly or concurrently.
type privateLoadObserver struct {
	cancel         context.CancelFunc
	stop           chan struct{}
	done           chan struct{}
	closeOnce      sync.Once
	values         map[string]float64
	sampleRequests <-chan chan struct{}
}

// Sampling belongs to this owner, never to the parent pool's teardown.
func newPrivateLoadObserver(ctx context.Context, conn server.PgCanQuery, release func() error, sampleRequests ...<-chan chan struct{}) *privateLoadObserver {
	ctx, cancel := context.WithCancel(ctx)
	observer := &privateLoadObserver{
		cancel: cancel, stop: make(chan struct{}), done: make(chan struct{}),
		values: map[string]float64{},
	}
	if len(sampleRequests) > 0 {
		observer.sampleRequests = sampleRequests[0]
	}
	go func() {
		defer close(observer.done)
		defer func() {
			if err := release(); err != nil {
				observer.values["cleanup_error"]++
			}
		}()
		observer.run(ctx, conn)
	}()
	return observer
}

// Cancellation interrupts an in-flight query; completion includes connection release.
func (self *privateLoadObserver) Close() map[string]float64 {
	self.closeOnce.Do(func() {
		self.cancel()
		close(self.stop)
	})
	<-self.done
	return maps.Clone(self.values)
}

// Sample only finite query shapes; identities and raw SQL never leave this owner.
func (self *privateLoadObserver) run(ctx context.Context, conn server.PgCanQuery) {
	for {
		var sampled chan struct{}
		select {
		case sampled = <-self.sampleRequests:
		default:
		}
		rows, err := conn.Query(ctx, `SELECT query,state,COALESCE(wait_event_type,''),
				EXTRACT(epoch FROM clock_timestamp()-query_start)::double precision
				FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state<>'idle'`)
		if err != nil {
			if !privateLoadOnlyCancellation(err, ctx.Err()) {
				self.values["sampling_error"]++
			}
			return
		}
		counts := map[string]float64{}
		for rows.Next() {
			var query, state, wait string
			var age float64
			if err := rows.Scan(&query, &state, &wait, &age); err != nil {
				self.values["sampling_error"]++
				rows.Close()
				return
			}
			query = strings.ToLower(strings.Join(strings.Fields(query), " "))
			kind := "other"
			switch {
			case strings.Contains(query, "update network_client as top"):
				kind = "usage_stamp"
			case strings.Contains(query, "coalesce(sum(selected_escrow.balance_byte_count)"):
				kind = "census"
			case strings.Contains(query, "snapshot") && strings.Contains(query, "transfer_balance_net_escrow"):
				kind = "cache"
			case strings.Contains(query, "transfer_balance") && strings.Contains(query, "for update"):
				kind = "balance"
			case strings.Contains(query, "update transfer_escrow"):
				kind = "metadata"
			case strings.Contains(query, "transfer_contract") && strings.Contains(query, "for update"):
				kind = "contract"
			}
			if wait == "Lock" {
				kind += "_lock"
			} else if state == "idle in transaction" {
				kind += "_idle_tx"
			} else if wait != "" {
				kind += "_other_wait"
			} else {
				kind += "_active"
			}
			counts[kind]++
			self.values[kind+"_max_query_age_seconds"] = max(self.values[kind+"_max_query_age_seconds"], age)
		}
		rows.Close()
		if rowErr := rows.Err(); rowErr != nil {
			if !privateLoadOnlyCancellation(rowErr, ctx.Err()) {
				self.values["sampling_error"]++
			}
			return
		}
		for kind, count := range counts {
			self.values[kind+"_peak"] = max(self.values[kind+"_peak"], count)
		}
		self.values["samples"]++
		if sampled != nil {
			close(sampled)
		}
		select {
		case <-self.stop:
			return
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Cancellation may be wrapped, but it cannot hide a sibling query or stream failure.
func privateLoadOnlyCancellation(err, cancellation error) bool {
	if err == nil || cancellation == nil {
		return false
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		children := many.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !privateLoadOnlyCancellation(child, cancellation) {
				return false
			}
		}
		return true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		if child := one.Unwrap(); child != nil {
			return privateLoadOnlyCancellation(child, cancellation)
		}
	}
	return err == cancellation
}
