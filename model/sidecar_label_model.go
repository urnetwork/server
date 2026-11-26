package model

// GET /sidecar/label's read of a network's alias (§12.5; owner decision 421, QD-935): the controller
// allocates the label with EnsureNetworkLabelInTx and reads the alias here, in the same transaction, and
// the aliases retiring with it, which GET /dns/zone's view reads too, so a sidecar's label table keeps a
// changed or released alias for its 90 days. An alias is a dns_label row of kind alias, which ClaimDnsAlias
// writes (dns_alias_model.go), so this reads MG2's columns alone and runs in a binary that links MG2
// without MG6, where no claim can have written one.

import (
	"context"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// The network's live alias in root, as dns_label holds it, a label with no root: "" while the network has
// none or in a binary that does not link MG2. A retiring alias is no longer the network's alias, and a
// tombstone never is.
func GetNetworkDnsAliasNameInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, root string) string {
	if !server.SidecarMg2Linked() {
		return ""
	}
	alias := ""
	result, err := query.Query(
		ctx,
		`
			SELECT label FROM dns_label
			WHERE network_id = $1 AND root = $2 AND kind = $3 AND state = $4
			ORDER BY state_time DESC, label
			LIMIT 1
		`,
		networkId,
		dnsLabelRoot(root),
		DnsLabelKindAlias,
		DnsLabelStateLive,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&alias))
		}
	})
	return alias
}

// The network's retiring aliases in root, as dns_label holds them, by label: each a label with no root and
// the time it tombstones, DnsAliasSettings.RetiringWindow after the state_time its release or change set, as
// the alias routes answer it (dnsAliasAnswer). One whose window has passed stays until the 90-day job
// tombstones it, as dns_label holds it, and a reader past its time drops it. nil for none, and in a binary
// that does not link MG2; a tombstone and another network's alias never.
func GetNetworkDnsRetiringAliasesInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id, root string) []sidecar.NetworkLabelRetiringAlias {
	var retiringAliases []sidecar.NetworkLabelRetiringAlias
	if !server.SidecarMg2Linked() {
		return nil
	}
	settings := DefaultDnsAliasSettings()
	result, err := query.Query(
		ctx,
		`
			SELECT label, state_time FROM dns_label
			WHERE network_id = $1 AND root = $2 AND kind = $3 AND state = $4
			ORDER BY label
		`,
		networkId,
		dnsLabelRoot(root),
		DnsLabelKindAlias,
		DnsLabelStateRetiring,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var alias string
			var stateTime time.Time
			server.Raise(result.Scan(&alias, &stateTime))
			retiringAliases = append(retiringAliases, sidecar.NetworkLabelRetiringAlias{
				Alias:         alias,
				TombstoneTime: stateTime.Add(settings.RetiringWindow).UTC(),
			})
		}
	})
	return retiringAliases
}
