package model

// §13.4's custom-host map, server side (DNS-M6-03; D-TLS-12, D-DNS-9, FS-35, FS-122, FI-11): the map's versions,
// each lb interface's report and route probes, and each custom host's readiness, behind dnsservice's routes
// (api_map.go), which dnshost.CustomHostMaps serves on these functions.
//
//   - Each change to the active rows raises the map's version in the change's own transaction
//     (DnsRaiseCustomHostMapInTx): dns_root.custom_host_map_version + 1 (plan reading 9), a dns_custom_host_map
//     row with the digest of the version's sorted host list (dnsservice.DnsCustomHostMapDigest), and each host
//     entering the map keeps that version as its map_version with no ready_time. A change that leaves the host
//     list as it was raises nothing. The raise takes dns_root's row last in its transaction, after any zone
//     write and before any admin event, the order §12.6's write path takes them in, so the two never wait on
//     each other.
//   - The newest version answers lbmap's poll with the active hosts; a list that no longer matches its digest
//     (a change that raised nothing) is raised again on the spot, so lbmap never loads hosts under another
//     version's digest.
//   - dns_custom_host_map_applied holds one row an lb inventory interface, which the dns block keeps to the
//     inventory: its report, polls and route probes, applied by dnsservice's pure functions under the row's
//     lock. host_probes holds the reported version's added hosts with the latest probe of each, and the hosts it
//     dropped, which a restore lists (§2.8 dns-1), as JSON.
//   - The judgments (current, ready, map-stale) are dnsservice.DnsMapSettings', read from dns.yml as the block
//     reads them: the block sets ready_time from them after each report and probe round, and GET /network/dns
//     names a pending host's interfaces from them (DnsCustomHostReadiness).
//   - Versions purge at 7 days, the newest always kept (PurgeDnsCustomHostMapVersions); interface rows leave
//     with the inventory.
//
// The map is one for the deployment's one user root: dns_custom_host_map has no root column, and its counter
// is that root's. Nothing here runs unless the binary links MG2, MG5 and MG6.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// Tunables of the map's server half.
type DnsCustomHostMapModelSettings struct {
	// How long a version is kept: §12.14's 7 days. The newest is kept whatever its age.
	VersionRetention time.Duration
}

// §12.14's 7 days.
func DefaultDnsCustomHostMapModelSettings() *DnsCustomHostMapModelSettings {
	return &DnsCustomHostMapModelSettings{
		VersionRetention: 7 * 24 * time.Hour,
	}
}

// The active TLS hosts of root, sorted: the map's host list.
func getDnsCustomHostMapHostsInTx(ctx context.Context, query server.PgCanQuery, root string) []string {
	hosts := []string{}
	for _, row := range getDnsCustomHostRowsInTx(ctx, query, `root = $1 AND state = $2`, root, sidecar.DnsCustomHostStateActive) {
		if !row.StrictMx() {
			hosts = append(hosts, row.Host)
		}
	}
	slices.Sort(hosts)
	return hosts
}

// A version's digest as dns_custom_host_map holds it.
func dnsCustomHostMapDigestBytes(digest string) []byte {
	digestBytes, err := hex.DecodeString(digest)
	server.Raise(err)
	return digestBytes
}

// Raises root's map to version, with hosts' digest, in tx, which holds dns_root's row: the version row, the
// counter, and every active TLS host matching enter takes the version as its map_version with no ready_time.
func setDnsCustomHostMapVersionInTx(ctx context.Context, tx server.PgTx, root string, version uint64, hosts []string, enter string) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO dns_custom_host_map (version, digest, create_time) VALUES ($1, $2, $3)
			ON CONFLICT (version) DO UPDATE SET digest = EXCLUDED.digest, create_time = EXCLUDED.create_time
		`,
		dnsSerialColumn(version),
		dnsCustomHostMapDigestBytes(dnsservice.DnsCustomHostMapDigest(hosts)),
		server.NowUtc(),
	))
	server.RaisePgResult(tx.Exec(ctx, `UPDATE dns_root SET custom_host_map_version = $2 WHERE root = $1`, root, dnsSerialColumn(version)))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE network_dns_custom_host SET map_version = $2, ready_time = NULL
			WHERE root = $1 AND state = $3 AND owner NOT LIKE $4 AND `+enter,
		root,
		dnsSerialColumn(version),
		sidecar.DnsCustomHostStateActive,
		dnsMailHostLabel+".%",
	))
}

