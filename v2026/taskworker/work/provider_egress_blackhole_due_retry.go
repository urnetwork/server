// Read-only cheap due lookups may retry transient request timeouts or explicit
// HTTP 503 within one task's admission boundary. No claim or probe is retried.
package work

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

const providerEgressBlackholeDueReadAttempts = 3

type providerEgressBlackholeDueReadResult uint8

const (
	blackholeDueReadOk providerEgressBlackholeDueReadResult = iota
	blackholeDueReadTimeout
	blackholeDueReadCanceled
	blackholeDueReadUnauthorized
	blackholeDueReadUnsupported
	blackholeDueReadRejected
	blackholeDueReadUnavailable
	blackholeDueReadDecode
	blackholeDueReadOther
)

// The finite vocabulary never carries an endpoint, identity or error message.
func (self providerEgressBlackholeDueReadResult) label() string {
	switch self {
	case blackholeDueReadOk:
		return "ok"
	case blackholeDueReadTimeout:
		return "timeout"
	case blackholeDueReadCanceled:
		return "canceled"
	case blackholeDueReadUnauthorized:
		return "unauthorized"
	case blackholeDueReadUnsupported:
		return "unsupported"
	case blackholeDueReadRejected:
		return "rejected"
	case blackholeDueReadUnavailable:
		return "unavailable"
	case blackholeDueReadDecode:
		return "decode"
	default:
		return "error_or_unknown"
	}
}

func newProviderEgressBlackholeDueReadMetrics() *prometheus.CounterVec {
	metric := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_egress_probe_blackhole_due_read_attempts_total",
		Help: "Returned cheap due-read attempts by fixed error class, including retries; not selected providers, task claims or acknowledged checks",
	}, []string{"result"})
	for result := blackholeDueReadOk; result <= blackholeDueReadOther; result++ {
		metric.WithLabelValues(result.label())
	}
	return metric
}

var egressProbeBlackholeDueReads = newProviderEgressBlackholeDueReadMetrics()

func init() {
	prometheus.MustRegister(egressProbeBlackholeDueReads)
}

// Cancellation and permanent endpoint failures outrank transient failures.
// Only a timeout or explicit due HTTP 503 with a live owner may be retried.
func providerEgressBlackholeDueReadClass(ctx context.Context, err error) providerEgressBlackholeDueReadResult {
	if err == nil {
		return blackholeDueReadOk
	}
	switch {
	case ctx.Err() != nil || errors.Is(err, context.Canceled):
		return blackholeDueReadCanceled
	case errors.Is(err, ingest.ErrUnauthorized):
		return blackholeDueReadUnauthorized
	case errors.Is(err, ingest.ErrBlackholeUnsupported):
		return blackholeDueReadUnsupported
	case errors.Is(err, ingest.ErrBlackholeDueUnavailable):
		return blackholeDueReadUnavailable
	case errors.Is(err, ingest.ErrRejected):
		return blackholeDueReadRejected
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return blackholeDueReadDecode
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return blackholeDueReadTimeout
	}
	return blackholeDueReadOther
}

// Each read has at most the existing 30-second operator timeout; three reads
// and two 250–500ms waits fit within 91 seconds, or an earlier task/admission
// deadline. A closed stop edge means normal admission ended, not an empty API
// response. Calls already in progress remain synchronously owned and joined.
func (self *providerEgressProbePass) blackholeDueWithRetry(ctx context.Context, limit int, stop <-chan struct{}) ([]ingest.DueProvider, error) {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-stop:
			return nil, nil
		default:
		}
		callCtx, cancel := context.WithTimeout(ctx, providerEgressControlPlaneTimeout)
		due, err := self.blackholeDue(callCtx, limit)
		cancel()
		result := providerEgressBlackholeDueReadClass(ctx, err)
		egressProbeBlackholeDueReads.WithLabelValues(result.label()).Inc()
		if (result != blackholeDueReadTimeout && result != blackholeDueReadUnavailable) || attempt == providerEgressBlackholeDueReadAttempts {
			return due, err
		}
		// Independent bounded jitter avoids synchronized shard re-reads. The
		// context owns every wait; no goroutine, process budget or new claim.
		delay := 250*time.Millisecond + time.Duration(rand.Int64N(int64(250*time.Millisecond)))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-stop:
			return nil, nil
		case <-time.After(delay):
		}
	}
}
