package model

// §20.7's downgrade column, dns side (DNS-M6-07; D-QUOTA-6, FS-51, FI-12): the effects QUOTA's daily
// ApplyNetworkDowngrades (QUOTA-M6-05) calls once a network is 14 days past its downgrade and still over a
// limit, and again when a renewal or a raised limit lets the records back. QUOTA owns the clock; these are the
// effects, each in the caller's transaction, which must be read committed.
//
//   - dns_entries, "newest over quota withheld after 14 days": the network's user RRsets past the limit, newest
//     first by their first write (create_time, which a later upsert keeps), leave the journal as deletes and
//     stay stored (withheld_time), as a suspension's do (§12.9), so nothing is deleted. DnsWithholdOverQuotaInTx
//     and DnsReleaseWithheldInTx both converge the zone on one rule: the newest RRsets past the limit withheld,
//     the rest served, so a release goes oldest first and a pass run twice writes nothing more. Copy rows follow
//     what stays served (dnsservice.PlanCopyRows), so a withheld host's children keep the wildcard answering.
//   - A row a suspension withholds stays the suspension's: a zone suspended is left alone, and a row under a
//     suspended host is neither withheld nor released here. A suspension's changes keep a row withheld here, as
//     a user row withheld while the suspension left it visible (dns_suspend_model.go); a lift that shows such a
//     row again lets the next pass withhold it again.
//   - The record API keeps growth frozen: a withheld row still counts as an entry, so a write past the limit is
//     refused (§20.7), and a write of a withheld RRset serves it again until the next pass.
//   - Custom domains, "web hosts and mail From withheld; mail hosts keep MX, TLSA and routing" (owner, QD-840,
//     on question dns-m6-05-custom-host-withheld-state): DnsWithholdCustomHostsInTx sets each active TLS web host
//     of the network to network_dns_custom_host.state withheld, which takes it out of §13.4's map (its version
//     raised) and tlsedge's routing while its row, verification and CAA mode stay. A release after a renewal
//     (DnsReleaseWithheldInTx with its custom hosts) sets each back to active with map_version 0, so it re-enters
//     the map at the next version and is pending until every interface is current, as a new host is, with no
//     re-verification and no new CAA walk; certs' verdicts never turn a withheld host active
//     (DnsCustomHostResultInTx refuses one). A strict-mode MX host, MX, TLSA and the router rows are never
//     touched, so no inbound address breaks (D-QUOTA-6). A custom domain's mail From is smtpedge's From-domain
//     gate (SMTP), and from M9 its delegated _domainkey zones.
//
// Each change adds one admin event (§7.11), so the network's admins can see why records stopped answering.
// Nothing here runs unless the binary links MG2 and MG5.

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The admin event a downgrade's withholding adds: the zone is its target.
const (
	AdminEventActionDnsQuotaWithhold = "dns.quota_withhold"
	AdminEventResultDnsWithheld      = "withheld"
	AdminEventResultDnsReleased      = "released"
)

// network_dns_custom_host.state of a web host a downgrade withholds (QD-840): in no map and routed by no edge,
// its row kept for the release.
const DnsCustomHostStateWithheld = "withheld"

// The admin event's target for a custom host's withholding and release.
const AdminEventTargetDnsCustomHosts = "dns_custom_hosts"

// One user RRset of the zone as the withholding orders it.
type dnsQuotaRow struct {
	set        dnsservice.DnsRecordSet
	withheld   bool
	createTime time.Time
}

// The zone's user RRsets, oldest first by their first write, and every row it holds.
func getDnsQuotaRowsInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id) []*dnsQuotaRow {
	rows := []*dnsQuotaRow{}
	result, err := tx.Query(
		ctx,
		`
			SELECT owner, rrtype, kind, ttl, rdata, withheld_time IS NOT NULL, create_time
			FROM network_dns_record
			WHERE network_id = $1 AND root = $2 AND kind = $3
			ORDER BY create_time, owner, rrtype
		`,
		networkId,
		root,
		string(dnsservice.DnsRecordKindUser),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			row := &dnsQuotaRow{}
			var rrType int32
			var kind string
			var ttl int64
			var rdata []string
			server.Raise(result.Scan(&row.set.Owner, &rrType, &kind, &ttl, &rdata, &row.withheld, &row.createTime))
			row.set.Type = uint16(rrType)
			row.set.Kind = dnsservice.DnsRecordKind(kind)
			row.set.Ttl = uint32(ttl)
			if 0 < len(rdata) {
				row.set.Rdata = rdata
			}
			rows = append(rows, row)
		}
	})
	return rows
}

