// Which URL turns measure post-quantum (FP2-PROTOCOL.md §12.2, with the
// schedule below in place of "every URL probe tunnel").
//
// A turn that measures post-quantum opens an opportunistic end-to-end session
// with its provider, which the provider can open only with the probe client's
// published key: the turn's derived client registers its key before
// admission, a signed st_client_key_history row of about 5 KB plus evidence
// blobs, kept at least 30 days (FP2FIX.md, "URL tunnel client keys"). On every
// turn, about 68 a second, that is about 29 GB a day of Postgres, the
// 2026-10-14 incident's cost again. So the claim asks for the session only
// when the provider needs a measurement: it has no measured post-quantum
// outcome (positive or negative) within RefreshAge, and no turn of its asked
// for one within RetryInterval. About two measured outcomes per provider land
// in each eight-hour window strict selection reads, at about a tenth of the
// registrations; a provider whose turns cannot measure (its HTTPS leg fails,
// its tunnel is lost) is asked at most once per RetryInterval.
//
// The flag is decided after the claim's transaction commits, from the history
// and one Redis key per provider, and never fails a claim: an unreadable
// history or Redis leaves the turn without a session, as a prober that
// predates the flag runs every turn.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The post-quantum turn schedule.
type ProviderUrlProbePostQuantumSettings struct {
	// A provider whose newest measured post-quantum outcome is younger than
	// this needs no session. Four hours puts about two measured outcomes in
	// each eight-hour window (ProviderEgressHealthMaxAge), so one turn that
	// cannot measure still leaves a fresh outcome in the window. A provider
	// that loses post-quantum reads negative within about this long.
	RefreshAge time.Duration
	// The least time between two turns of one provider that ask for a
	// session, measured or not, so a provider whose turns never measure
	// registers at most one key per interval. Non-positive asks on every
	// turn that needs a measurement.
	RetryInterval time.Duration
}

// The schedule's settings.
func DefaultProviderUrlProbePostQuantumSettings() *ProviderUrlProbePostQuantumSettings {
	return &ProviderUrlProbePostQuantumSettings{
		RefreshAge:    4 * time.Hour,
		RetryInterval: time.Hour,
	}
}

// The key that holds a provider's latest post-quantum turn for RetryInterval.
func providerUrlProbePostQuantumTurnKey(clientId server.Id) string {
	return fmt.Sprintf("url_probe_post_quantum_turn_%s", clientId)
}

// Sets PostQuantum on the claimed providers whose turn should measure
// post-quantum at now. Runs after the claim commits: it holds no claim lock,
// and a turn it marks is one the claim already issued.
func markProviderUrlProbePostQuantumTurns(ctx context.Context, now time.Time, providers []ProviderUrlProbeDue, settings *ProviderUrlProbePostQuantumSettings) {
	if len(providers) == 0 {
		return
	}
	server.HandleError(func() {
		clientIds := make([]server.Id, 0, len(providers))
		for _, provider := range providers {
			clientIds = append(clientIds, provider.ClientId)
		}
		// providers with a measured outcome young enough need none
		measuredClientIds := map[server.Id]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `
				SELECT DISTINCT client_id FROM provider_egress_health_history
				WHERE client_id = ANY($1) AND measured_at > $2 AND measured_at <= $3 AND pq_outcome <> 0`,
				clientIds, now.Add(-settings.RefreshAge).UTC(), now.UTC())
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var clientId server.Id
					server.Raise(rows.Scan(&clientId))
					measuredClientIds[clientId] = true
				}
			})
		})
		server.Redis(ctx, func(r server.RedisClient) {
			for i := range providers {
				provider := &providers[i]
				if measuredClientIds[provider.ClientId] {
					continue
				}
				if settings.RetryInterval <= 0 {
					provider.PostQuantum = true
					continue
				}
				// one turn per interval wins the key; the rest of the
				// interval's turns open no session
				asked, err := r.SetNX(ctx, providerUrlProbePostQuantumTurnKey(provider.ClientId), now.UTC().Format(time.RFC3339), settings.RetryInterval).Result()
				server.Raise(err)
				provider.PostQuantum = asked
			}
		})
	}, func(err error) {
		glog.Warningf("[egress]post-quantum turns were not marked for %d claimed providers: %s\n", len(providers), err)
	})
}
