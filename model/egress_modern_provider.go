// The modern provider gate of the Quality and Speed buckets (FP2FIX.md,
// 2026-10-14, option 4: probe-verified, shadow first). A provider is modern
// when the FP2 URL prober has measured, on a real client to provider session,
// both end-to-end post-quantum encryption (the TLS 1.3 hybrid X25519MLKEM768,
// which providers answer since v2026.7.21-999024260) and resizable transfer
// windows (acks that carry receive_window_byte_count, since
// v2026.9.14-1046484290). Versions are not read: most providers run a custom
// lineage whose version says nothing of either.
//
// Each probe run that observed its session reports what it saw with its URL
// result, and the ingest records it in provider_modern_evidence, one row per
// provider holding the latest observation (RecordProviderModernEvidence).
// Every pass that decides the buckets classifies each provider's row into one
// outcome (classifyProviderModernEvidence) and counts, over the providers the
// other rules admit to Quality and to Speed, what the gate decides for them:
// the shadow counts, which read the same whether the gate is on or off. The
// dashboard counts publish them as urnetwork_stats_provider_modern_gate_shadow
// and every score export logs them.
//
// The gate turns on by itself at its start, 2026-10-21 00:00 UTC by default
// (owner decision 2026-10-15; EgressIndexSettings.RequiresModernProvider),
// and from then takes every provider whose outcome is not modern out of
// Quality and Speed under the not_modern reason. Each pass decides at its
// own instant, so a pass decides every provider alike.
// `quality_speed_requires_modern_provider` in provider.yml overrides the
// start: false keeps the gate off, the kill switch and the way to postpone,
// and true turns it on at once; `quality_speed_modern_provider_start` moves
// the start. A provider the gate takes out stays in Online and the location
// counts, and the rule is bucket membership only, like the URL ratio: probes,
// explicit client ids and force_minimum keep only the common gates.
package model

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/server"
)

// What the gate decides for one provider, in the order
// classifyProviderModernEvidence tests them after the evidence's age: a row
// that fails both capabilities is not_post_quantum.
const (
	ProviderModernOutcomeModern             = "modern"
	ProviderModernOutcomeNotPostQuantum     = "not_post_quantum"
	ProviderModernOutcomeNoResizableWindows = "no_resizable_windows"
	ProviderModernOutcomeNoEvidence         = "no_evidence"
	ProviderModernOutcomeStaleEvidence      = "stale_evidence"
)

// Every outcome, in the order the gauge and the export log list them.
var ProviderModernOutcomes = []string{
	ProviderModernOutcomeModern,
	ProviderModernOutcomeNotPostQuantum,
	ProviderModernOutcomeNoResizableWindows,
	ProviderModernOutcomeNoEvidence,
	ProviderModernOutcomeStaleEvidence,
}

// The native buckets the gate holds to modern providers. Online is not one.
var ProviderModernGateBuckets = []string{
	RankModeQuality,
	RankModeSpeed,
}

// How long the latest observation decides a provider. The prober measures an
// eligible provider about ten times in four hours and every run that sees its
// session replaces the observation, so a provider in Quality or Speed, which
// already needs URL evidence within eight hours, is measured far more often
// than this. The window only has to outlast runs that observe nothing, a
// prober outage or a rollout of probers without the evidence, while a
// capability changes only when the provider's app changes; a downgrade shows
// at its next run whatever the window. Evidence exactly this old is stale, as
// a URL outcome exactly eight hours old is.
const ProviderModernEvidenceMaxAge = 7 * 24 * time.Hour

// The furthest ahead of the server an observation may claim to be, as for URL
// evidence; anything later is refused rather than allowed to outrank the
// observations that follow it.
const providerModernEvidenceMaxFutureSkew = time.Minute

// The longest key exchange name the evidence table holds, in bytes. The
// names crypto/tls gives a curve are a few ASCII characters.
const providerModernEvidenceKeyExchangeMaxLength = 64

// What one probe run observed of the provider's end of its session: the
// end-to-end encryption the two negotiated and the transfer window the
// provider's acks advertised. The zero value observed nothing and records
// nothing. The prober must report a negative only from a session that asked
// for end-to-end encryption and finished negotiating it: a probe with
// encryption off, or one whose handshake had not concluded, observed nothing
// about the provider's encryption and reports the zero value.
type ProviderModernEvidence struct {
	// an end-to-end encrypted session with the provider was established
	E2eNegotiated bool
	// the negotiated TLS key exchange group as crypto/tls names it, for
	// example X25519MLKEM768; "" for none
	KeyExchange string
	// the negotiated group is a post-quantum hybrid
	PostQuantum bool
	// the provider's acks carried receive_window_byte_count
	WindowAdvertised bool
	// the largest receive window the provider advertised
	MaxReceiveWindowByteCount ByteCount
	// when the session was observed, zero for no observation
	ObservedAt time.Time
}

// Whether the run observed the session at all.
func (self ProviderModernEvidence) Observed() bool {
	return !self.ObservedAt.IsZero()
}

