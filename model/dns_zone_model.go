package model

// A network's zone in a root (DNS-M5-11; plan reading 2): network_dns_zone's
// row, its label, and §12.7's system set written from the row's state. A zone
// comes to be on first need, as its label does (§12.5): a first publish or
// order, or /mail/enable. Every change of its state recomputes the whole set
// (dns_system_records_model.go) and writes one journal row when it changed and
// none otherwise.

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"io"
	"net/netip"
	"strings"
	"sync"

	"github.com/miekg/dns"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// dns.yml as config mounts it, which dns's blocks read and api may not hold.
const dnsSettingsResourceName = "dns.yml"

// dns.yml as this process reads it, for what the model writes and judges by: the
// file's when the process has it and it loads, else DefaultDnsSettings, whose
// TTLs, caps and windows are the values dns.yml is held to. Read once a process.
var dnsZoneSettings = sync.OnceValue(func() *dnsservice.DnsSettings {
	resource, err := server.Config.SimpleResource(dnsSettingsResourceName)
	if err != nil {
		return dnsservice.DefaultDnsSettings()
	}
	data, err := resource.BytesE()
	if err != nil {
		glog.Warningf("[dns]%s unreadable, zones take §12.6's defaults: %v\n", dnsSettingsResourceName, err)
		return dnsservice.DefaultDnsSettings()
	}
	settings, err := dnsservice.LoadDnsSettings(data)
	if err != nil {
		glog.Warningf("[dns]%s refused, zones take §12.6's defaults: %v\n", dnsSettingsResourceName, err)
		return dnsservice.DefaultDnsSettings()
	}
	return settings
})

// The TTLs a zone's system set is written with: dnsZoneSettings'.
func dnsZoneTtl() *dnsservice.DnsTtlSettings {
	return &dnsZoneSettings().Ttl
}

// The zone's state as its system set reads it, with no root or apex, or nil
// before its row.
func getDnsZoneStateInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id) (state *DnsZoneState) {
	result, err := query.Query(
		ctx,
		`SELECT edge_wildcard, edge_apex, mail FROM network_dns_zone WHERE root = $1 AND network_id = $2`,
		root,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			state = &DnsZoneState{}
			server.Raise(result.Scan(&state.EdgeWildcard, &state.EdgeApex, &state.Mail))
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
//
// The label's lock comes first and the zone's second, here and in every writer
// of the zone's state, so two of them never wait on each other, and the state
// is read under both, so a toggle committing meanwhile is read and not
// written back over.
func EnsureNetworkZoneInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, ttl *dnsservice.DnsTtlSettings) (*DnsWriteResult, error) {
	return ensureNetworkZoneInTx(ctx, tx, networkId, root, ttl, nil)
}

// EnsureNetworkZoneInTx with the direct MX directMx names, when set, in place of
// the one the zone's direct rows hold: an empty list puts the router rows back
// (DNS-M7-03).
func ensureNetworkZoneInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, ttl *dnsservice.DnsTtlSettings, directMx *[]netip.Addr) (*DnsWriteResult, error) {
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
	lockDnsZoneInTx(ctx, tx, root, networkId)
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
	state := getDnsZoneStateInTx(ctx, tx, root, networkId)
	state.Root = root
	state.Apex = label + "." + root
	switch {
	case directMx == nil:
		state.DirectMx = getDnsDirectMxInTx(ctx, tx, root, networkId, state.Apex)
	case 0 < len(*directMx):
		state.DirectMx = *directMx
	}
	state.MailPolicy = getDnsZoneMailPolicyInTx(ctx, tx, root, networkId)
	desired, err := DnsSystemRecords(state, ttl)
	if err != nil {
		return nil, err
	}
	result, err := reconcileDnsSystemRowsInTx(ctx, tx, root, networkId, dnsZoneSystemKeys(state.Apex), desired)
	if err != nil {
		return nil, err
	}
	if result != nil && 0 < result.Serial {
		touchDnsLabelMapInTx(ctx, tx, root, networkId, result.Serial)
	}
	return result, nil
}

// The bytes of a report token: 128 random bits (plan-DNS-rua-token).
const dnsReportTokenByteCount = 16

// A new report token: 16 bytes from crypto/rand in lowercase base32 with no
// padding, 26 characters (plan-DNS-rua-token), which no local part a root's
// _dmarc uses can equal.
func newDnsReportToken() (string, error) {
	tokenBytes := make([]byte, dnsReportTokenByteCount)
	if _, err := io.ReadFull(rand.Reader, tokenBytes); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(tokenBytes)), nil
}

// The zone's report token, minted in the caller's transaction where the zone
// has none (plan-DNS-rua-token: written with the zone's set and fixed for the
// network's life), and the report host and MTA-STS policy dns.yml gives
// (DNS-M8-01). The zone's row must exist and its lock be held. A binary that
// does not link MG6, where the column lives, has no token, so its zones
// publish no rua.
func getDnsZoneMailPolicyInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id) *DnsZoneMailPolicy {
	policy := &DnsZoneMailPolicy{MtaSts: sidecar.DefaultMtaStsSettings()}
	if records := dnsZoneSettings().RootRecords[root]; records != nil && records.ReportMx != "" {
		policy.ReportHost = strings.TrimSuffix(dns.CanonicalName(records.ReportMx), ".")
	}
	if !server.SidecarMg6Linked() {
		return policy
	}
	var token *string
	result, err := tx.Query(ctx, `SELECT report_token FROM network_dns_zone WHERE root = $1 AND network_id = $2`, root, networkId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&token))
		}
	})
	if token != nil {
		policy.ReportToken = *token
		return policy
	}
	minted, err := newDnsReportToken()
	server.Raise(err)
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE network_dns_zone SET report_token = $3 WHERE root = $1 AND network_id = $2 AND report_token IS NULL`,
		root,
		networkId,
		minted,
	))
	if tag.RowsAffected() == 1 {
		policy.ReportToken = minted
	}
	return policy
}

// The network whose zone holds token as its report token, the key a report's
// rua names (plan-DNS-rua-token, plan-SMTP-net-token), and whether one does.
// None in a binary that does not link MG6.
func GetDnsNetworkForReportToken(ctx context.Context, token string) (networkId server.Id, ok bool) {
	if !server.SidecarMg5Linked() || !server.SidecarMg6Linked() || token == "" {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT network_id FROM network_dns_zone WHERE report_token = $1`, token)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
				ok = true
			}
		})
	})
	return
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
	// the label's lock, then the zone's, as EnsureNetworkZoneInTx takes them
	if _, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root); err != nil {
		return nil, err
	}
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