// Raises root's custom-host map in the caller's transaction after a change to its active rows (see the file
// header), and answers the version that holds them: a new one, or the newest when the host list is unchanged.
// Call it after the transaction's zone writes and before its admin events. Zero in a binary that does not link
// what it reads.
func DnsRaiseCustomHostMapInTx(ctx context.Context, tx server.PgTx, root string) (uint64, error) {
	if !dnsCustomHostLinked() {
		return 0, nil
	}
	root = dnsLabelRoot(root)
	// a root the API block has not yet added is added here, as EnsureDnsRoot adds it, so no change to the
	// active rows fails for want of the counter's row
	server.RaisePgResult(tx.Exec(ctx, `INSERT INTO dns_root (root) VALUES ($1) ON CONFLICT (root) DO NOTHING`, root))
	var current int64
	found := false
	result, err := tx.Query(ctx, `SELECT custom_host_map_version FROM dns_root WHERE root = $1 FOR UPDATE`, root)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&current))
			found = true
		}
	})
	if !found {
		return 0, fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
	}
	hosts := getDnsCustomHostMapHostsInTx(ctx, tx, root)
	digest := dnsservice.DnsCustomHostMapDigest(hosts)
	if 0 < current {
		var newestDigest []byte
		result, err := tx.Query(ctx, `SELECT digest FROM dns_custom_host_map WHERE version = $1`, current)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&newestDigest))
			}
		})
		if hex.EncodeToString(newestDigest) == digest {
			return dnsSerialFromColumn(current), nil
		}
	}
	version := dnsSerialFromColumn(current) + 1
	setDnsCustomHostMapVersionInTx(ctx, tx, root, version, hosts, `map_version = 0`)
	return version, nil
}

// The newest version of root's map with its hosts, nil before any (see the file header).
func GetDnsCustomHostMap(ctx context.Context, root string) (*dnsservice.DnsCustomHostMap, error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	read := func() (*dnsservice.DnsCustomHostMap, bool) {
		var customHostMap *dnsservice.DnsCustomHostMap
		matched := true
		server.Tx(ctx, func(tx server.PgTx) {
			customHostMap = nil
			matched = true
			var version int64
			var digestBytes []byte
			result, err := tx.Query(
				ctx,
				`
					SELECT dns_root.custom_host_map_version, dns_custom_host_map.digest
					FROM dns_root
					LEFT JOIN dns_custom_host_map ON dns_custom_host_map.version = dns_root.custom_host_map_version
					WHERE dns_root.root = $1
				`,
				root,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&version, &digestBytes))
				}
			})
			hosts := getDnsCustomHostMapHostsInTx(ctx, tx, root)
			digest := dnsservice.DnsCustomHostMapDigest(hosts)
			if version == 0 {
				matched = len(hosts) == 0
				return
			}
			matched = hex.EncodeToString(digestBytes) == digest
			customHostMap = &dnsservice.DnsCustomHostMap{
				Format:  dnsservice.DnsFormat,
				Version: dnsSerialFromColumn(version),
				Digest:  digest,
				Hosts:   hosts,
			}
		}, server.TxRepeatableRead)
		return customHostMap, matched
	}
	customHostMap, matched := read()
	if matched {
		return customHostMap, nil
	}
	// the host list moved with no raise: raise now, so lbmap loads it under its own digest
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		_, returnErr = DnsRaiseCustomHostMapInTx(ctx, tx, root)
	}, server.TxReadCommitted)
	if returnErr != nil {
		return nil, returnErr
	}
	customHostMap, _ = read()
	return customHostMap, nil
}

// host_probes as the applied row holds it.
type dnsMapHostProbesJson struct {
	Added   []string        `json:"added,omitempty"`
	Dropped []string        `json:"dropped,omitempty"`
	Results map[string]bool `json:"results,omitempty"`
}

const dnsMapAppliedColumns = `interface_name, reported_version, digest, nginx_master, poll_time, probe_version, probe_time, host_probes`