// Converges the zone's user RRsets on limit (see the file header): the newest past limit withheld, the rest
// served, -1 serving all. One journal write, none when nothing changes. Returns the write, nil when nothing
// changed.
func dnsApplyQuotaWithholdingInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, limit int64) (*DnsWriteResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	lockDnsZoneInTx(ctx, tx, root, networkId)
	if getDnsZoneSuspendedInTx(ctx, tx, root, networkId) {
		// every row is the suspension's until Restore; the next pass after it decides
		return nil, nil
	}
	apexes := getDnsZoneApexesInTx(ctx, tx, root, networkId)
	if len(apexes) == 0 {
		return nil, nil
	}
	stored := getDnsZoneRowsInTx(ctx, tx, root, networkId)
	hostMarkers := []string{}
	for _, row := range stored {
		if row.set.Kind == dnsservice.DnsRecordKindWithheld && !slices.Contains(apexes, row.set.Owner) {
			hostMarkers = append(hostMarkers, row.set.Owner)
		}
	}
	suspendedHost := func(owner string) bool {
		return slices.ContainsFunc(hostMarkers, func(marker string) bool { return dnsOwnerAtOrBelow(owner, marker) })
	}
	userRows := getDnsQuotaRowsInTx(ctx, tx, root, networkId)
	keep := len(userRows)
	if 0 <= limit && limit < int64(len(userRows)) {
		keep = int(limit)
	}
	upserts := []dnsservice.DnsRecordSet{}
	withholds := []dnsservice.DnsRecordKey{}
	willWithhold := map[dnsservice.DnsRecordKey]bool{}
	willRelease := map[dnsservice.DnsRecordKey]bool{}
	for index, row := range userRows {
		if suspendedHost(row.set.Owner) {
			continue
		}
		served := index < keep
		switch {
		case served && row.withheld:
			upserts = append(upserts, row.set)
			willRelease[row.set.Key()] = true
		case !served && !row.withheld:
			withholds = append(withholds, row.set.Key())
			willWithhold[row.set.Key()] = true
		}
	}
	if len(upserts) == 0 && len(withholds) == 0 {
		return nil, nil
	}
	// what edges serve after the change, for the copy rows it needs
	served := []dnsservice.DnsRecordSet{}
	for _, row := range stored {
		key := row.set.Key()
		if (row.withheld && !willRelease[key]) || willWithhold[key] {
			continue
		}
		served = append(served, row.set)
	}
	deletes := []dnsservice.DnsRecordKey{}
	held := map[dnsservice.DnsRecordKey]bool{}
	for _, row := range stored {
		held[row.set.Key()] = true
	}
	for _, apex := range apexes {
		copyUpserts, copyDeletes := dnsservice.PlanCopyRows(apex, served)
		upserts = append(upserts, copyUpserts...)
		for _, key := range copyDeletes {
			if held[key] {
				deletes = append(deletes, key)
			}
		}
	}
	serial, err := dnsCommitInTx(ctx, tx, &dnsservice.DnsZoneWrite{
		Root:      root,
		NetworkId: dnsSidecarId(networkId),
		Upserts:   upserts,
		Deletes:   deletes,
	}, server.NowUtc(), withholds)
	if err != nil {
		return nil, err
	}
	result := AdminEventResultDnsReleased
	if 0 < len(withholds) {
		result = AdminEventResultDnsWithheld
	}
	targetId := networkId
	AddAdminEventInTx(ctx, tx, &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  AdminEventActorSystem,
		Action:     AdminEventActionDnsQuotaWithhold,
		TargetKind: AdminEventTargetDnsZone,
		TargetId:   &targetId,
		Result:     result,
	})
	server.AddTxPostCommit(tx, "dns_write:"+root, func() any {
		DnsWritePost(context.Background(), root, serial, networkId)
		return nil
	})
	touchDnsLabelMapInTx(ctx, tx, root, networkId, serial)
	return &DnsWriteResult{Serial: serial, Upserts: upserts, Deletes: slices.Concat(deletes, withholds), PostRegistered: true}, nil
}

