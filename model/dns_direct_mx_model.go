package model

// §12.8's direct MX (DNS-M7-03; D-MAIL-12, FS-33, FI-11; owner decision 33), Team only: once mxedge has
// verified each mail indexer of §15.4's POST /mail/direct-mx (MAIL-M7-23), mx.<net>.<root>'s A and AAAA
// name their addresses in place of the router interfaces, checked against §12.7's A rules, and the TLSA
// set at _25._tcp.mx stays. DELETE restores the router rows, and a receiver's removal drops its address
// (I-15), its caller naming the addresses still verified.
//
// The addresses are rows of kind direct at the mail host, which every reconcile of the zone's system set
// writes back as they are (DnsZoneState.DirectMx), so mail on or a published host never undoes a direct
// MX, and mail off takes them with the rest of mx. These records stay in URnetwork's zone, so a direct MX
// does not exclude URnetwork (§2.8 EX2): an operator's write to them redirects it, which FS-33 shows. Only
// a strict-mode MX host in the customer's own signed zone (dns_custom_host_model.go) carries strict mode's
// exclusion, and dns publishes nothing for one.
//
// Both writers run in the caller's transaction, MAIL's, one journal write each, and need MG2, MG5 and MG6.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A direct MX needs mail on, since it answers at the mail host mail on writes.
var ErrDnsDirectMxMailOff = errors.New("dns direct mx needs mail on")

// The direct MX the zone's direct rows at its mail host hold, nil for the routers.
func getDnsDirectMxInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id, apex string) []netip.Addr {
	var addresses []netip.Addr
	result, err := query.Query(
		ctx,
		`
			SELECT rdata FROM network_dns_record
			WHERE root = $1 AND owner = $2 AND network_id = $3 AND kind = $4 AND withheld_time IS NULL
			ORDER BY rrtype
		`,
		root,
		dnsMailHostLabel+"."+apex,
		networkId,
		string(dnsservice.DnsRecordKindDirect),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var rdata []string
			server.Raise(result.Scan(&rdata))
			for _, text := range rdata {
				if address, err := netip.ParseAddr(strings.TrimSpace(text)); err == nil {
					addresses = append(addresses, address)
				}
			}
		}
	})
	return addresses
}

// Points the network's mail host in root at a direct MX's verified mail indexers, addresses, in the
// caller's transaction, which must be read committed: Team only, mail on, each address held to §12.7's A
// rules. A call naming fewer addresses than the last drops the rest, as a receiver's removal does (I-15).
// One journal write, none when mx already names exactly these. ttl nil takes dns.yml's. Returns the write,
// nil when nothing changed.
func DnsSetDirectMxInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, addresses []netip.Addr, ttl *dnsservice.DnsTtlSettings) (*DnsWriteResult, error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	if err := dnsservice.ValidateDirectMxAddresses(addresses, &dnsZoneSettings().UserRecords); err != nil {
		return nil, err
	}
	if err := dnsRequireTeamTierInTx(ctx, tx, networkId); err != nil {
		return nil, err
	}
	root = dnsLabelRoot(root)
	if err := requireDnsMailOnInTx(ctx, tx, networkId, root); err != nil {
		return nil, err
	}
	return ensureNetworkZoneInTx(ctx, tx, networkId, root, ttl, &addresses)
}

// Restores the router interfaces at the network's mail host in root, in the caller's transaction, which
// must be read committed: §15.4's DELETE /mail/direct-mx. One journal write, none when no direct MX
// stands. ttl nil takes dns.yml's. Returns the write, nil when nothing changed.
func DnsClearDirectMxInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, ttl *dnsservice.DnsTtlSettings) (*DnsWriteResult, error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	routers := []netip.Addr{}
	return ensureNetworkZoneInTx(ctx, tx, networkId, root, ttl, &routers)
}

// Refuses unless the network's zone in root has mail on, read under the label's and the zone's locks, as
// the zone's writers take them.
func requireDnsMailOnInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string) error {
	if _, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root); err != nil {
		return err
	}
	lockDnsZoneInTx(ctx, tx, root, networkId)
	if state := getDnsZoneStateInTx(ctx, tx, root, networkId); state == nil || !state.Mail {
		return fmt.Errorf("%w: %s", ErrDnsDirectMxMailOff, networkId)
	}
	return nil
}