// One applied row as dnsservice reads it.
func scanDnsMapInterface(result server.PgResult) *dnsservice.DnsMapInterface {
	lbInterface := &dnsservice.DnsMapInterface{HostProbes: map[string]bool{}}
	var reportedVersion int64
	var digest []byte
	var master *string
	var pollTime *time.Time
	var probeVersion int64
	var probeTime *time.Time
	var hostProbesJson []byte
	server.Raise(result.Scan(&lbInterface.Name, &reportedVersion, &digest, &master, &pollTime, &probeVersion, &probeTime, &hostProbesJson))
	lbInterface.ReportedVersion = dnsSerialFromColumn(reportedVersion)
	if 0 < len(digest) {
		lbInterface.Digest = hex.EncodeToString(digest)
	}
	if master != nil {
		lbInterface.Master = *master
	}
	if pollTime != nil {
		lbInterface.PollTime = *pollTime
	}
	lbInterface.ProbeVersion = dnsSerialFromColumn(probeVersion)
	if probeTime != nil {
		lbInterface.ProbeTime = *probeTime
	}
	hostProbes := &dnsMapHostProbesJson{}
	if err := json.Unmarshal(hostProbesJson, hostProbes); err == nil {
		lbInterface.AddedHosts = hostProbes.Added
		lbInterface.DroppedHosts = hostProbes.Dropped
		for host, passed := range hostProbes.Results {
			lbInterface.HostProbes[host] = passed
		}
	}
	return lbInterface
}

// Writes lbInterface back to its row.
func updateDnsMapInterfaceInTx(ctx context.Context, tx server.PgTx, lbInterface *dnsservice.DnsMapInterface) {
	var digest []byte
	if lbInterface.Digest != "" {
		digest = dnsCustomHostMapDigestBytes(lbInterface.Digest)
	}
	var master *string
	if lbInterface.Master != "" {
		master = &lbInterface.Master
	}
	var pollTime *time.Time
	if !lbInterface.PollTime.IsZero() {
		pollTime = &lbInterface.PollTime
	}
	var probeTime *time.Time
	if !lbInterface.ProbeTime.IsZero() {
		probeTime = &lbInterface.ProbeTime
	}
	hostProbes, err := json.Marshal(&dnsMapHostProbesJson{
		Added:   lbInterface.AddedHosts,
		Dropped: lbInterface.DroppedHosts,
		Results: lbInterface.HostProbes,
	})
	server.Raise(err)
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE dns_custom_host_map_applied
			SET reported_version = $2, digest = $3, nginx_master = $4, poll_time = $5, probe_version = $6, probe_time = $7, host_probes = $8::jsonb
			WHERE interface_name = $1
		`,
		lbInterface.Name,
		dnsSerialColumn(lbInterface.ReportedVersion),
		digest,
		master,
		pollTime,
		dnsSerialColumn(lbInterface.ProbeVersion),
		probeTime,
		string(hostProbes),
	))
}

// Applies change to each of names' rows in one transaction, in name order so two writers never wait on each
// other, adding a row for each name that has none when add is set.
func changeDnsMapInterfaces(ctx context.Context, names []string, add bool, change func(lbInterface *dnsservice.DnsMapInterface)) error {
	if !dnsCustomHostLinked() {
		return ErrDnsUnlinked
	}
	server.Tx(ctx, func(tx server.PgTx) {
		changeDnsMapInterfacesInTx(ctx, tx, names, add, change)
	}, server.TxReadCommitted)
	return nil
}

func changeDnsMapInterfacesInTx(ctx context.Context, tx server.PgTx, names []string, add bool, change func(*dnsservice.DnsMapInterface)) {
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	for _, name := range names {
		if add {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO dns_custom_host_map_applied (interface_name) VALUES ($1) ON CONFLICT (interface_name) DO NOTHING`,
				name,
			))
		}
		var lbInterface *dnsservice.DnsMapInterface
		result, err := tx.Query(ctx, `SELECT `+dnsMapAppliedColumns+` FROM dns_custom_host_map_applied WHERE interface_name = $1 FOR UPDATE`, name)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				lbInterface = scanDnsMapInterface(result)
			}
		})
		if lbInterface == nil {
			continue
		}
		change(lbInterface)
		updateDnsMapInterfaceInTx(ctx, tx, lbInterface)
	}
}

// Records lbmap's poll by dnsservice.ApplyDnsMapPoll.
func RecordDnsCustomHostMapPoll(ctx context.Context, poll *dnsservice.DnsMapPoll) error {
	return changeDnsMapInterfaces(ctx, []string{poll.Interface}, true, func(lbInterface *dnsservice.DnsMapInterface) {
		dnsservice.ApplyDnsMapPoll(lbInterface, poll)
	})
}

