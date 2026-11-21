package model

// What §13.8's network-level identifier set is built from on the server, for
// certs' order route (QD-846; controller.InstallTlsCerts): the network's label
// in a root and its live aliases, the same reads POST /dns/key-record builds
// its set from (dns_key_record_model.go), so an order and a published key
// attest one set.
//
// A Team scope's set is TLS-M6-03's and is not read here.

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
)

// The names could not be read. The cause stays in the log.
var ErrTlsCertNamesUnavailable = errors.New("the network's names could not be read")

// The network's label in root and its live aliases, in name order. With
// allocate, a network that has no label yet takes one, §12.5's first need at
// an order; without, such a network answers no label and nothing is written.
// A binary that does not link MG2 answers ErrDnsLabelUnlinked.
func GetNetworkTlsCertNames(ctx context.Context, networkId server.Id, root string, allocate bool) (label string, aliases []string, returnErr error) {
	if !server.SidecarMg2Linked() {
		return "", nil, ErrDnsLabelUnlinked
	}
	root = dnsLabelRoot(root)
	if r := server.HandleError(func() {
		if allocate {
			server.Tx(ctx, func(tx server.PgTx) {
				label, returnErr = EnsureNetworkLabelInTx(ctx, tx, networkId, root)
				if returnErr != nil {
					return
				}
				aliases = getNetworkLiveAliasesInTx(ctx, tx, networkId, root)
			}, server.TxReadCommitted)
			return
		}
		label, aliases = getNetworkLabelAndLiveAliases(ctx, networkId, root)
	}); r != nil {
		return "", nil, ErrTlsCertNamesUnavailable
	}
	if returnErr != nil || label == "" {
		return "", nil, returnErr
	}
	return label, aliases, nil
}

// The network's label and live aliases for a non-allocating certificate or SMTP gate read.
func getNetworkLabelAndLiveAliases(ctx context.Context, networkId server.Id, root string) (label string, aliases []string) {
	if !server.SidecarMg2Linked() {
		return "", nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		label = getNetworkLabelInTx(ctx, conn, networkId, root)
		if label != "" {
			aliases = getNetworkLiveAliasesInTx(ctx, conn, networkId, root)
		}
	})
	return
}
