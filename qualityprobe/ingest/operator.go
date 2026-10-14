package ingest

import (
	"context"
	"strings"
	"time"

	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The operator's half of a probe pass, split by what a caller needs. *Client
// implements every part over the operator HTTP API. A process that is itself
// the platform -- the server's taskworker, when its operator_transport is
// local -- can implement them in-process against the same controller and
// model code the API handlers call (server/localoperator), and then makes no
// API request at all.
//
// Every implementation keeps the same contract: the client-side refusals and
// truncations of the New*/Prepare* builders in this package run first, and a
// refusal returns the sentinel the HTTP client returns for that answer
// (ErrRejected via RejectedError, ErrDuePriorityMaintenancePending,
// ErrBlackholeDueUnavailable, ErrPinsUnavailable, egresshealth.ErrPoolUnavailable,
// bandwidth.ErrNoBudget, qualityprobe.ErrUrlProbeCompletionUnsupported), so a
// caller's classification cannot tell the transports apart.

// Claims the next due URL providers of the operator's shard.
type DueLister interface {
	Due(ctx context.Context, limit int) ([]DueProvider, error)
}

// Records attempts and completes claimed URL turns.
type AttemptRecorder interface {
	ReportAttempt(ctx context.Context, providerClientId string, probeFailure string) error
	qualityprobe.UrlProbeCompletionReporter
}

// Records one egress-health run.
type HealthSubmitter interface {
	SubmitEgressHealth(ctx context.Context, providerClientId string, res *egresshealth.Result) error
}

// Records an independently observed exit address.
type LocationSubmitter interface {
	Submit(ctx context.Context, providerClientId string, exitIp string, observedAt time.Time) error
}

// Serves the blackhole sweep and records its batches.
type BlackholeChecker interface {
	BlackholeDue(ctx context.Context, limit int) ([]DueProvider, error)
	SubmitBlackholeChecks(ctx context.Context, checks []BlackholeCheck) error
}

// Rations and records active bandwidth samples.
type BandwidthRecorder interface {
	ReserveBandwidth(ctx context.Context, providerClientId string, byteCount int64) error
	SubmitBandwidth(ctx context.Context, providerClientId string, source string, bytesPerSecond float64, sampleByteCount int64) error
}

// The per-pass catalogs: the certificate pins the server observed and its
// destination pool. A returned pool has already passed egresshealth.ValidatePool.
type CatalogSource interface {
	GeolocationPins(ctx context.Context) (map[string]GeolocationPin, error)
	DestinationPool(ctx context.Context) (*egresshealth.Pool, error)
}

// Everything a probe pass asks of, or reports to, the operator's server.
type Operator interface {
	DueLister
	AttemptRecorder
	HealthSubmitter
	LocationSubmitter
	BlackholeChecker
	BandwidthRecorder
	CatalogSource
}

var _ Operator = (*Client)(nil)

// Fetches the server's destination pool from <ServerUrl>/network/provider-egress-destinations
// under the operator secret (see egresshealth.FetchPool). A failed fetch is
// egresshealth.ErrPoolUnavailable; no other catalog is substituted.
func (self *Client) DestinationPool(ctx context.Context) (*egresshealth.Pool, error) {
	return egresshealth.FetchPool(ctx, self.httpClient(), strings.TrimRight(self.ServerUrl, "/")+egresshealth.PoolPath, self.OperatorSecret)
}