// Records lbmap's report at now by dnsservice.ApplyDnsMapReport.
func RecordDnsCustomHostMapReport(ctx context.Context, report *dnsservice.DnsCustomHostMapReport, now time.Time, routeHostCount int, droppedMaxCount int) error {
	return RecordDnsCustomHostMapReportForRoot(ctx, "", report, now, routeHostCount, droppedMaxCount)
}

// The hosted route commits its live probe sample and its full incident receipt together. Root lister
// locks precede interface row locks, the order used by the restore raise.
func RecordDnsCustomHostMapReportForRoot(ctx context.Context, root string, report *dnsservice.DnsCustomHostMapReport, now time.Time, routeHostCount, droppedMaxCount int) error {
	if !dnsCustomHostLinked() {
		return ErrDnsUnlinked
	}
	server.Tx(ctx, func(tx server.PgTx) {
		if root != "" {
			recordDnsRestoreMapReportInTx(ctx, tx, dnsLabelRoot(root), report)
		}
		changeDnsMapInterfacesInTx(ctx, tx, []string{report.Interface}, true, func(row *dnsservice.DnsMapInterface) {
			dnsservice.ApplyDnsMapReport(row, report, now, routeHostCount, droppedMaxCount)
		})
	}, server.TxReadCommitted)
	return nil
}

// Records an edge's round of route probes at now by dnsservice.ApplyDnsRouteProbes, for interfaces with rows.
func RecordDnsRouteProbes(ctx context.Context, report *dnsservice.DnsRouteProbeReport, now time.Time) error {
	results := map[string]*dnsservice.DnsRouteProbeResult{}
	names := []string{}
	for index := range report.Interfaces {
		result := &report.Interfaces[index]
		results[result.Name] = result
		names = append(names, result.Name)
	}
	return changeDnsMapInterfaces(ctx, names, false, func(lbInterface *dnsservice.DnsMapInterface) {
		dnsservice.ApplyDnsRouteProbes(lbInterface, results[lbInterface.Name], now)
	})
}

// Keeps one applied row for each of names, the lb inventory's interfaces, and removes every other (§12.14).
func SyncDnsCustomHostMapInterfaces(ctx context.Context, names []string) error {
	if !dnsCustomHostLinked() {
		return ErrDnsUnlinked
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM dns_custom_host_map_applied WHERE NOT (interface_name = ANY($1))`,
			names,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO dns_custom_host_map_applied (interface_name)
				SELECT unnest($1::varchar[])
				ON CONFLICT (interface_name) DO NOTHING
			`,
			names,
		))
	}, server.TxReadCommitted)
	return nil
}

// The map's state for the judgments, in query's transaction.
func getDnsCustomHostMapStateInTx(ctx context.Context, query server.PgCanQuery, root string) *dnsservice.DnsMapState {
	state := &dnsservice.DnsMapState{}
	result, err := query.Query(ctx, `SELECT version, digest, create_time FROM dns_custom_host_map ORDER BY version`)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var version int64
			var digest []byte
			var createTime time.Time
			server.Raise(result.Scan(&version, &digest, &createTime))
			state.Versions = append(state.Versions, dnsservice.DnsMapVersion{Version: dnsSerialFromColumn(version), Digest: hex.EncodeToString(digest), CreateTime: createTime})
		}
	})
	result, err = query.Query(ctx, `SELECT `+dnsMapAppliedColumns+` FROM dns_custom_host_map_applied ORDER BY interface_name`)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			state.Interfaces = append(state.Interfaces, scanDnsMapInterface(result))
		}
	})
	result, err = query.Query(
		ctx,
		`
			SELECT DISTINCT map_version FROM network_dns_custom_host
			WHERE root = $1 AND state = $2 AND map_version > 0
			ORDER BY map_version
		`,
		root,
		sidecar.DnsCustomHostStateActive,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var version int64
			server.Raise(result.Scan(&version))
			state.HostVersions = append(state.HostVersions, dnsSerialFromColumn(version))
		}
	})
	return state
}

// The map's state for the judgments (dnsservice.DnsMapState).
func GetDnsCustomHostMapState(ctx context.Context, root string) (*dnsservice.DnsMapState, error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	var state *dnsservice.DnsMapState
	server.Tx(ctx, func(tx server.PgTx) {
		state = getDnsCustomHostMapStateInTx(ctx, tx, dnsLabelRoot(root))
	}, server.TxRepeatableRead)
	return state, nil
}

