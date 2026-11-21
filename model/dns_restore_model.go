package model

// §12.6's restore step, its Postgres half (DNS-M5-15; D-DNS-10, D-OPS-14,
// FS-42, FI-12, I-14): the incident's lists of the RRsets a restore changed,
// and each listed network's admin event. dnsservice.RestoreStep runs the step,
// and dnshost.RestoreStore hands it these as dnsservice.DnsRestoreStore.
//
//   - A posted diff's RRsets map to networks through dns_label: the label or
//     alias directly below the root, in any state, names the network, since a
//     row's network never changes. <root>'s own records and a name no row
//     holds list with no network.
//   - A network with an open §21 case as it is listed is flagged (§21.10's
//     check), and the abuse role clears each flag (ABUSE's
//     ClearDnsRestoreFlag). The flag stays in the incident: the network's admin
//     event says only that a restore changed its records, since a barred case
//     reveals nothing to the network.
//   - Listing is idempotent by incident: an RRset already listed under the
//     incident is not listed again, and a network gets one admin event an
//     incident, in the transaction that first lists it, so a rerun after a
//     crash at any step lists the same.
//   - Until dns_restore_incident has its §2.8 row (plan-DNS-metadata-rows #2,
//     v9), the lists ride dns_root.restore_lists, the restore's incident record
//     v8's dns-1 names, beside the posted diffs DNS-M5-09 keeps there. Both are
//     kept 400 days (dns-1, api-8), which PurgeDnsRestoreLists holds.
//   - The custom-host map's half (DNS-M6-07; §12.6's last clause): the
//     incident's raise of the map's version, kept as a marker entry so a rerun
//     answers the same version, and the hosts the regenerated map added or
//     dropped as lbmap reported them, each listed once with its network, by its
//     row or else by the verified domain it is under, flagged and given its
//     network's one admin event as an RRset is.
//
// Listers of a root take an advisory lock first, so they run one at a time,
// then each admin event's chain lock, then dns_root's row lock last, the order
// every dns writer takes them in. Nothing here touches a table unless the
// binary links MG5, and each call then answers ErrDnsUnlinked.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The admin event a restore writes for each network it lists (§7.11): the
// incident is its target, and nothing else is said.
const (
	AdminEventActionDnsRestore       = "dns.restore"
	AdminEventTargetDnsRestore       = "dns_restore"
	AdminEventResultDnsRestoreListed = "listed"
)

// Tunables of the restore's lists.
type DnsRestoreModelSettings struct {
	// How long a listed RRset and a posted diff are kept: dns-1's and api-8's
	// 400 days. A longer value breaks that published ttl.
	ListRetention time.Duration
}

func DefaultDnsRestoreModelSettings() *DnsRestoreModelSettings {
	return &DnsRestoreModelSettings{
		ListRetention: 400 * 24 * time.Hour,
	}
}

// One entry of the incident record as dns_root.restore_lists holds it: an
// RRset, a custom host the regenerated map added or dropped (DNS-M6-07), or the
// incident's map raise, which lists nothing.
type dnsRestoreListRow struct {
	IncidentId    server.Id  `json:"incident_id"`
	ListTime      time.Time  `json:"list_time"`
	NetworkId     *server.Id `json:"network_id,omitempty"`
	Owner         string     `json:"owner"`
	Type          uint16     `json:"type"`
	Flagged       bool       `json:"flagged,omitempty"`
	FlagClearTime *time.Time `json:"flag_clear_time,omitempty"`
	// a custom host's entry: the host and dnsservice.DnsRestoreCustomHostAdded
	// or Dropped
	CustomHost       string `json:"custom_host,omitempty"`
	CustomHostChange string `json:"custom_host_change,omitempty"`
	// the incident's map raise: the version it raised to
	MapVersion uint64 `json:"map_version,omitempty"`
}

// Whether the row is the incident's map raise rather than an entry.
func (self *dnsRestoreListRow) mapRaise() bool {
	return 0 < self.MapVersion && self.CustomHost == "" && self.Owner == ""
}

// A posted diff as dns_root.reload_diffs holds it: the diff and when it came.
type dnsReloadDiffRow struct {
	*dnsservice.DnsReloadDiff
	PostTime *time.Time `json:"post_time,omitempty"`
}

