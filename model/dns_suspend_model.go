package model

// §12.9's abuse hooks (DNS-M5-14; R-DNS-5, I-14; RESOLUTIONS
// plan-DNS-metadata-rows #5): SuspendZone, SuspendHost and Restore withhold a
// network's records, MX included, and return them, each within one write plus
// the TTL. The rows stay: a withheld row leaves the journal as a delete and
// every snapshot, and Restore puts back exactly what was there.
//
//   - A zone suspension is network_dns_zone's reason and until, and a withheld
//     owner at each of the zone's apexes, its label and each live alias, so a
//     suspended zone answers NODATA at its apex and NXDOMAIN below it, its
//     wildcard included. Until passing lifts it at the next taskworker pass
//     (LiftExpiredDnsSuspensions); writes to the zone are refused meanwhile.
//   - A host suspension is a withheld owner at <host>.<apex> for each apex,
//     whose own rows and every row below it are withheld, so * no longer covers
//     it and it answers NODATA for every type. Until
//     network_dns_host_suspension has §2.8's v9 row the withheld owner's row in
//     network_dns_record is the suspension itself.
//   - tlsedge and mxedge read the same state through the label map
//     (DnsLabelMapSince): each change moves the network's names' change serial.
//   - While a root is restoring, every hook sets only its rows and takes no
//     serial, so §19.10's abuse step lands in the snapshot the edges reload
//     (§12.6). Its map change takes the serial after the restored one, which no
//     write can hold before the restore's jump.
//
// Every hook is idempotent by network and host: a second call finds nothing to
// change and writes nothing. Each runs in a transaction of its own, holds no
// transaction across Redis, and needs MG5 (and MG2 for the apexes).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

var (
	// A host SuspendHost or Restore cannot name: empty, @, a wildcard, or no name.
	ErrDnsHostInvalid = errors.New("dns host invalid")
	// A zone suspension's reason that is empty or past its column.
	ErrDnsSuspendReason = errors.New("dns suspension reason invalid")
)

// The longest reason a zone suspension keeps: network_dns_zone's column.
const dnsSuspendReasonMaxLength = 64

// What a hook changes: the zone's suspension and the hosts it withholds.
type dnsSuspension struct {
	zoneSuspended bool
	reason        string
	until         *time.Time
	// relative host names, as SuspendHost takes them
	hosts map[string]bool
}

// Withholds every record of the network's zones until Restore or until (§12.9).
// reason is an opaque word for staff, never content; until nil holds until a
// lift. A zone already suspended takes the new reason and until.
func SuspendZone(ctx context.Context, networkId server.Id, reason string, until *time.Time) error {
	if reason == "" || dnsSuspendReasonMaxLength < len(reason) {
		return fmt.Errorf("%w: %d bytes", ErrDnsSuspendReason, len(reason))
	}
	return changeDnsSuspension(ctx, networkId, func(suspension *dnsSuspension) {
		suspension.zoneSuspended = true
		suspension.reason = reason
		suspension.until = until
	})
}

// Withholds host and everything below it in each of the network's zones, so
// it answers NODATA for every type and * no longer covers it (§12.9). host is
// relative to the zone's apex, as shop or a.shop.
func SuspendHost(ctx context.Context, networkId server.Id, host string) error {
	host, err := dnsSuspendHost(host)
	if err != nil {
		return err
	}
	return changeDnsSuspension(ctx, networkId, func(suspension *dnsSuspension) {
		suspension.hosts[host] = true
	})
}

// Returns what a suspension withheld: the zone's for host "", else that host's.
// A zone still suspended keeps a lifted host's rows withheld until it lifts too.
func Restore(ctx context.Context, networkId server.Id, host string) error {
	if host == "" {
		return changeDnsSuspension(ctx, networkId, func(suspension *dnsSuspension) {
			suspension.zoneSuspended = false
			suspension.reason = ""
			suspension.until = nil
		})
	}
	host, err := dnsSuspendHost(host)
	if err != nil {
		return err
	}
	return changeDnsSuspension(ctx, networkId, func(suspension *dnsSuspension) {
		delete(suspension.hosts, host)
	})
}

