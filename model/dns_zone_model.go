package model

// A network's zone in a root (DNS-M5-11; plan reading 2): network_dns_zone's
// row, its label, and §12.7's system set written from the row's state. A zone
// comes to be with its label, on the label's first need (§12.5): a read, a
// first publish or order, or /mail/enable. Its first write is the one that
// places the label in the label map's serial order (dns_label_model.go). Every
// change of its state recomputes the whole set (dns_system_records_model.go)
// and writes one journal row when it changed and none otherwise.
//
// One input of the set is not the zone's own: _mta-sts follows the network's
// verification (owner decision 630), which changes with no write to the zone,
// so a verification refresh reconciles the network's zones whose _mta-sts it
// would change (ReconcileNetworkZoneMailPolicy).

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"io"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

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
	// this write is the one that places a new label in the root's serial order
	label, err := ensureNetworkLabelForZoneInTx(ctx, tx, networkId, root)
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
//
// The MTA-STS policy is the network's only while it is verified (§12.7, §15.5;
// owner decision 630, QD-1092): network_verified is read here, in the caller's
// transaction, from the source of truth, never the cache, so an unverified
// network's zone publishes no _mta-sts, whose mode: enforce policy no receiver
// may serve for it (§13.9's ValidateListen), and DnsSystemRecords publishes it
// only with mail on besides. A Team scope's set takes no MTA-STS policy from
// it, verified or not (DnsScopeRecords; QD-1206). A verification
// change reaches a zone written earlier through
// ReconcileNetworkZoneMailPolicy. The receivers' certificates are no condition
// yet: that waits for the review phase (DESIGN-DEFER.md's
// dns-mta-sts-certificate-gate).
func getDnsZoneMailPolicyInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id) *DnsZoneMailPolicy {
	policy := &DnsZoneMailPolicy{}
	if isNetworkVerifiedWithQuery(ctx, tx, networkId, server.NowUtc()) {
		policy.MtaSts = sidecar.DefaultMtaStsSettings()
	}
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
// Its callers are api's sync after each accepted config entry and the hourly
// pass over the published names (dns_edge_grants_model.go).
func DnsSyncEdgeRecordsInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, wildcard bool, apex bool, ttl *dnsservice.DnsTtlSettings) (*DnsWriteResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	// the label's lock, then the zone's, as EnsureNetworkZoneInTx takes them; its write places a new label
	if _, err := ensureNetworkLabelForZoneInTx(ctx, tx, networkId, root); err != nil {
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

// Reconciles the network's zones whose _mta-sts no longer follows its
// verification (§12.7, §15.5; owner decision 630, QD-1092): the half of the
// gate a verification change needs, since getDnsZoneMailPolicyInTx reads the
// verification only when a zone's set is written. UpdateNetworkVerified runs
// it after each refresh, wherever a balance lands or ends or a sign-in
// verifies, so verifying a network with mail on publishes the record at that
// refresh and losing verification takes it away.
//
//   - Only an unsuspended zone with mail on, or one still holding an _mta-sts
//     row, its apex's or a Team scope's, is looked at: one indexed read for a
//     network with none.
//   - Each such zone is checked first, under its lock held shared, which waits
//     out a writer in flight: a reconcile that read the verification before it
//     changed commits before the check reads the rows, so the check sees what
//     that reconcile wrote. Checks never wait on one another.
//   - Only a zone whose _mta-sts would change is written, in a transaction of
//     its own, the label's lock first as every zone writer takes them: its own
//     set (EnsureNetworkZoneInTx) and each Team scope's that still holds an
//     _mta-sts, which a scope's set no longer publishes (QD-1206), so the whole
//     set is recomputed, never the one row patched.
//   - A suspended zone is left: its writes are refused until Restore, which
//     returns what it withheld, and the network's next refresh or write
//     reconciles it. A restoring root waits likewise.
//
// Nothing here fails its caller, a sign-in or a payment: a refusal or a failure
// is logged, and the next refresh checks again. A verification that changes
// with no writer, a paid balance's window ending, reaches the zone at the
// network's next refresh or write.
func ReconcileNetworkZoneMailPolicy(ctx context.Context, networkId server.Id) {
	ReconcileNetworkZoneMailPolicyInConn(nil, ctx, networkId)
}

// ReconcileNetworkZoneMailPolicy on the caller's Postgres session, which must
// hold no transaction; nil selects the outer acquisition boundary. A zone
// write's post (DnsWritePost) still reads the network's sidecars on a pooled
// connection after its commit.
func ReconcileNetworkZoneMailPolicyInConn(connOwner server.PgConn, ctx context.Context, networkId server.Id) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return
	}
	server.HandleError(func() {
		var roots []string
		server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
			roots = getDnsMailPolicyZoneRootsInTx(ctx, conn, networkId)
		})
		for _, root := range roots {
			stale := false
			server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
				stale = isDnsZoneMailPolicyStaleInTx(ctx, tx, root, networkId, server.NowUtc())
			}, server.TxReadCommitted, server.TxLockTimeout(server.PgLockTimeout))
			if !stale {
				continue
			}
			var returnErr error
			err := dnsCatchRefusal(func() {
				server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
					returnErr = reconcileDnsZoneMailPolicyInTx(ctx, tx, networkId, root)
					if returnErr != nil {
						panic(dnsWriteRefusal{err: returnErr})
					}
				}, server.TxReadCommitted, server.TxLockTimeout(server.PgLockTimeout))
			})
			switch {
			case err == nil:
			case IsDnsZoneWaiting(err) || errors.Is(err, ErrDnsHostSuspended):
				if glog.V(1) {
					glog.Infof("[dns]network %s's _mta-sts in %s waits for its zone: %v\n", networkId, root, err)
				}
			default:
				glog.Warningf("[dns]network %s's _mta-sts in %s is not reconciled, its next refresh tries again: %v\n", networkId, root, err)
			}
		}
	})
}

