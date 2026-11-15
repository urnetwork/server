package model

// §12.8's CAA probe, dns side (DNS-M6-05; D-DNS-9, D-DNS-8, FS-32, FI-11): the canary certs publishes at a
// strong-mode custom host's owner while its probe runs, and takes out again, each one write through §12.6's
// path. POST /dns/internal/caa-canary is its only caller (dnshost.CaaCanaries), and certs' probe chain
// (TLS-M6-04) the route's.
//
//   - The owner is the host owner a strong-mode custom host of its network maps to, <host>.<label>.<root>,
//     pending or active: never a network's, alias's or scope's @, whose CAA would govern the zone's device
//     certificates (RFC 8659 §3), and never a weak-mode host's or a strict-mode MX host's, which certs checks
//     neither way. The custom host's row names the network.
//   - The RRset is the walk's effective set plus 0 urcanary "<nonce>" (dnsservice.DnsCaaCanaryRdata), TTL
//     ttl.canary, kind canary, so it expires an hour after the write, as a challenge does, and taskworker's
//     chain removes it then (RemoveExpiredDnsChallenges). Where the owner holds nothing else the write path
//     plans a copy of *'s records beside it, and drops the copy with it, so the host's addresses keep
//     answering from * (dnsservice.PlanCopyRows).
//   - One probe a target at a time: a canary standing at the owner refuses a publish
//     (dnsservice.ErrDnsCaaCanaryBusy) unless the publish replaces it by its nonce, as certs does after
//     /dns/ready answers stale, which takes a new nonce at a new serial (§13.8).
//   - A CNAME at the owner makes the zone lint refuse the canary (RFC 2181 §10.1), which the route answers as
//     the probe's failing verdict (dnsservice.ErrDnsCaaCanaryLint).
//   - A delete takes the canary and its copy out in one write, whatever the verdict.
//
// Canaries are free of dns_entries and add no admin event (§12.7). Nothing here runs unless the binary links
// MG2, MG5 and MG6.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The network whose strong-mode custom host maps to owner in root, and the host's zone apex, or a refusal
// wrapping dnsservice.ErrDnsCaaCanaryRefused.
func resolveDnsCaaCanaryOwnerInTx(ctx context.Context, tx server.PgTx, root string, owner string) (server.Id, string, error) {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", dnsservice.ErrDnsCaaCanaryRefused, fmt.Sprintf(format, args...))
	}
	var row *DnsCustomHostRow
	for _, candidate := range getDnsCustomHostRowsInTx(ctx, tx, `root = $1 AND owner = $2 AND caa_mode = $3`, root, owner, sidecar.DnsCustomHostModeStrong) {
		if !candidate.StrictMx() {
			row = candidate
			break
		}
	}
	if row == nil {
		return server.Id{}, "", refuse("no strong-mode custom host maps to %s", owner)
	}
	label, err := EnsureNetworkLabelInTx(ctx, tx, row.NetworkId, root)
	if err != nil {
		return server.Id{}, "", err
	}
	apex := label + "." + root
	// the row was a host owner when it was added; the zone may since hold a scope there
	hostLabel, err := dnsCustomHostOwnerLabel(owner, apex)
	if err != nil {
		return server.Id{}, "", refuse("%v", err)
	}
	if scopeLabels, _ := dnsNetworkScopeLabels(ctx, row.NetworkId); slices.Contains(scopeLabels, hostLabel) {
		return server.Id{}, "", refuse("%s is a Team scope's apex", owner)
	}
	return row.NetworkId, apex, nil
}

// The CAA canary standing at owner among rows, nil for none.
func dnsCaaCanaryRow(owner string, rows []*dnsStoredRow) *dnsStoredRow {
	for _, row := range rows {
		if row.set.Owner == owner && row.set.Type == dns.TypeCAA {
			return row
		}
	}
	return nil
}