// The label or alias directly below root that owner sits under, or "" for
// root itself. owner is canonical and at or below root, as every diff's key is.
func dnsOwnerLabel(root string, owner string) string {
	rest, ok := strings.CutSuffix(owner, "."+root)
	if !ok || rest == "" {
		return ""
	}
	labels := dns.SplitDomainName(rest + ".")
	return labels[len(labels)-1]
}

// Each label's network in root, for the labels keys sit under. A binary that
// does not link MG2 has no labels.
func getDnsRestoreLabelNetworks(ctx context.Context, root string, keys []dnsservice.DnsRecordKey) map[string]server.Id {
	labelNetworkIds := map[string]server.Id{}
	if !server.SidecarMg2Linked() {
		return labelNetworkIds
	}
	labels := []string{}
	for _, key := range keys {
		if label := dnsOwnerLabel(root, key.Owner); label != "" && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		return labelNetworkIds
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT label, network_id FROM dns_label WHERE root = $1 AND label = ANY($2) AND network_id IS NOT NULL`,
			dnsLabelRoot(root),
			labels,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var label string
				var networkId server.Id
				server.Raise(result.Scan(&label, &networkId))
				labelNetworkIds[label] = networkId
			}
		})
	})
	return labelNetworkIds
}

// root's incident record and posted diffs, or false for no such root.
func getDnsRestoreRowsInTx(ctx context.Context, tx server.PgTx, root string) (listRows []*dnsRestoreListRow, diffRows []*dnsReloadDiffRow, found bool) {
	var listJson []byte
	var diffJson []byte
	result, err := tx.Query(ctx, `SELECT restore_lists, reload_diffs FROM dns_root WHERE root = $1`, root)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&listJson, &diffJson))
			found = true
		}
	})
	if !found {
		return nil, nil, false
	}
	server.Raise(json.Unmarshal(listJson, &listRows))
	server.Raise(json.Unmarshal(diffJson, &diffRows))
	return listRows, diffRows, true
}

// Takes root's lister lock, held to the end of tx.
func lockDnsRestoreListsInTx(ctx context.Context, tx server.PgTx, root string) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('dns_restore_lists:' || $1, 0))`,
		root,
	))
}

// An incident's entries as dnsservice reads them.
func dnsRestoreListing(root string, incidentId server.Id, listRows []*dnsRestoreListRow) *dnsservice.DnsRestoreListing {
	listing := &dnsservice.DnsRestoreListing{Root: root, IncidentId: dnsSidecarId(incidentId)}
	for _, row := range listRows {
		if row.IncidentId != incidentId || row.mapRaise() {
			continue
		}
		entry := dnsservice.DnsRestoreEntry{
			ListTime:         row.ListTime,
			Owner:            row.Owner,
			Type:             row.Type,
			Flagged:          row.Flagged,
			FlagClearTime:    row.FlagClearTime,
			CustomHost:       row.CustomHost,
			CustomHostChange: row.CustomHostChange,
		}
		if row.NetworkId != nil {
			networkId := dnsSidecarId(*row.NetworkId)
			entry.NetworkId = &networkId
		}
		listing.Entries = append(listing.Entries, entry)
	}
	return listing
}

