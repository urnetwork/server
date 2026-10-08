// Exact retry and wire-error observations complement the owner-entry barrier.
// They do not turn an unsampled lock wait into proof that no wait occurred.
package model

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

// The existing server hook runs once immediately before each actual callback
// rerun. Database retry decisions and task reschedules are separate counters.
type parallelCloseRetryObservation struct {
	callbacks atomic.Int64
}

func (self *parallelCloseRetryObservation) context(ctx context.Context) context.Context {
	return server.Testing_WithTxRerunHook(ctx, func() { self.callbacks.Add(1) })
}

// Only already-registered finite database/taskworker counter families are read.
// The fixture owns its process window; registry decisions can exceed actual
// reruns when cancellation or a budget prevents the next attempt from starting.
func parallelCloseCounterSnapshot(t testing.TB) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal("parallel-close counter snapshot failed", err)
	}
	wanted := map[string]bool{
		"urnetwork_db_rerun_decisions_total":       true,
		"urnetwork_db_aborted_transactions_total":  true,
		"urnetwork_taskworker_polls_total":         true,
		"urnetwork_taskworker_finalizations_total": true,
		"urnetwork_taskworker_executions_total":    true,
		"urnetwork_transfer_debit_flush_total":     true,
	}
	result := map[string]float64{}
	for _, family := range families {
		if !wanted[family.GetName()] {
			continue
		}
		for _, metric := range family.GetMetric() {
			if metric.Counter == nil {
				t.Fatal("parallel-close expected a registered counter", family.GetName())
			}
			labels := make([]string, 0, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				labels = append(labels, label.GetName()+"="+label.GetValue())
			}
			slices.Sort(labels)
			result[family.GetName()+"{"+strings.Join(labels, ",")+"}"] = metric.Counter.GetValue()
		}
	}
	return result
}

func parallelCloseCounterDelta(t testing.TB, before, after map[string]float64) map[string]float64 {
	t.Helper()
	result := map[string]float64{}
	for key, old := range before {
		if value, ok := after[key]; !ok || value < old {
			t.Fatal("parallel-close counter lifetime changed", key, old, value)
		}
	}
	for key, value := range after {
		result[key] = value - before[key]
	}
	return result
}

// ErrorResponse bodies contain zero-terminated typed fields. Retain only one
// finite SQLSTATE class; never retain messages, query text, identities or data.
func parallelCloseWireErrorClass(body []byte) string {
	code, found := "", false
	for offset := 0; offset < len(body); {
		field := body[offset]
		offset++
		if field == 0 {
			if offset != len(body) || !found {
				return "malformed"
			}
			switch code {
			case "40001":
				return "serialization_failure"
			case "40P01":
				return "deadlock_detected"
			case "55P03":
				return "lock_not_available"
			case "57014":
				return "query_canceled"
			case "25P02":
				return "in_failed_transaction"
			case "23505":
				return "unique_violation"
			case "23503":
				return "foreign_key_violation"
			default:
				return "other"
			}
		}
		end := offset
		for end < len(body) && body[end] != 0 {
			end++
		}
		if end == len(body) {
			return "malformed"
		}
		if field == 'C' {
			if found || end-offset != 5 {
				return "malformed"
			}
			code, found = string(body[offset:end]), true
		}
		offset = end + 1
	}
	return "malformed"
}

func TestParallelCloseWireErrorClasses(t *testing.T) {
	for _, item := range []struct {
		code string
		want string
	}{
		{code: "40001", want: "serialization_failure"},
		{code: "40P01", want: "deadlock_detected"},
		{code: "55P03", want: "lock_not_available"},
		{code: "57014", want: "query_canceled"},
		{code: "25P02", want: "in_failed_transaction"},
		{code: "23505", want: "unique_violation"},
		{code: "23503", want: "foreign_key_violation"},
		{code: "23514", want: "other"},
	} {
		body := []byte("SERROR\x00C" + item.code + "\x00Msynthetic ignored payload\x00\x00")
		if actual := parallelCloseWireErrorClass(body); actual != item.want {
			t.Fatal("wire error classification changed", item.code, actual, item.want)
		}
	}
	for index, body := range [][]byte{nil, {0}, []byte("C40001"), []byte("C40001\x00"), []byte("C40001\x00C40P01\x00\x00"), []byte("C40001\x00\x00x"), []byte("C4000\x00\x00")} {
		if actual := parallelCloseWireErrorClass(body); actual != "malformed" {
			t.Fatal("wire decoder accepted malformed ErrorResponse", index, actual)
		}
	}
}

// The returned error is an acceptance failure, not a retry or a settlement
// policy. Caller-owned money/replay assertions run before this final verdict.
func parallelCloseForbiddenCounters(delta map[string]float64, actualReruns int64) error {
	if actualReruns != 0 {
		return fmt.Errorf("actual transaction callback reruns=%d", actualReruns)
	}
	for key, value := range delta {
		if value == 0 {
			continue
		}
		if strings.HasPrefix(key, "urnetwork_db_rerun_decisions_total{") ||
			strings.HasPrefix(key, "urnetwork_db_aborted_transactions_total{") ||
			(strings.HasPrefix(key, "urnetwork_taskworker_polls_total{") && strings.Contains(key, "outcome=error")) ||
			(strings.HasPrefix(key, "urnetwork_taskworker_finalizations_total{") && (strings.Contains(key, "outcome=rescheduled") || strings.Contains(key, "outcome=post_rescheduled"))) ||
			(strings.HasPrefix(key, "urnetwork_taskworker_executions_total{") && !strings.Contains(key, "outcome=succeeded")) ||
			(strings.HasPrefix(key, "urnetwork_transfer_debit_flush_total{") && strings.Contains(key, "result=busy")) {
			return fmt.Errorf("nonzero retry/contention counter %s=%g", key, value)
		}
	}
	return nil
}
