// Which URL turns measure post-quantum (FP2-PROTOCOL.md §12.2): those whose
// provider has no measured outcome in the last four hours, in place of every
// URL probe tunnel (owner decision 2026-11-12, "accepted").
//
// A turn that measures post-quantum opens an opportunistic end-to-end session
// with its provider, which the provider can open only with the probe client's
// published key: the turn's derived client registers its key before
// admission, a signed st_client_key_history row of about 5 KB plus evidence
// blobs, kept at least 30 days (FP2FIX.md, "URL tunnel client keys"). On every
// turn, about 68 a second, that is about 29 GB a day of Postgres, the
// 2026-10-14 incident's cost again. So the claim asks for the session only
// when the provider needs a measurement: it has no measured post-quantum
// outcome (positive or negative) within RefreshAge. About two measured
// outcomes per provider land in each eight-hour window strict selection
// reads, at about a tenth of the registrations.
//
// A provider whose turns cannot measure (its HTTPS leg fails, its tunnel is
// lost) would otherwise be asked on every turn. It is asked on one turn in
// TurnStride, chosen by the turn's claim ordinal, so the choice is the
// claim's own state: the same claim decides the same way, and a rolled-back
// claim leaves nothing behind.
//
// A turn that asked a provider without the end-to-end responder counts as
// asked for RefreshAge, as a measured outcome does (owner decision
// 2026-11-13): its session stayed unmeasured, and the provider's
// acknowledgements advertised no receive window (window fixed), a provider
// that predates both. Its turns never conclude, so the stride alone asked it
// on every third turn, forever; counted as asked, it is asked about twice in
// each eight-hour window, as a provider that concludes is. The outcome stays
// unmeasured, so strict selection still needs a positive one.
//
// The flag is decided after the claim's transaction commits, from the
// history, and never fails a claim: an unreadable history leaves the turn
// without a session, as a prober that predates the flag runs every turn.
package model

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The post-quantum turn schedule.
type ProviderUrlProbePostQuantumSettings struct {
	// A provider whose newest measured post-quantum outcome is younger than
	// this needs no session, nor one a turn asked this recently without an
	// end-to-end responder to answer (window fixed, session unmeasured). Four
	// hours puts about two measured outcomes in each eight-hour window
	// (ProviderEgressHealthMaxAge), so one turn that cannot measure still
	// leaves a fresh outcome in the window. A provider that loses
	// post-quantum reads negative within about this long.
	RefreshAge time.Duration
	// A provider that needs a measurement is asked on the turns whose claim
	// ordinal is a multiple of this, one in three by default: about an hour
	// apart at the 24-minute renewal spacing, so a provider whose turns never
	// measure registers a key on at most a third of its turns, and one that
	// needs a measurement waits about an hour for it. One or less asks on
	// every turn that needs a measurement.
	TurnStride int
}

// The schedule's settings.
func DefaultProviderUrlProbePostQuantumSettings() *ProviderUrlProbePostQuantumSettings {
	return &ProviderUrlProbePostQuantumSettings{
		RefreshAge: 4 * time.Hour,
		TurnStride: 3,
	}
}

// Sets PostQuantum on the claimed providers whose turn should measure
// post-quantum at now. Runs after the claim commits: it holds no claim lock,
// and a turn it marks is one the claim already issued.
func markProviderUrlProbePostQuantumTurns(ctx context.Context, now time.Time, providers []ProviderUrlProbeDue, settings *ProviderUrlProbePostQuantumSettings) {
	// only a turn on the stride can be asked, so only its provider's history
	// is read
	strideClientIds := []server.Id{}
	for _, provider := range providers {
		if settings.TurnStride <= 1 || provider.ClaimOrdinal%int64(settings.TurnStride) == 0 {
			strideClientIds = append(strideClientIds, provider.ClientId)
		}
	}
	if len(strideClientIds) == 0 {
		return
	}
	server.HandleError(func() {
		// the stored e2e evidence of a turn that asked a provider without the
		// end-to-end responder: unmeasured, and acknowledged with no window
		withoutResponderJson, err := json.Marshal(map[string]map[string]string{
			"e2e": {"pq": egresshealth.UrlProbePqUnmeasured, "window": egresshealth.UrlProbeWindowFixed},
		})
		server.Raise(err)
		// providers with a measured outcome young enough need none, nor do
		// those a turn asked as recently without a responder to answer
		askedClientIds := map[server.Id]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `
				SELECT DISTINCT client_id FROM provider_egress_health_history
				WHERE client_id = ANY($1) AND measured_at > $2 AND measured_at <= $3
				AND (pq_outcome <> 0 OR url_probe_evidence @> $4::jsonb)`,
				strideClientIds, now.Add(-settings.RefreshAge).UTC(), now.UTC(), string(withoutResponderJson))
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var clientId server.Id
					server.Raise(rows.Scan(&clientId))
					askedClientIds[clientId] = true
				}
			})
		})
		for i := range providers {
			provider := &providers[i]
			onStride := settings.TurnStride <= 1 || provider.ClaimOrdinal%int64(settings.TurnStride) == 0
			provider.PostQuantum = onStride && !askedClientIds[provider.ClientId]
		}
	}, func(err error) {
		glog.Warningf("[egress]post-quantum turns were not marked for %d claimed providers: %s\n", len(providers), err)
	})
}