// Lists keys under incidentId at now (see the file header): each key not yet
// listed under it, with its network and flag, and one admin event for each
// network it lists first. Returns the incident's whole list.
func ListDnsRestoreChanges(ctx context.Context, root string, incidentId server.Id, keys []dnsservice.DnsRecordKey, now time.Time) (*dnsservice.DnsRestoreListing, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	labelNetworkIds := getDnsRestoreLabelNetworks(ctx, root, keys)
	// each network's case read before the transaction, on its own connection
	openNetworkIds := map[server.Id]bool{}
	for _, networkId := range labelNetworkIds {
		if _, read := openNetworkIds[networkId]; !read {
			openNetworkIds[networkId] = NetworkHasOpenAbuseCase(ctx, networkId)
		}
	}
	listTime := now.UTC().Truncate(time.Microsecond)
	var listing *dnsservice.DnsRestoreListing
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		listing = nil
		returnErr = nil
		lockDnsRestoreListsInTx(ctx, tx, root)
		listRows, _, found := getDnsRestoreRowsInTx(ctx, tx, root)
		if !found {
			returnErr = fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
			return
		}
		listedKeys := map[dnsservice.DnsRecordKey]bool{}
		listedNetworkIds := map[server.Id]bool{}
		for _, row := range listRows {
			if row.IncidentId != incidentId {
				continue
			}
			if row.CustomHost == "" && !row.mapRaise() {
				listedKeys[dnsservice.DnsRecordKey{Owner: row.Owner, Type: row.Type}] = true
			}
			if row.NetworkId != nil {
				listedNetworkIds[*row.NetworkId] = true
			}
		}
		newNetworkIds := []server.Id{}
		addedCount := 0
		for _, key := range keys {
			if listedKeys[key] {
				continue
			}
			listedKeys[key] = true
			row := &dnsRestoreListRow{IncidentId: incidentId, ListTime: listTime, Owner: key.Owner, Type: key.Type}
			if networkId, ok := labelNetworkIds[dnsOwnerLabel(root, key.Owner)]; ok {
				row.NetworkId = &networkId
				row.Flagged = openNetworkIds[networkId]
				if !listedNetworkIds[networkId] {
					listedNetworkIds[networkId] = true
					newNetworkIds = append(newNetworkIds, networkId)
				}
			}
			listRows = append(listRows, row)
			addedCount += 1
		}
		// chain locks in one order, so two listers' events cannot wait on each other
		slices.SortFunc(newNetworkIds, func(a server.Id, b server.Id) int { return bytes.Compare(a.Bytes(), b.Bytes()) })
		for _, networkId := range newNetworkIds {
			AddAdminEventInTx(ctx, tx, &AdminEvent{
				NetworkId:  networkId,
				ActorKind:  AdminEventActorSystem,
				Action:     AdminEventActionDnsRestore,
				TargetKind: AdminEventTargetDnsRestore,
				TargetId:   &incidentId,
				Result:     AdminEventResultDnsRestoreListed,
			})
		}
		if 0 < addedCount {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE dns_root SET restore_lists = $2::jsonb WHERE root = $1`,
				root,
				dnsJsonArray(listRows),
			))
		}
		listing = dnsRestoreListing(root, incidentId, listRows)
	}, server.TxReadCommitted)
	if returnErr != nil {
		return nil, returnErr
	}
	return listing, nil
}

// The custom-host map's raise for a restore's incident (§12.6's last clause;
// DNS-M6-07; QD-257): the version 2^24 above the newest of the counter, every
// custom host's map_version and every interface's reported version, every
// active TLS host's map_version with it and its ready time cleared, so each
// stays pending until every interface is current for the restored map. Kept in
// the incident record, so a rerun for the incident answers the same version.
func DnsRaiseCustomHostMapForRestore(ctx context.Context, root string, incidentId server.Id, now time.Time) (uint64, error) {
	if !dnsCustomHostLinked() {
		return 0, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	var version uint64
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		version = 0
		returnErr = nil
		lockDnsRestoreListsInTx(ctx, tx, root)
		listRows, _, found := getDnsRestoreRowsInTx(ctx, tx, root)
		if !found {
			returnErr = fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
			return
		}
		for _, row := range listRows {
			if row.IncidentId == incidentId && row.mapRaise() {
				version = row.MapVersion
				return
			}
		}
		var counter int64
		server.Raise(tx.QueryRow(ctx, `SELECT custom_host_map_version FROM dns_root WHERE root = $1 FOR UPDATE`, root).Scan(&counter))
		var hostMax int64
		server.Raise(tx.QueryRow(ctx, `SELECT coalesce(max(map_version), 0) FROM network_dns_custom_host WHERE root = $1`, root).Scan(&hostMax))
		var appliedMax int64
		server.Raise(tx.QueryRow(ctx, `SELECT coalesce(max(reported_version), 0) FROM dns_custom_host_map_applied`).Scan(&appliedMax))
		base := max(dnsSerialFromColumn(counter), dnsSerialFromColumn(hostMax), dnsSerialFromColumn(appliedMax))
		version = base + dnsservice.DnsRestoreSerialJump
		setDnsCustomHostMapVersionInTx(ctx, tx, root, version, getDnsCustomHostMapHostsInTx(ctx, tx, root), `true`)
		// Capture each interface's first load of this map before regular map updates can replace its report.
		server.RaisePgResult(tx.Exec(ctx, `UPDATE dns_custom_host_map_applied
			SET host_probes = jsonb_set(COALESCE(host_probes, '{}'::jsonb), '{restore}', jsonb_build_object('version', $1::bigint))`, dnsSerialColumn(version)))
		listRows = append(listRows, &dnsRestoreListRow{IncidentId: incidentId, ListTime: now.UTC().Truncate(time.Microsecond), MapVersion: version})
		server.RaisePgResult(tx.Exec(ctx, `UPDATE dns_root SET restore_lists = $2::jsonb WHERE root = $1`, root, dnsJsonArray(listRows)))
	}, server.TxReadCommitted)
	if returnErr != nil {
		return 0, returnErr
	}
	return version, nil
}

// The network a custom host the regenerated map named is under: its row's, or
// else the network whose verified domain holds it, the longest; none for a host
// neither names.
func getDnsRestoreCustomHostNetworkInTx(ctx context.Context, query server.PgCanQuery, host string) *server.Id {
	if rows := getDnsCustomHostRowsInTx(ctx, query, `host = $1`, host); 0 < len(rows) {
		networkId := rows[0].NetworkId
		return &networkId
	}
	var networkId *server.Id
	result, err := query.Query(
		ctx,
		`
			SELECT network_id FROM network_domain
			WHERE verify_time IS NOT NULL AND ($1 = domain OR $1 LIKE '%.' || domain)
			ORDER BY length(domain) DESC
			LIMIT 1
		`,
		host,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&networkId))
		}
	})
	return networkId
}

// Lists the hosts the regenerated map added and dropped under incidentId at now
// (see the file header): each not yet listed under it, with its network and
// flag, and one admin event for each network the incident lists first, RRsets'
// included. Returns the incident's custom-host entries.
func ListDnsRestoreCustomHosts(ctx context.Context, root string, incidentId server.Id, added []string, dropped []string, now time.Time) (*dnsservice.DnsRestoreListing, error) {
	if !dnsCustomHostLinked() {
		return nil, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	type change struct {
		host   string
		change string
	}
	changes := []change{}
	for _, host := range added {
		changes = append(changes, change{host: host, change: dnsservice.DnsRestoreCustomHostAdded})
	}
	for _, host := range dropped {
		changes = append(changes, change{host: host, change: dnsservice.DnsRestoreCustomHostDropped})
	}
	// each host's network and case read before the transaction
	hostNetworkIds := map[string]*server.Id{}
	openNetworkIds := map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, change := range changes {
			if _, read := hostNetworkIds[change.host]; !read {
				hostNetworkIds[change.host] = getDnsRestoreCustomHostNetworkInTx(ctx, conn, change.host)
			}
		}
	})
	for _, networkId := range hostNetworkIds {
		if networkId != nil {
			if _, read := openNetworkIds[*networkId]; !read {
				openNetworkIds[*networkId] = NetworkHasOpenAbuseCase(ctx, *networkId)
			}
		}
	}
	listTime := now.UTC().Truncate(time.Microsecond)
	var listing *dnsservice.DnsRestoreListing
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		listing = nil
		returnErr = nil
		lockDnsRestoreListsInTx(ctx, tx, root)
		listRows, _, found := getDnsRestoreRowsInTx(ctx, tx, root)
		if !found {
			returnErr = fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
			return
		}
		listedChanges := map[change]bool{}
		listedNetworkIds := map[server.Id]bool{}
		for _, row := range listRows {
			if row.IncidentId != incidentId {
				continue
			}
			if row.CustomHost != "" {
				listedChanges[change{host: row.CustomHost, change: row.CustomHostChange}] = true
			}
			if row.NetworkId != nil {
				listedNetworkIds[*row.NetworkId] = true
			}
		}
		newNetworkIds := []server.Id{}
		addedCount := 0
		for _, change := range changes {
			if listedChanges[change] {
				continue
			}
			listedChanges[change] = true
			row := &dnsRestoreListRow{IncidentId: incidentId, ListTime: listTime, CustomHost: change.host, CustomHostChange: change.change}
			if networkId := hostNetworkIds[change.host]; networkId != nil {
				row.NetworkId = networkId
				row.Flagged = openNetworkIds[*networkId]
				if !listedNetworkIds[*networkId] {
					listedNetworkIds[*networkId] = true
					newNetworkIds = append(newNetworkIds, *networkId)
				}
			}
			listRows = append(listRows, row)
			addedCount += 1
		}
		slices.SortFunc(newNetworkIds, func(a server.Id, b server.Id) int { return bytes.Compare(a.Bytes(), b.Bytes()) })
		for _, networkId := range newNetworkIds {
			AddAdminEventInTx(ctx, tx, &AdminEvent{
				NetworkId:  networkId,
				ActorKind:  AdminEventActorSystem,
				Action:     AdminEventActionDnsRestore,
				TargetKind: AdminEventTargetDnsRestore,
				TargetId:   &incidentId,
				Result:     AdminEventResultDnsRestoreListed,
			})
		}
		if 0 < addedCount {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE dns_root SET restore_lists = $2::jsonb WHERE root = $1`, root, dnsJsonArray(listRows)))
		}
		listing = dnsRestoreListing(root, incidentId, listRows)
		listing.Entries = slices.DeleteFunc(listing.Entries, func(entry dnsservice.DnsRestoreEntry) bool { return entry.CustomHost == "" })
	}, server.TxReadCommitted)
	if returnErr != nil {
		return nil, returnErr
	}
	return listing, nil
}