// Publishes a probe's canary in the caller's transaction, which must be read committed (see the file header):
// put's RRset at put's owner, replacing a standing canary only by its nonce. Refusals wrap
// dnsservice.ErrDnsCaaCanaryRefused, dnsservice.ErrDnsCaaCanaryBusy or dnsservice.ErrDnsCaaCanaryLint; a
// restore's passes as dnsservice.ErrDnsRestoring. Returns the write.
func DnsPutCaaCanaryInTx(ctx context.Context, tx server.PgTx, root string, put *dnsservice.DnsCaaCanaryPut) (*DnsWriteResult, error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	owner := dns.CanonicalName(put.Owner)
	if dnsservice.DnsCaaCanaryNonce(put.Rdata) != put.Nonce || put.Nonce == "" {
		return nil, fmt.Errorf("%w: the RRset does not carry the publish's nonce", dnsservice.ErrDnsCaaCanaryRefused)
	}
	networkId, _, err := resolveDnsCaaCanaryOwnerInTx(ctx, tx, root, owner)
	if err != nil {
		return nil, err
	}
	// the zone's lock before its rows, so two publishes at one owner see each other
	lockDnsZoneInTx(ctx, tx, root, networkId)
	standing := dnsCaaCanaryRow(owner, getDnsZoneRowsInTx(ctx, tx, root, networkId))
	if standing != nil {
		if standing.set.Kind != dnsservice.DnsRecordKindCanary {
			return nil, fmt.Errorf("%w: %s holds a CAA that is no canary", dnsservice.ErrDnsCaaCanaryRefused, owner)
		}
		if nonce := dnsservice.DnsCaaCanaryNonce(standing.set.Rdata); put.Replaces == "" || nonce != put.Replaces {
			return nil, fmt.Errorf("%w: another probe's canary stands at %s", dnsservice.ErrDnsCaaCanaryBusy, owner)
		}
	}
	result, err := DnsWriteInTx(ctx, tx, &DnsWriteArgs{
		Root:      root,
		NetworkId: networkId,
		Upserts: []dnsservice.DnsRecordSet{{
			Owner: owner,
			Type:  dns.TypeCAA,
			Ttl:   put.Ttl,
			Kind:  dnsservice.DnsRecordKindCanary,
			Rdata: slices.Clone(put.Rdata),
		}},
	})
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, dnsservice.ErrDnsZoneLint):
		return nil, fmt.Errorf("%w: %w", dnsservice.ErrDnsCaaCanaryLint, err)
	case errors.Is(err, ErrDnsZoneSuspended), errors.Is(err, ErrDnsHostSuspended), errors.Is(err, ErrDnsOwnerOutsideZone),
		errors.Is(err, dnsservice.ErrDnsRowMalformed):
		return nil, fmt.Errorf("%w: %w", dnsservice.ErrDnsCaaCanaryRefused, err)
	}
	return nil, err
}

// DnsPutCaaCanaryInTx in a transaction of its own, whose commit raises the serial.
func DnsPutCaaCanary(ctx context.Context, root string, put *dnsservice.DnsCaaCanaryPut) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = DnsPutCaaCanaryInTx(ctx, tx, root, put)
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, err
	}
	return result, returnErr
}

// Takes the canary at owner and its copy out in the caller's transaction, which must be read committed, in one
// write, whatever the probe's verdict: dnsservice.ErrDnsWriteEmpty when none stands, and a refusal wrapping
// dnsservice.ErrDnsCaaCanaryRefused for an owner holding a CAA that is no canary or none of a network's.
func DnsDeleteCaaCanaryInTx(ctx context.Context, tx server.PgTx, root string, owner string) (*DnsWriteResult, error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	owner = dns.CanonicalName(owner)
	var networkId *server.Id
	var kind string
	found := false
	result, err := tx.Query(
		ctx,
		`SELECT network_id, kind FROM network_dns_record WHERE root = $1 AND owner = $2 AND rrtype = $3`,
		root,
		owner,
		int32(dns.TypeCAA),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&networkId, &kind))
			found = true
		}
	})
	switch {
	case !found:
		return nil, fmt.Errorf("%w: no canary at %s", dnsservice.ErrDnsWriteEmpty, owner)
	case networkId == nil || kind != string(dnsservice.DnsRecordKindCanary) || !strings.HasSuffix(owner, "."+root):
		return nil, fmt.Errorf("%w: %s holds no network's canary", dnsservice.ErrDnsCaaCanaryRefused, owner)
	}
	return DnsWriteInTx(ctx, tx, &DnsWriteArgs{
		Root:      root,
		NetworkId: *networkId,
		Deletes:   []dnsservice.DnsRecordKey{{Owner: owner, Type: dns.TypeCAA}},
	})
}

// DnsDeleteCaaCanaryInTx in a transaction of its own.
func DnsDeleteCaaCanary(ctx context.Context, root string, owner string) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = DnsDeleteCaaCanaryInTx(ctx, tx, root, owner)
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, err
	}
	return result, returnErr
}