// An error for an observation the table must not hold: one claiming to be
// from the future, a negative window, or a key exchange name the column
// cannot store.
func (self ProviderModernEvidence) validate(now time.Time) error {
	if self.ObservedAt.After(now.Add(providerModernEvidenceMaxFutureSkew)) {
		return fmt.Errorf("modern provider evidence observed at %s is in the future", self.ObservedAt.UTC().Format(time.RFC3339Nano))
	}
	if self.MaxReceiveWindowByteCount < 0 {
		return fmt.Errorf("modern provider evidence has a negative receive window %d", self.MaxReceiveWindowByteCount)
	}
	if providerModernEvidenceKeyExchangeMaxLength < len(self.KeyExchange) || !utf8.ValidString(self.KeyExchange) {
		return fmt.Errorf("modern provider evidence key exchange name is not %d bytes or fewer of utf-8", providerModernEvidenceKeyExchangeMaxLength)
	}
	return nil
}

// One provider's row of provider_modern_evidence: the latest observation,
// when it and the first observation were measured, and how many runs
// advanced it.
type ProviderModernEvidenceRecord struct {
	// end-to-end encryption was negotiated with a post-quantum hybrid group
	PostQuantum bool
	// the negotiated group, "" when no end-to-end session was negotiated
	KeyExchange string
	// the provider's acks advertised a receive window
	WindowResizable bool
	// the largest window advertised, 0 without one
	MaxReceiveWindowByteCount ByteCount
	MeasuredAt                time.Time
	FirstMeasuredAt           time.Time
	RunCount                  int
}

// One row per provider, keeping the latest observation, in one statement so
// concurrent runs need no lock. A newer observation replaces the stored one
// and counts a run. One at the same time is a retried call when it is the
// same observation, and changes nothing; when it conflicts, each capability
// keeps the less modern of the two, as equal-time URL security evidence keeps
// the failure, so the result does not depend on which arrived first. An older
// observation, delayed behind a newer one, only moves first_measured_at
// earlier. Every time is the observation's, never the database clock's: the
// columns are naive timestamps holding utc.
const recordProviderModernEvidenceSql = `
	INSERT INTO provider_modern_evidence AS evidence (
		client_id,
		post_quantum,
		key_exchange,
		window_resizable,
		max_receive_window_byte_count,
		measured_at,
		first_measured_at,
		run_count
	) VALUES ($1, $2, $3, $4, $5, $6, $6, 1)
	ON CONFLICT (client_id) DO UPDATE SET
		post_quantum = CASE
			WHEN evidence.measured_at < excluded.measured_at THEN excluded.post_quantum
			WHEN evidence.measured_at = excluded.measured_at THEN evidence.post_quantum AND excluded.post_quantum
			ELSE evidence.post_quantum
		END,
		key_exchange = CASE
			WHEN evidence.measured_at < excluded.measured_at THEN excluded.key_exchange
			WHEN evidence.measured_at = excluded.measured_at AND evidence.post_quantum AND NOT excluded.post_quantum THEN excluded.key_exchange
			ELSE evidence.key_exchange
		END,
		window_resizable = CASE
			WHEN evidence.measured_at < excluded.measured_at THEN excluded.window_resizable
			WHEN evidence.measured_at = excluded.measured_at THEN evidence.window_resizable AND excluded.window_resizable
			ELSE evidence.window_resizable
		END,
		max_receive_window_byte_count = CASE
			WHEN evidence.measured_at < excluded.measured_at THEN excluded.max_receive_window_byte_count
			WHEN evidence.measured_at = excluded.measured_at THEN LEAST(evidence.max_receive_window_byte_count, excluded.max_receive_window_byte_count)
			ELSE evidence.max_receive_window_byte_count
		END,
		measured_at = GREATEST(evidence.measured_at, excluded.measured_at),
		first_measured_at = LEAST(evidence.first_measured_at, excluded.first_measured_at),
		run_count = evidence.run_count + CASE WHEN evidence.measured_at < excluded.measured_at THEN 1 ELSE 0 END
`

// Records what one probe run observed of the provider's session. Evidence
// that observed nothing records nothing and is no error; evidence the table
// must not hold is refused with an error and records nothing. A database
// failure raises, as every model write does. Post-quantum needs an
// established end-to-end session, and a window needs the provider to have
// advertised one, so an inconsistent observation is stored as the less
// modern reading.
func RecordProviderModernEvidence(ctx context.Context, clientId server.Id, evidence ProviderModernEvidence) error {
	if !evidence.Observed() {
		return nil
	}
	if clientId == (server.Id{}) {
		return fmt.Errorf("modern provider evidence has no client id")
	}
	if err := evidence.validate(server.NowUtc()); err != nil {
		return err
	}
	postQuantum := evidence.E2eNegotiated && evidence.PostQuantum
	keyExchange := ""
	if evidence.E2eNegotiated {
		keyExchange = evidence.KeyExchange
	}
	maxReceiveWindowByteCount := ByteCount(0)
	if evidence.WindowAdvertised {
		maxReceiveWindowByteCount = evidence.MaxReceiveWindowByteCount
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			recordProviderModernEvidenceSql,
			clientId,
			postQuantum,
			keyExchange,
			evidence.WindowAdvertised,
			maxReceiveWindowByteCount,
			evidence.ObservedAt.UTC(),
		))
	})
	return nil
}

