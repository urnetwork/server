// URL completion receipts are shared by probe producers and ingest adapters.
package qualityprobe

import (
	"context"
	"errors"
	"time"
)

// A legacy acknowledgement cannot certify durable completion ingestion.
var ErrUrlProbeCompletionUnsupported = errors.New("URL completion receipts are unsupported")

// One server-issued turn, including setup failure. Retries preserve identity
// and completion time; AllowPacing never grants quality or quota credit.
type UrlProbeCompletion struct {
	ClientId     string    `json:"client_id"`
	ClaimOrdinal int64     `json:"claim_ordinal"`
	CompletedAt  time.Time `json:"completed_at"`
	ProbeFailure string    `json:"probe_failure,omitempty"`
	AllowPacing  bool      `json:"allow_pacing"`
}

// Optional for legacy/manual probes. Claimed URL work requires an acknowledged
// completion-capable path all the way through buffering and operator ingest.
type UrlProbeCompletionReporter interface {
	ReportUrlProbeCompletion(context.Context, UrlProbeCompletion) error
}