// The roots of the network's unsuspended zones whose _mta-sts can follow its
// verification: mail on, or an _mta-sts row still held.
func getDnsMailPolicyZoneRootsInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id) []string {
	roots := []string{}
	result, err := query.Query(
		ctx,
		`
			SELECT network_dns_zone.root
			FROM network_dns_zone
			WHERE
				network_dns_zone.network_id = $1 AND
				network_dns_zone.suspend_reason IS NULL AND (
					network_dns_zone.mail OR
					EXISTS (
						SELECT 1
						FROM network_dns_record
						WHERE
							network_dns_record.network_id = network_dns_zone.network_id AND
							network_dns_record.root = network_dns_zone.root AND
							network_dns_record.rrtype = $2 AND
							starts_with(network_dns_record.owner, $3)
					)
				)
			ORDER BY network_dns_zone.root
		`,
		networkId,
		int32(dns.TypeTXT),
		dnsMtaStsLabel+".",
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var root string
			server.Raise(result.Scan(&root))
			roots = append(roots, root)
		}
	})
	return roots
}

// Takes the zone's lock shared to the end of the transaction: the key
// lockDnsZoneInTx takes, so it waits out a writer holding that, and never
// another shared holder.
func lockDnsZoneSharedInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock_shared(hashtextextended('dns_zone:' || $1 || ':' || $2::uuid::text, 0))`,
		root,
		networkId,
	))
}

// The Team scopes whose sets the zone's rows hold: each label directly below
// apex whose owner holds a system MX, which every scope's set writes
// (dnsScopeWritten), but b, the envelope domain, whose MX is the zone's own.
func dnsZoneWrittenScopes(apex string, rows []*dnsStoredRow) []string {
	scopes := []string{}
	for _, row := range rows {
		if row.set.Type != dns.TypeMX || row.set.Kind != dnsservice.DnsRecordKindSystem {
			continue
		}
		scope, found := strings.CutSuffix(row.set.Owner, "."+apex)
		if found && scope != dnsMailEnvelopeLabel && isDnsCtLabel(scope) {
			scopes = append(scopes, scope)
		}
	}
	slices.Sort(scopes)
	return slices.Compact(scopes)
}

// The row rows hold at owner of rrType, nil for none.
func dnsZoneRowAt(rows []*dnsStoredRow, owner string, rrType uint16) *dnsStoredRow {
	for _, row := range rows {
		if row.set.Owner == owner && row.set.Type == rrType {
			return row
		}
	}
	return nil
}

// Whether a reconcile would change the _mta-sts TXT rows hold at owner, where
// publishes says whether the set there publishes one. A user row at the owner
// is the record API's, which no reconcile writes over, and an owner at or below
// a withheld one takes no write until its suspension lifts (§12.9), so neither
// counts.
func isDnsMtaStsRowStale(rows []*dnsStoredRow, owner string, publishes bool) bool {
	withheld := slices.ContainsFunc(rows, func(row *dnsStoredRow) bool {
		return row.set.Kind == dnsservice.DnsRecordKindWithheld && dnsOwnerAtOrBelow(owner, row.set.Owner)
	})
	row := dnsZoneRowAt(rows, owner, dns.TypeTXT)
	switch {
	case withheld:
		return false
	case row != nil && row.set.Kind == dnsservice.DnsRecordKindUser:
		return false
	default:
		return (row != nil) != publishes
	}
}

// Whether a reconcile would change the zone's _mta-sts rows
// (isDnsMtaStsRowStale): one at its apex while mail is on and the network is
// verified at now, and none elsewhere, so none at any Team scope's apex its
// rows hold, whose set publishes none (DnsScopeRecords; QD-1206) though its
// rows may still hold one from before. It takes the zone's lock shared first,
// so a writer in flight commits before the rows are read. A zone with no row
// or no label has nothing to change.
func isDnsZoneMailPolicyStaleInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id, now time.Time) bool {
	lockDnsZoneSharedInTx(ctx, tx, root, networkId)
	state := getDnsZoneStateInTx(ctx, tx, root, networkId)
	if state == nil {
		return false
	}
	label := getNetworkLabelInTx(ctx, tx, networkId, root)
	if label == "" {
		return false
	}
	apex := label + "." + root
	stored := getDnsZoneRowsInTx(ctx, tx, root, networkId)
	publishes := state.Mail && isNetworkVerifiedWithQuery(ctx, tx, networkId, now)
	if isDnsMtaStsRowStale(stored, dnsMtaStsLabel+"."+apex, publishes) {
		return true
	}
	for _, scope := range dnsZoneWrittenScopes(apex, stored) {
		if isDnsMtaStsRowStale(stored, dnsMtaStsLabel+"."+dnsservice.DnsScopeApex(apex, scope), false) {
			return true
		}
	}
	return false
}

// The zone's set and each Team scope's whose _mta-sts is stale, recomputed
// with the network's mail policy as it stands, in the caller's transaction,
// which must be read committed: what ReconcileNetworkZoneMailPolicy writes for
// a zone whose _mta-sts is stale. A scope's set publishes no _mta-sts
// (QD-1206), so only a scope whose rows still hold one is written, which takes
// it away. A scope's state is read back from its rows, as
// DnsSetScopeRecordsInTx reads it, so only what the policy changes moves, one
// journal row a set changed.
func reconcileDnsZoneMailPolicyInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string) error {
	if _, err := EnsureNetworkZoneInTx(ctx, tx, networkId, root, nil); err != nil {
		return err
	}
	// the label's lock and the zone's are held from here to the commit
	apex := getNetworkLabelInTx(ctx, tx, networkId, root) + "." + root
	stored := getDnsZoneRowsInTx(ctx, tx, root, networkId)
	var policy *DnsZoneMailPolicy
	for _, scope := range dnsZoneWrittenScopes(apex, stored) {
		scopeApex := dnsservice.DnsScopeApex(apex, scope)
		if !isDnsMtaStsRowStale(stored, dnsMtaStsLabel+"."+scopeApex, false) {
			continue
		}
		state := &DnsScopeState{Root: root, Apex: apex, Scope: scope}
		dnsScopeStateFromRows(state, stored)
		if policy == nil {
			policy = getDnsZoneMailPolicyInTx(ctx, tx, root, networkId)
		}
		state.MailPolicy = policy
		desired, err := DnsScopeRecords(state, dnsZoneTtl())
		if err != nil {
			return err
		}
		result, err := reconcileDnsSystemRowsInTx(ctx, tx, root, networkId, dnsZoneSystemKeys(scopeApex), desired)
		if err != nil {
			return err
		}
		if result != nil && 0 < result.Serial {
			touchDnsLabelMapInTx(ctx, tx, root, networkId, result.Serial)
		}
	}
	return nil
}