// Lifts every zone suspension whose until has passed at now. Returns how many
// lifted.
func LiftExpiredDnsSuspensions(ctx context.Context, now time.Time) (int, error) {
	if !server.SidecarMg5Linked() {
		return 0, nil
	}
	networkIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT DISTINCT network_id FROM network_dns_zone WHERE suspend_until <= $1`,
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	errs := []error{}
	for _, networkId := range networkIds {
		if err := Restore(ctx, networkId, ""); err != nil {
			errs = append(errs, err)
		}
	}
	return len(networkIds) - len(errs), errors.Join(errs...)
}

// host as a suspension keys it: lowercase, no final dot, a name below an apex.
func dnsSuspendHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || host == "@" || strings.Contains(host, "*") {
		return "", fmt.Errorf("%w: %q", ErrDnsHostInvalid, host)
	}
	if _, ok := dns.IsDomainName(host); !ok {
		return "", fmt.Errorf("%w: %q", ErrDnsHostInvalid, host)
	}
	return host, nil
}

// Runs change on each of the network's zones, each in a transaction of its own.
func changeDnsSuspension(ctx context.Context, networkId server.Id, change func(suspension *dnsSuspension)) error {
	if !server.SidecarMg5Linked() {
		return ErrDnsUnlinked
	}
	var roots []string
	server.Db(ctx, func(conn server.PgConn) {
		roots = getNetworkDnsRootsInTx(ctx, conn, networkId)
	})
	slices.Sort(roots)
	for _, root := range roots {
		var returnErr error
		err := dnsCatchRefusal(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				returnErr = changeDnsSuspensionInTx(ctx, tx, root, networkId, change)
				if returnErr != nil {
					panic(dnsWriteRefusal{err: returnErr})
				}
			}, server.TxReadCommitted)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// One zone's change, in its own transaction: the suspension it asks for, as
// the visible rows it leaves, diffed against what edges hold now.
func changeDnsSuspensionInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id, change func(suspension *dnsSuspension)) error {
	now := server.NowUtc()
	lockDnsZoneInTx(ctx, tx, root, networkId)
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_dns_zone (root, network_id, create_time) VALUES ($1, $2, $3)
			ON CONFLICT (root, network_id) DO NOTHING
		`,
		root,
		networkId,
		now,
	))
	suspension := &dnsSuspension{hosts: map[string]bool{}}
	var reason *string
	result, err := tx.Query(
		ctx,
		`SELECT suspend_reason, suspend_until FROM network_dns_zone WHERE root = $1 AND network_id = $2 FOR UPDATE`,
		root,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&reason, &suspension.until))
		}
	})
	if reason != nil {
		suspension.zoneSuspended = true
		suspension.reason = *reason
	}
	apexes := getDnsZoneApexesInTx(ctx, tx, root, networkId)
	stored := getDnsZoneRowsInTx(ctx, tx, root, networkId)
	for _, row := range stored {
		if row.set.Kind != dnsservice.DnsRecordKindWithheld || slices.Contains(apexes, row.set.Owner) {
			continue
		}
		for _, apex := range apexes {
			if host, found := strings.CutSuffix(row.set.Owner, "."+apex); found {
				suspension.hosts[host] = true
			}
		}
	}

	change(suspension)

	// the withheld owners the suspension asks for: one at each apex while the
	// zone is suspended, and one at each host under each apex
	markers := map[string]bool{}
	hostMarkers := []string{}
	for _, apex := range apexes {
		if suspension.zoneSuspended {
			markers[apex] = true
		}
		for host := range suspension.hosts {
			owner := host + "." + apex
			markers[owner] = true
			hostMarkers = append(hostMarkers, owner)
		}
	}
	visible := func(set *dnsservice.DnsRecordSet) bool {
		if suspension.zoneSuspended {
			return set.Kind == dnsservice.DnsRecordKindWithheld && slices.Contains(apexes, set.Owner)
		}
		if set.Kind == dnsservice.DnsRecordKindWithheld {
			return true
		}
		return !slices.ContainsFunc(hostMarkers, func(marker string) bool { return dnsOwnerAtOrBelow(set.Owner, marker) })
	}

	upserts := []dnsservice.DnsRecordSet{}
	deletes := []dnsservice.DnsRecordKey{}
	withholds := []dnsservice.DnsRecordKey{}
	// rows that change with no journal row: never seen by an edge, before or after
	quietInserts := []dnsservice.DnsRecordSet{}
	quietDeletes := []dnsservice.DnsRecordKey{}
	held := map[string]bool{}
	for _, row := range stored {
		set := row.set
		if set.Kind == dnsservice.DnsRecordKindWithheld {
			held[set.Owner] = true
			if !markers[set.Owner] {
				if row.withheld {
					quietDeletes = append(quietDeletes, set.Key())
				} else {
					deletes = append(deletes, set.Key())
				}
				continue
			}
		}
		switch {
		case !row.withheld && !visible(&set):
			withholds = append(withholds, set.Key())
		case row.withheld && visible(&set):
			upserts = append(upserts, set)
		}
	}
	markerOwners := []string{}
	for owner := range markers {
		markerOwners = append(markerOwners, owner)
	}
	slices.Sort(markerOwners)
	for _, owner := range markerOwners {
		if held[owner] {
			continue
		}
		set := dnsservice.DnsRecordSet{Owner: owner, Type: dnsservice.DnsWithheldType, Kind: dnsservice.DnsRecordKindWithheld}
		if visible(&set) {
			upserts = append(upserts, set)
		} else {
			quietInserts = append(quietInserts, set)
		}
	}

	var restoring bool
	var serialColumn int64
	server.Raise(tx.QueryRow(ctx, `SELECT restoring, serial FROM dns_root WHERE root = $1`, root).Scan(&restoring, &serialColumn))
	var mapSerial uint64
	journaled := 0 < len(upserts)+len(deletes)+len(withholds)
	switch {
	case restoring:
		// rows only, so the snapshot the edges reload carries it
		quietInserts = append(quietInserts, upserts...)
		quietDeletes = append(quietDeletes, deletes...)
		for _, key := range withholds {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_dns_record SET withheld_time = $4 WHERE root = $1 AND owner = $2 AND rrtype = $3`,
				root, key.Owner, int32(key.Type), now,
			))
		}
		if journaled {
			mapSerial = dnsSerialFromColumn(serialColumn) + 1
		}
	case journaled:
		serial, err := dnsCommitInTx(ctx, tx, &dnsservice.DnsZoneWrite{
			Root:      root,
			NetworkId: dnsSidecarId(networkId),
			Upserts:   upserts,
			Deletes:   deletes,
		}, now, withholds)
		if err != nil {
			return err
		}
		mapSerial = serial
		server.AddTxPostCommit(tx, "dns_write:"+root, func() any {
			DnsWritePost(context.Background(), root, serial, networkId)
			return nil
		})
	}
	for _, set := range quietInserts {
		withheldTime := &now
		if restoring && visible(&set) {
			withheldTime = nil
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_dns_record (root, owner, rrtype, network_id, kind, ttl, rdata, create_time, withheld_time)
				VALUES ($1, $2, $3, $4, $5, 0, '{}', $6, $7)
				ON CONFLICT (root, owner, rrtype) DO UPDATE SET withheld_time = EXCLUDED.withheld_time
			`,
			root, set.Owner, int32(set.Type), networkId, string(set.Kind), now, withheldTime,
		))
	}
	for _, key := range quietDeletes {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_dns_record WHERE root = $1 AND owner = $2 AND rrtype = $3`,
			root, key.Owner, int32(key.Type),
		))
	}
	var reasonColumn *string
	if suspension.zoneSuspended {
		reasonColumn = &suspension.reason
	} else {
		suspension.until = nil
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE network_dns_zone SET suspend_reason = $3, suspend_until = $4 WHERE root = $1 AND network_id = $2`,
		root,
		networkId,
		reasonColumn,
		suspension.until,
	))
	if 0 < mapSerial {
		touchDnsLabelMapInTx(ctx, tx, root, networkId, mapSerial)
	}
	return nil
}
