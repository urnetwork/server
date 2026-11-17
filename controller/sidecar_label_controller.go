package controller

// GET /sidecar/label (§12.5; plan-DNS-labels-before-m5, QD-021; owner decision 421, QD-935): the bearer's own
// network's label, allocated on first need, and its live alias, which feed a sidecar's NetworkLabelTable from M2,
// before the dns block's GET /dns/zone answers the same from M5. The answer is the sidecar's own
// sidecar.SidecarLabel, so the body is what the client's GetSidecarLabel reads. Client class and on §7.4's
// strict_exp allowlist (api/route_authz.go): any credential of the network reaches it, a sidecar's token among
// them, and a forwarding client's token is refused before this runs.
//
//   - The network is the credential's, never the request's: the route reads no parameter and no body, so no
//     request names another network.
//   - The root is dns.yml's one root, as /network/dns's routes serve it (networkDnsRoot); with none the route
//     answers 503, never a label of another root.
//   - The label is model.EnsureNetworkLabelInTx's, drawn under the network's label lock, so two first requests
//     at once allocate one label and every later one draws nothing. The alias is
//     model.GetNetworkDnsAliasNameInTx's, dns_label's live alias row, read in the same transaction and left out
//     while the network has none.
//   - A binary that does not link MG2 holds no dns_label, and the route answers 503 without a query, never a
//     label that is not the network's.
//
// No sidecar.yml gate applies: a label names its network in .ur names, git-remote-ur, §9.4's addresses and
// network:<net> from M2, before §19.11's dns gate opens the dns service at M5. Until MG2's Gn-out registers the
// group, the route is dark by MG2's link alone.

import (
	"errors"
	"net/http"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The route's refusal while it cannot answer a label: 503, as the dns routes answer theirs, so a sidecar
// keeps the table it has and asks again later.
func sidecarLabelUnavailable(message string) error {
	return &sidecar.DnsApiError{Status: http.StatusServiceUnavailable, Code: sidecar.DnsApiCodeUnavailable, Message: message}
}

// GET /sidecar/label: {network_id, label, alias} for the credential's own network, the label allocated on
// first need and the alias omitted while there is none.
func GetSidecarLabel(clientSession *session.ClientSession) (*sidecar.SidecarLabel, error) {
	if !server.SidecarMg2Linked() {
		return nil, sidecarLabelUnavailable("network labels are not available")
	}
	root, err := networkDnsRoot()
	if err != nil {
		return nil, err
	}
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	answer := &sidecar.SidecarLabel{NetworkId: sidecar.Id(networkId)}
	var labelErr error
	server.Tx(ctx, func(tx server.PgTx) {
		answer.Label, labelErr = model.EnsureNetworkLabelInTx(ctx, tx, networkId, root)
		if labelErr != nil {
			// nothing was written: each refused draw inserted no row
			return
		}
		answer.Alias = model.GetNetworkDnsAliasNameInTx(ctx, tx, networkId, root)
	}, server.TxReadCommitted)
	switch {
	case errors.Is(labelErr, model.ErrDnsLabelUnlinked):
		return nil, sidecarLabelUnavailable("network labels are not available")
	case errors.Is(labelErr, model.ErrDnsLabelDrawsSpent):
		return nil, sidecarLabelUnavailable("no label could be drawn")
	case labelErr != nil:
		return nil, labelErr
	}
	return answer, nil
}
