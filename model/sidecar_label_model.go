package model

// GET /sidecar/label's read of a network's alias (§12.5; owner decision 421, QD-935): the controller
// allocates the label with EnsureNetworkLabelInTx and reads the alias here, in the same transaction. An
// alias is a dns_label row of kind alias, which ClaimDnsAlias writes (dns_alias_model.go), so this reads
// MG2's columns alone and runs in a binary that links MG2 without MG6, where no claim can have written one.

import (
	"context"

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
