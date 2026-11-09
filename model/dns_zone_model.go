package model

// A network's zone in a root (DNS-M5-11; plan reading 2): network_dns_zone's
// row, its label, and §12.7's system set written from the row's state. A zone
// comes to be on first need, as its label does (§12.5): a first publish or
// order, or /mail/enable. Every change of its state recomputes the whole set
// (dns_system_records_model.go) and writes one journal row when it changed and
// none otherwise.

import (
	"context"
	"errors"
	"sync"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// dns.yml as config mounts it, which dns's blocks read and api may not hold.
const dnsSettingsResourceName = "dns.yml"

// The TTLs a zone's system set is written with: dns.yml's when the process
// has it, else §12.6's defaults, which are the values dns.yml is held to. Read
// once a process.
var dnsZoneTtl = sync.OnceValue(func() *dnsservice.DnsTtlSettings {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	resource, err := server.Config.SimpleResource(dnsSettingsResourceName)
	if err != nil {
		return &ttl
	}
	data, err := resource.BytesE()
	if err != nil {
		glog.Warningf("[dns]%s unreadable, zones take §12.6's TTLs: %v\n", dnsSettingsResourceName, err)
		return &ttl
	}
	settings, err := dnsservice.LoadDnsSettings(data)
	if err != nil {
		glog.Warningf("[dns]%s refused, zones take §12.6's TTLs: %v\n", dnsSettingsResourceName, err)
		return &ttl
	}
	return &settings.Ttl
})

// The zone's state as its system set reads it, or nil before its row.
func getDnsZoneStateInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id) (state *DnsZoneState, mail bool) {
	result, err := query.Query(
		ctx,
		`SELECT edge_wildcard, edge_apex, mail FROM network_dns_zone WHERE root = $1 AND network_id = $2`,
		root,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			state = &DnsZoneState{}
			server.Raise(result.Scan(&state.EdgeWildcard, &state.EdgeApex, &mail))
		}
	})
	return
}

// The network's zone in root, its label allocated and its system set written
// on first need, in the caller's transaction, which must be read committed. A
// zone that exists has its set reconciled, which writes nothing when nothing
// changed. ttl nil takes dns.yml's. Returns the write, or nil when nothing was
// written. A suspended zone answers ErrDnsZoneSuspended, and its set waits for
// Restore.
func EnsureNetworkZoneInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, ttl *dnsservice.DnsTtlSettings) (*DnsWriteResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	if ttl == nil {
		ttl = dnsZoneTtl()
	}
	root = dnsLabelRoot(root)
	label, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root)
	if err != nil {
		return nil, err
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_dns_zone (root, network_id, create_time) VALUES ($1, $2, $3)
			ON CONFLICT (root, network_id) DO NOTHING
		`,
		root,
		networkId,
		server.NowUtc(),
	))
	state, mail := getDnsZoneStateInTx(ctx, tx, root, networkId)
	if mail {
		return nil, ErrDnsMailRecordsUnbuilt
	}
	state.Apex = label + "." + root
	result, err := reconcileDnsSystemRowsInTx(ctx, tx, root, networkId, dnsZoneSystemKeys(state.Apex), DnsSystemRecords(state, ttl))
	if err != nil {
		return nil, err
	}
	if result != nil && 0 < result.Serial {
		touchDnsLabelMapInTx(ctx, tx, root, networkId, result.Serial)
	}
	return result, nil
}

// EnsureNetworkZoneInTx in a transaction of its own.
func EnsureNetworkZone(ctx context.Context, networkId server.Id, root string) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = EnsureNetworkZoneInTx(ctx, tx, networkId, root, nil)
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

// Sets whether * and @ answer the healthy lb interfaces (§12.7;
// plan-DNS-edge-record-trigger: * while the zone's set holds any host but @
// or the system publish, @ while it holds @), creating the zone on first need,
// in the caller's transaction. One journal write a transition, none otherwise.
// Its caller is api's recompute after each accepted config delta and the
// hourly pass over the published names, which are other tasks'.
func DnsSyncEdgeRecordsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, wildcard bool, apex bool, ttl *dnsservice.DnsTtlSettings) (*DnsWriteResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	lockDnsZoneInTx(ctx, tx, root, networkId)
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_dns_zone (root, network_id, create_time, edge_wildcard, edge_apex) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (root, network_id) DO UPDATE SET edge_wildcard = EXCLUDED.edge_wildcard, edge_apex = EXCLUDED.edge_apex
		`,
		root,
		networkId,
		server.NowUtc(),
		wildcard,
		apex,
	))
	return EnsureNetworkZoneInTx(ctx, tx, networkId, root, ttl)
}

// DnsSyncEdgeRecordsInTx in a transaction of its own.
func DnsSyncEdgeRecords(ctx context.Context, networkId server.Id, root string, wildcard bool, apex bool) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = DnsSyncEdgeRecordsInTx(ctx, tx, networkId, root, wildcard, apex, nil)
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

// Whether err is a refusal a zone write may answer for a reason of its own
// state rather than a failure: restoring or suspended.
func IsDnsZoneWaiting(err error) bool {
	return errors.Is(err, dnsservice.ErrDnsRestoring) || errors.Is(err, ErrDnsZoneSuspended)
}