// An incident's list in root, empty for an incident that listed nothing.
func GetDnsRestoreListing(ctx context.Context, root string, incidentId server.Id) (*dnsservice.DnsRestoreListing, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	var listing *dnsservice.DnsRestoreListing
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		listing = nil
		returnErr = nil
		listRows, _, found := getDnsRestoreRowsInTx(ctx, tx, root)
		if !found {
			returnErr = fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
			return
		}
		listing = dnsRestoreListing(root, incidentId, listRows)
	}, server.TxReadCommitted)
	if returnErr != nil {
		return nil, returnErr
	}
	return listing, nil
}

// Drops every root's listed RRsets and posted diffs past their 400 days at
// now, a diff with no post time among them, for taskworker's dns chain.
// Returns how many went.
func PurgeDnsRestoreLists(ctx context.Context, now time.Time) (int, error) {
	if !server.SidecarMg5Linked() {
		return 0, nil
	}
	cutoff := now.Add(-DefaultDnsRestoreModelSettings().ListRetention)
	roots := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		// a root with nothing kept costs this read alone
		result, err := conn.Query(ctx, `SELECT root FROM dns_root WHERE restore_lists <> '[]'::jsonb OR reload_diffs <> '[]'::jsonb ORDER BY root`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var root string
				server.Raise(result.Scan(&root))
				roots = append(roots, root)
			}
		})
	})
	purgedCount := 0
	for _, root := range roots {
		rootPurgedCount := 0
		server.Tx(ctx, func(tx server.PgTx) {
			rootPurgedCount = 0
			lockDnsRestoreListsInTx(ctx, tx, root)
			listRows, diffRows, found := getDnsRestoreRowsInTx(ctx, tx, root)
			if !found {
				return
			}
			keptLists := slices.DeleteFunc(slices.Clone(listRows), func(row *dnsRestoreListRow) bool {
				return row.ListTime.Before(cutoff)
			})
			keptDiffs := slices.DeleteFunc(slices.Clone(diffRows), func(row *dnsReloadDiffRow) bool {
				return row.PostTime == nil || row.PostTime.Before(cutoff)
			})
			dropped := len(listRows) - len(keptLists) + len(diffRows) - len(keptDiffs)
			if dropped == 0 {
				return
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE dns_root SET restore_lists = $2::jsonb, reload_diffs = $3::jsonb WHERE root = $1`,
				root,
				dnsJsonArray(keptLists),
				dnsJsonArray(keptDiffs),
			))
			rootPurgedCount = dropped
		}, server.TxReadCommitted)
		purgedCount += rootPurgedCount
	}
	return purgedCount, nil
}

// rows as a JSON array, [] for none, never null, which the columns' '[]'
// default and every reader expect.
func dnsJsonArray[T any](rows []T) string {
	if len(rows) == 0 {
		return "[]"
	}
	data, err := json.Marshal(rows)
	server.Raise(err)
	return string(data)
}