// The rows of clientIds, read in batches like the scoped common gates so a
// large export cohort keeps every query bounded; nil clientIds reads every
// row. A provider without a row is absent.
func getProviderModernEvidenceRecords(ctx context.Context, clientIds []server.Id) map[server.Id]*ProviderModernEvidenceRecord {
	records := map[server.Id]*ProviderModernEvidenceRecord{}
	const selectSql = `
		SELECT
			client_id,
			post_quantum,
			key_exchange,
			window_resizable,
			max_receive_window_byte_count,
			measured_at,
			first_measured_at,
			run_count
		FROM provider_modern_evidence
	`
	server.Db(ctx, func(conn server.PgConn) {
		read := func(query string, args ...any) {
			result, err := conn.Query(ctx, query, args...)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					record := &ProviderModernEvidenceRecord{}
					server.Raise(result.Scan(
						&clientId,
						&record.PostQuantum,
						&record.KeyExchange,
						&record.WindowResizable,
						&record.MaxReceiveWindowByteCount,
						&record.MeasuredAt,
						&record.FirstMeasuredAt,
						&record.RunCount,
					))
					records[clientId] = record
				}
			})
		}
		if clientIds == nil {
			read(selectSql)
			return
		}
		const batchSize = 1024
		for start := 0; start < len(clientIds); start += batchSize {
			read(selectSql+` WHERE client_id = ANY($1)`, clientIds[start:min(start+batchSize, len(clientIds))])
		}
	})
	return records
}

// What the gate decides for a provider from its row at now, nil for no row.
// Evidence is fresh while it is newer than ProviderModernEvidenceMaxAge.
func classifyProviderModernEvidence(record *ProviderModernEvidenceRecord, now time.Time) string {
	switch {
	case record == nil:
		return ProviderModernOutcomeNoEvidence
	case !record.MeasuredAt.After(now.Add(-ProviderModernEvidenceMaxAge)):
		return ProviderModernOutcomeStaleEvidence
	case !record.PostQuantum:
		return ProviderModernOutcomeNotPostQuantum
	case !record.WindowResizable:
		return ProviderModernOutcomeNoResizableWindows
	default:
		return ProviderModernOutcomeModern
	}
}

// For each bucket of ProviderModernGateBuckets, every outcome at zero.
func newProviderModernGateShadowCounts() map[string]map[string]int64 {
	bucketOutcomeCounts := map[string]map[string]int64{}
	for _, bucket := range ProviderModernGateBuckets {
		outcomeCounts := map[string]int64{}
		for _, outcome := range ProviderModernOutcomes {
			outcomeCounts[outcome] = 0
		}
		bucketOutcomeCounts[bucket] = outcomeCounts
	}
	return bucketOutcomeCounts
}

// Counts the decision into the shadow: under its outcome, in each native
// bucket the other rules admit it to. A provider the gate takes out counts
// as it would with the gate off, so the shadow reads the same either way.
func countProviderModernGateShadow(bucketOutcomeCounts map[string]map[string]int64, decision providerEgressDecision) {
	if decision.qualityBeforeModernGate {
		bucketOutcomeCounts[RankModeQuality][decision.modernOutcome] += 1
	}
	if decision.speedBeforeModernGate {
		bucketOutcomeCounts[RankModeSpeed][decision.modernOutcome] += 1
	}
}

// One line for a pass's log: whether the gate is on at the pass's instant,
// the same decision the pass made, and how provider.yml sets it, and per
// bucket the providers the other rules admit, those of them the gate keeps,
// and every outcome that would take one out.
func providerModernGateShadowSummary(bucketOutcomeCounts map[string]map[string]int64, settings *EgressIndexSettings, now time.Time) string {
	state := "off, shadow only"
	if settings.RequiresModernProvider(now) {
		state = "on"
	}
	state = fmt.Sprintf("%s (%s)", state, settings.ModernProviderGateSchedule())
	bucketTexts := make([]string, 0, len(ProviderModernGateBuckets))
	for _, bucket := range ProviderModernGateBuckets {
		outcomeCounts := bucketOutcomeCounts[bucket]
		admitted := int64(0)
		outcomeTexts := make([]string, 0, len(ProviderModernOutcomes)-1)
		for _, outcome := range ProviderModernOutcomes {
			admitted += outcomeCounts[outcome]
			if outcome != ProviderModernOutcomeModern {
				outcomeTexts = append(outcomeTexts, fmt.Sprintf("%s %d", outcome, outcomeCounts[outcome]))
			}
		}
		bucketTexts = append(bucketTexts, fmt.Sprintf(
			"%s %d admitted, %d modern, %d not (%s)",
			bucket,
			admitted,
			outcomeCounts[ProviderModernOutcomeModern],
			admitted-outcomeCounts[ProviderModernOutcomeModern],
			strings.Join(outcomeTexts, ", "),
		))
	}
	return fmt.Sprintf("%s, evidence max age %s: %s", state, ProviderModernEvidenceMaxAge, strings.Join(bucketTexts, "; "))
}
