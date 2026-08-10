package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// GeolocationSourceHosts is the set of hosts the geolocation prober reaches
// through a provider tunnel, and therefore the set this server observes
// certificate pins for.
//
// # This list has a counterpart in another repository
//
// Its counterpart is `geolocate/sources.go` in the operator-proxy repo
// (github.com/urnetwork/operator-proxy), whose `sources` table is the
// authority on which endpoints are actually queried; `geolocate.SourceHosts()`
// derives the host set from it. The two CANNOT be linked in code: the prober is
// a separate Go module in a separate repository, and it depends on this server
// rather than the other way round -- importing the prober from the server would
// invert that dependency. So this is a deliberate second copy, and the comment
// is the only thing keeping it honest. When a source endpoint changes there,
// change it here in the same pass.
//
// # Drift is caught at runtime, fail-closed, not silently
//
// That is not merely a promise. The prober treats a source host with no served
// pin as a hard error and refuses to probe rather than probing unpinned, so a
// host added to `sources.go` but not to this list stops the prober at startup
// instead of quietly leaving one source unprotected. A host removed there but
// left here only costs a pointless observation. The dangerous direction is the
// one that fails loudly.
//
// This list is a compile-time constant on purpose: it is a trust decision about
// which hosts the server will vouch for, and nothing outside a code change --
// no request, no database row, and above all no provider -- may add to it.
var GeolocationSourceHosts = []string{
	// ip.pn -- moved from ip.pn to api.i.pn on 2026-08-02, which is exactly
	// the drift this comment block exists for.
	"api.i.pn",
	"free.freeipapi.com",
	"ipinfo.io",
}

// GeolocationSourcePin is the certificate pin this server observed for one
// geolocation source host, by connecting to it DIRECTLY -- on the server's own
// network, with no provider anywhere in the path -- and validating the chain
// under full WebPKI.
//
// That direct, validated observation is the entire basis for trusting the
// value. The geolocation lookup itself is issued THROUGH a provider's tunnel
// precisely so a provider forging its own location can be caught; if a pin
// could ever be learned from a connection that traversed a tunnel, or from one
// whose chain was not verified, the provider under test could teach this server
// its own forged certificate and the pin would authenticate the attacker. See
// the observation job for the code that upholds this.
//
// Both a leaf and an intermediate pin are recorded because the prober's check
// (providertunnel.checkPin) accepts a match anywhere in the verified chain: the
// intermediate is what absorbs routine leaf renewal without a redeploy, and the
// leaf is the tighter of the two while it lasts.
type GeolocationSourcePin struct {
	Host             string
	LeafSpki         string
	IntermediateSpki string
	ObservedAt       time.Time
}

// SetGeolocationSourcePin upserts the observed pin for one host and returns the
// row it REPLACED, or nil when the host had never been observed.
//
// Returning the previous row is what makes rotation visible. The outage this
// mechanism exists to prevent was invisible: a pin that had gone stale failed
// closed, the source dropped out of the set, and the fleet reported
// "no_consensus" -- indistinguishable from "fewer sources answered". The caller
// logs old and new from this return value, so a rotation leaves a record even
// though the table itself keeps no history.
//
// The read and the write are in one transaction so the returned "old" value is
// the one this write actually replaced, not whatever a separate earlier read
// happened to see.
//
// One row per host, replaced in place: this is the current picture, and history
// belongs in the log line rather than in a second key column that every reader
// would then have to filter on.
func SetGeolocationSourcePin(ctx context.Context, pin *GeolocationSourcePin) *GeolocationSourcePin {
	var previous *GeolocationSourcePin

	server.Tx(ctx, func(tx server.PgTx) {
		// server.Tx may rerun this closure, so start from a clean slate rather
		// than carrying an abandoned attempt's reading forward. This does not
		// cover the case where a commit succeeded but was REPORTED as failed:
		// the rerun's select then finds the row it just wrote and the rotation
		// goes unlogged once. That is rare, self-corrects at the next pass
		// (the certificate genuinely has not changed by then), and fixing it
		// properly means changing server.Tx's retry semantics for everyone.
		previous = nil

		result, err := tx.Query(
			ctx,
			`
			SELECT
				leaf_spki,
				intermediate_spki,
				observed_at
			FROM geolocation_source_pin
			WHERE host = $1
			FOR UPDATE
			`,
			pin.Host,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				p := &GeolocationSourcePin{Host: pin.Host}
				server.Raise(result.Scan(
					&p.LeafSpki,
					&p.IntermediateSpki,
					&p.ObservedAt,
				))
				previous = p
			}
		})

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO geolocation_source_pin (
				host,
				leaf_spki,
				intermediate_spki,
				observed_at
			)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (host) DO UPDATE
			SET
				leaf_spki = $2,
				intermediate_spki = $3,
				observed_at = $4
			`,
			pin.Host,
			pin.LeafSpki,
			pin.IntermediateSpki,
			// naive timestamp column holding utc, as everywhere else in this
			// schema
			pin.ObservedAt.UTC(),
		))
	})

	return previous
}

// GetGeolocationSourcePin reads one host's observed pin, or nil when the host
// has never been successfully observed.
//
// Never observed is not the same as observed-and-empty: a caller that could not
// tell those apart would be one step from serving an empty pin set, which is
// the same as serving no pin at all.
func GetGeolocationSourcePin(ctx context.Context, host string) *GeolocationSourcePin {
	var pin *GeolocationSourcePin

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				leaf_spki,
				intermediate_spki,
				observed_at
			FROM geolocation_source_pin
			WHERE host = $1
			`,
			host,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				p := &GeolocationSourcePin{Host: host}
				server.Raise(result.Scan(
					&p.LeafSpki,
					&p.IntermediateSpki,
					&p.ObservedAt,
				))
				pin = p
			}
		})
	})

	return pin
}

// GetGeolocationSourcePins reads every observed pin, keyed by host.
//
// It returns exactly what has been observed and never synthesizes a row for a
// host in GeolocationSourceHosts that has not been observed yet. A missing host
// must stay missing all the way to the consumer, because the consumer's correct
// response to a missing pin is to refuse to probe -- and a placeholder row here
// would take that decision away from it.
func GetGeolocationSourcePins(ctx context.Context) map[string]*GeolocationSourcePin {
	pins := map[string]*GeolocationSourcePin{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				host,
				leaf_spki,
				intermediate_spki,
				observed_at
			FROM geolocation_source_pin
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				p := &GeolocationSourcePin{}
				server.Raise(result.Scan(
					&p.Host,
					&p.LeafSpki,
					&p.IntermediateSpki,
					&p.ObservedAt,
				))
				pins[p.Host] = p
			}
		})
	})

	return pins
}