// Withholds the network's newest user RRsets in root past limit, through the journal, their rows kept, and
// serves again any older one a smaller limit withheld (see the file header). limit is the network's dns_entries
// as the downgrade left it. Returns the write, nil when nothing changed.
func DnsWithholdOverQuotaInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, limit int64) (*DnsWriteResult, error) {
	if limit < 0 {
		return nil, fmt.Errorf("dns withholding: a limit of %d withholds nothing", limit)
	}
	return dnsApplyQuotaWithholdingInTx(ctx, tx, networkId, root, limit)
}

// Serves again the network's user RRsets in root the downgrade withheld, oldest first, while the served count
// stays within limit, -1 for no limit, as a renewal or a raised limit allows, and with customHosts its withheld
// custom web hosts, as a Team renewal allows (see the file header). Returns the zone's write, nil when no RRset
// changed.
func DnsReleaseWithheldInTx(ctx context.Context, tx server.PgTx, networkId server.Id, root string, limit int64, customHosts bool) (*DnsWriteResult, error) {
	result, err := dnsApplyQuotaWithholdingInTx(ctx, tx, networkId, root, limit)
	if err != nil {
		return nil, err
	}
	if customHosts {
		if _, err := dnsChangeCustomHostStatesInTx(ctx, tx, networkId, DnsCustomHostStateWithheld, sidecar.DnsCustomHostStateActive); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Withholds the network's custom web hosts after the downgrade's 14 days (§20.7; QD-840): each active TLS host
// goes withheld, leaving the map and tlsedge's routing with its row kept; MX, TLSA and routing for mail stay, a
// strict-mode MX host among them. Answers how many went withheld, none when none was active.
func DnsWithholdCustomHostsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) (int, error) {
	return dnsChangeCustomHostStatesInTx(ctx, tx, networkId, sidecar.DnsCustomHostStateActive, DnsCustomHostStateWithheld)
}

// Moves the network's TLS custom hosts in state from to state to, a host going active again with map_version 0 so
// it re-enters the map at the next version, raises each root's map whose active rows changed, then adds one admin
// event. Answers how many moved.
func dnsChangeCustomHostStatesInTx(ctx context.Context, tx server.PgTx, networkId server.Id, from string, to string) (int, error) {
	if !dnsCustomHostLinked() {
		return 0, ErrDnsUnlinked
	}
	movedCount := 0
	roots := []string{}
	result, err := tx.Query(
		ctx,
		`
			UPDATE network_dns_custom_host SET state = $3, map_version = 0, ready_time = NULL
			WHERE network_id = $1 AND state = $2 AND owner NOT LIKE $4
			RETURNING root
		`,
		networkId,
		from,
		to,
		dnsMailHostLabel+".%",
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var root string
			server.Raise(result.Scan(&root))
			movedCount += 1
			if !slices.Contains(roots, root) {
				roots = append(roots, root)
			}
		}
	})
	if movedCount == 0 {
		return 0, nil
	}
	// the map's raise before the admin event's chain lock, as every custom-host change takes them
	slices.Sort(roots)
	for _, root := range roots {
		if _, err := DnsRaiseCustomHostMapInTx(ctx, tx, root); err != nil {
			return 0, err
		}
	}
	eventResult := AdminEventResultDnsWithheld
	if to == sidecar.DnsCustomHostStateActive {
		eventResult = AdminEventResultDnsReleased
	}
	targetId := networkId
	AddAdminEventInTx(ctx, tx, &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  AdminEventActorSystem,
		Action:     AdminEventActionDnsQuotaWithhold,
		TargetKind: AdminEventTargetDnsCustomHosts,
		TargetId:   &targetId,
		Result:     eventResult,
	})
	return movedCount, nil
}