// Sets ready_time at now on root's active custom hosts of each version ready names true where none is set, and
// clears it on those of each version it names false (dnsservice.DnsMapSettings.Judge's Ready).
func SetDnsCustomHostReadiness(ctx context.Context, root string, ready map[uint64]bool, now time.Time) error {
	if !dnsCustomHostLinked() {
		return ErrDnsUnlinked
	}
	readyVersions := []int64{}
	notReadyVersions := []int64{}
	for version, isReady := range ready {
		if isReady {
			readyVersions = append(readyVersions, dnsSerialColumn(version))
		} else {
			notReadyVersions = append(notReadyVersions, dnsSerialColumn(version))
		}
	}
	root = dnsLabelRoot(root)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_dns_custom_host SET ready_time = $3
				WHERE root = $1 AND state = $4 AND map_version = ANY($2) AND ready_time IS NULL
			`,
			root,
			readyVersions,
			now.UTC(),
			sidecar.DnsCustomHostStateActive,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_dns_custom_host SET ready_time = NULL WHERE root = $1 AND map_version = ANY($2) AND ready_time IS NOT NULL`,
			root,
			notReadyVersions,
		))
	}, server.TxReadCommitted)
	return nil
}

// One custom host's readiness (§13.4): ready once ready_time is set, else the interfaces not current for its
// map_version now, by dns.yml's windows.
type DnsCustomHostReadinessRow struct {
	Host       string
	MapVersion uint64
	ReadyTime  *time.Time
	// While not ready: the lb interfaces not current for MapVersion, in name order. Empty for a host not yet in
	// the map, pending certs' checks.
	PendingInterfaces []string
}

// The network's custom hosts' readiness in root (§13.4), in host order, strict-mode MX hosts left out: what GET
// /network/dns shows and TLS-M6-05's work list reads (host_ready: active with ready_time set).
func DnsCustomHostReadiness(ctx context.Context, root string, networkId server.Id) ([]*DnsCustomHostReadinessRow, error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	var rows []*DnsCustomHostReadinessRow
	server.Tx(ctx, func(tx server.PgTx) {
		rows = getDnsCustomHostReadinessInTx(ctx, tx, dnsLabelRoot(root), networkId, server.NowUtc())
	}, server.TxRepeatableRead)
	return rows, nil
}

// DnsCustomHostReadiness in query's transaction, judged at now.
func getDnsCustomHostReadinessInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id, now time.Time) []*DnsCustomHostReadinessRow {
	rows := []*DnsCustomHostReadinessRow{}
	var judgment *dnsservice.DnsMapJudgment
	for _, row := range getNetworkDnsCustomHostsInTx(ctx, query, networkId) {
		if row.StrictMx() || row.Root != root {
			continue
		}
		readiness := &DnsCustomHostReadinessRow{Host: row.Host, MapVersion: row.MapVersion, ReadyTime: row.ReadyTime}
		if row.State == sidecar.DnsCustomHostStateActive && row.ReadyTime == nil && 0 < row.MapVersion {
			if judgment == nil {
				settings := dnsZoneSettings().Map
				judgment = settings.Judge(getDnsCustomHostMapStateInTx(ctx, query, root), now)
			}
			readiness.PendingInterfaces = slices.Clone(judgment.Pending[row.MapVersion])
			if readiness.PendingInterfaces == nil && !judgment.Ready[row.MapVersion] {
				// no interface row at all: nothing is current
				readiness.PendingInterfaces = []string{}
			}
		}
		rows = append(rows, readiness)
	}
	return rows
}

// Drops the map's versions past their 7 days at now, the newest always kept, for taskworker's dns chain.
// Returns how many went.
func PurgeDnsCustomHostMapVersions(ctx context.Context, now time.Time) (int, error) {
	if !dnsCustomHostLinked() {
		return 0, nil
	}
	cutoff := now.Add(-DefaultDnsCustomHostMapModelSettings().VersionRetention)
	purgedCount := 0
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM dns_custom_host_map
				WHERE create_time < $1 AND version < (SELECT max(version) FROM dns_custom_host_map)
			`,
			cutoff.UTC(),
		))
		purgedCount = int(tag.RowsAffected())
	}, server.TxReadCommitted)
	return purgedCount, nil
}
