package model

// §12.5's CT replay and label reclaim (DNS-M5-16; D-DNS-10, R-DNS-5, I-14, FS-41, FS-42, FI-12; §19.10's labels
// step), so I-14 holds across a Postgres restore:
//
//   - ReplayDnsCtNames, which taskworker's dns_ct_replay runs after the restore step, takes every <root> name CT
//     logged since the sidecar dump's time less 7 days, and inserts the label of each that dns_label lacks as retiring
//     with no network, recording each logged leaf's SHA-256 under the incident in dns_restore_incident. Such a row
//     routes nothing and resolves to nothing, yet holds its name from every claim, rename and draw, since a row is
//     never deleted (dns_label_model.go). A name dns_label holds, in any state, is left as it is. A rerun records the
//     same rows, so a crash anywhere reruns to one result.
//   - ReclaimDnsLabel, POST /network/dns/label-reclaim's and the dns block's POST /dns/label-reclaim's: a signature by
//     the key of a logged leaf naming the name, over ("urnetwork-sidecar-v1/label-reclaim", name, network_id,
//     incident id) (sidecar.VerifyDnsReclaimProof), re-binds an ownerless retiring row to the caller's network, as its
//     label when the network holds none in the root, else as its alias when it holds no live one; the zone comes to be
//     with it (EnsureNetworkZoneInTx), whose write moves the label map's cursor so tlsedge routes the name again.
//   - TombstoneUnclaimedDnsLabels tombstones each ownerless retiring row nobody re-bound within D-DNS-10's 90 days,
//     and PurgeDnsRestoreIncidents drops the incident record past dns-1's 400 days; both run in dns_maintenance.
//
// Nothing here reads or writes unless the binary links MG2 and MG5.

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// The admin event a reclaim adds (§7.11): the network's name came back to it.
const (
	AdminEventActionDnsLabelReclaim = "dns.label_reclaim"
	AdminEventTargetDnsLabel        = "dns_label"
)

// Tunables of the replay and the reclaim.
type DnsReclaimSettings struct {
	// How long an ownerless retiring name waits for its network before it tombstones: D-DNS-10's 90 days,
	// outlasting every certificate a lost label could hold.
	ReclaimWindow time.Duration
	// How long the incident record is kept: dns-1's 400 days for a restore's lists.
	IncidentRetention time.Duration
	// The most names one replay transaction inserts, so a large root costs bounded transactions.
	ReplayBatchCount int
	// The most rows one tombstone pass or purge changes, so a backlog costs bounded passes.
	SweepBatchCount int
}

func DefaultDnsReclaimSettings() *DnsReclaimSettings {
	return &DnsReclaimSettings{
		ReclaimWindow:     90 * 24 * time.Hour,
		IncidentRetention: 400 * 24 * time.Hour,
		ReplayBatchCount:  1000,
		SweepBatchCount:   1000,
	}
}

// One certificate CT logged under a root: a name it carries and its leaf's SHA-256.
type DnsCtName struct {
	// A name under the root as the log has it: <label>.<root> or *.<label>.<root>, with or without its final dot.
	Name       string
	LeafSha256 [sha256.Size]byte
}

// The label directly below root that name sits under, lowercase, or "" for a name outside root or root itself.
func dnsCtLabel(root string, name string) string {
	name = dns.CanonicalName(strings.TrimSpace(name))
	rest, ok := strings.CutSuffix(name, "."+root)
	if !ok || rest == "" {
		return ""
	}
	labels := dns.SplitDomainName(rest + ".")
	label := labels[len(labels)-1]
	if !isDnsCtLabel(label) {
		return ""
	}
	return label
}

// Whether label is one dns_label could hold: 1 to 63 lowercase LDH characters with no dash at either end.
func isDnsCtLabel(label string) bool {
	if label == "" || 63 < len(label) || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for index := range len(label) {
		character := label[index]
		if !('a' <= character && character <= 'z' || '0' <= character && character <= '9' || character == '-') {
			return false
		}
	}
	return true
}

// §19.10's labels step for root under incidentId (see the file header): each logged label dns_label lacks goes in
// retiring with no network, its leaves recorded. Returns how many labels went in this run.
func ReplayDnsCtNames(ctx context.Context, root string, incidentId server.Id, names []*DnsCtName, now time.Time) (insertedCount int, returnErr error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return 0, ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	settings := DefaultDnsReclaimSettings()
	labelLeaves := map[string][][sha256.Size]byte{}
	labels := []string{}
	for _, name := range names {
		label := dnsCtLabel(root, name.Name)
		if label == "" {
			continue
		}
		if _, ok := labelLeaves[label]; !ok {
			labels = append(labels, label)
		}
		labelLeaves[label] = append(labelLeaves[label], name.LeafSha256)
	}
	recordTime := now.UTC().Truncate(time.Microsecond)
	for start := 0; start < len(labels); start += settings.ReplayBatchCount {
		batch := labels[start:min(start+settings.ReplayBatchCount, len(labels))]
		server.Tx(ctx, func(tx server.PgTx) {
			for _, label := range batch {
				tag := server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO dns_label (root, label, network_id, kind, state, state_time)
						VALUES ($1, $2, NULL, $3, $4, $5)
						ON CONFLICT (root, label) DO NOTHING
					`,
					root,
					label,
					DnsLabelKindLabel,
					DnsLabelStateRetiring,
					recordTime,
				))
				if tag.RowsAffected() == 1 {
					insertedCount += 1
				} else if !isDnsLabelOwnerlessRetiringInTx(ctx, tx, root, label) {
					// a name the restored rows hold is its network's, or tombstoned: left alone
					continue
				}
				for _, leafSha256 := range labelLeaves[label] {
					server.RaisePgResult(tx.Exec(
						ctx,
						`
							INSERT INTO dns_restore_incident (root, incident_id, name, leaf_sha256, record_time)
							VALUES ($1, $2, $3, $4, $5)
							ON CONFLICT DO NOTHING
						`,
						root,
						incidentId,
						label,
						leafSha256[:],
						recordTime,
					))
				}
			}
		}, server.TxReadCommitted)
	}
	return insertedCount, nil
}

// Whether root's row for label has no network and is retiring.
func isDnsLabelOwnerlessRetiringInTx(ctx context.Context, query server.PgCanQuery, root string, label string) bool {
	ownerless := false
	result, err := query.Query(
		ctx,
		`SELECT network_id IS NULL AND state = $3 FROM dns_label WHERE root = $1 AND label = $2`,
		root,
		label,
		DnsLabelStateRetiring,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&ownerless))
		}
	})
	return ownerless
}

// One reclaim: the request a route took, for the network its credential names.
type DnsLabelReclaim struct {
	Root string
	// The caller's network: a network credential's, or the sidecar token's.
	NetworkId server.Id
	Request   *sidecar.DnsLabelReclaimRequest
	// The admin event's actor (§7.11).
	ActorKind string
	ActorId   *server.Id
	RequestId *server.Id
}

// §12.8's label reclaim (see the file header): the name and the kind it took back, or a *sidecar.DnsApiError: the
// tuple for another network, a name the root's rows hold otherwise than ownerless and retiring, a leaf the replay did
// not record under the incident, and a signature that does not prove the tuple are each refused, and nothing is
// written.
func ReclaimDnsLabel(ctx context.Context, reclaim *DnsLabelReclaim) (*sidecar.DnsLabelReclaimAnswer, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, dnsRecordsUnavailable("the label tables are not linked")
	}
	request := reclaim.Request
	if request == nil {
		return nil, dnsRecordsInvalid("no body")
	}
	root := dnsLabelRoot(reclaim.Root)
	if server.Id(request.NetworkId) != reclaim.NetworkId {
		return nil, dnsRecordsRefused("the tuple names another network")
	}
	label := dnsCtLabel(root, request.Name)
	if label == "" || dns.CanonicalName(request.Name) != label+"."+root {
		return nil, dnsRecordsInvalid("%q is not a label or alias of the root", request.Name)
	}
	proof := &sidecar.DnsReclaimProof{Name: request.Name, NetworkId: request.NetworkId, IncidentId: request.IncidentId}
	if err := sidecar.VerifyDnsReclaimProof(proof, request.Leaf, request.Signature); err != nil {
		if errors.Is(err, sidecar.ErrDnsReclaimProofInvalid) {
			return nil, dnsRecordsInvalid("%v", err)
		}
		return nil, dnsRecordsRefused("%v", err)
	}
	leafSha256 := sidecar.DnsReclaimLeafSha256(request.Leaf)
	var answer *sidecar.DnsLabelReclaimAnswer
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			answer, returnErr = reclaimDnsLabelInTx(ctx, tx, root, label, leafSha256, reclaim)
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, dnsRecordsApiError(err)
	}
	return answer, nil
}

func reclaimDnsLabelInTx(ctx context.Context, tx server.PgTx, root string, label string, leafSha256 [sha256.Size]byte, reclaim *DnsLabelReclaim) (*sidecar.DnsLabelReclaimAnswer, error) {
	networkId := reclaim.NetworkId
	// the network's label lock first, as EnsureNetworkLabelInTx takes it, so a draw and a reclaim of one network
	// never both bind a label
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('dns_label:' || $1::uuid::text, 0))`,
		networkId,
	))
	var holderId *server.Id
	var state string
	found := false
	result, err := tx.Query(
		ctx,
		`SELECT network_id, state FROM dns_label WHERE root = $1 AND label = $2 FOR UPDATE`,
		root,
		label,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&holderId, &state))
			found = true
		}
	})
	if !found || holderId != nil || state != DnsLabelStateRetiring {
		return nil, dnsRecordsError(http.StatusConflict, sidecar.DnsApiCodeRefused, "%s is not an ownerless retiring name", label)
	}
	logged := false
	result, err = tx.Query(
		ctx,
		`
			SELECT EXISTS (
				SELECT 1 FROM dns_restore_incident
				WHERE root = $1 AND incident_id = $2 AND name = $3 AND leaf_sha256 = $4
			)
		`,
		root,
		server.Id(reclaim.Request.IncidentId),
		label,
		leafSha256[:],
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&logged))
		}
	})
	if !logged {
		return nil, dnsRecordsRefused("the leaf is not one the incident's replay logged for %s", label)
	}
	kind := DnsLabelKindLabel
	if getNetworkLabelInTx(ctx, tx, networkId, root) != "" {
		kind = DnsLabelKindAlias
		aliasHeld := false
		result, err = tx.Query(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM dns_label WHERE network_id = $1 AND root = $2 AND kind = $3 AND state = $4)`,
			networkId,
			root,
			DnsLabelKindAlias,
			DnsLabelStateLive,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&aliasHeld))
			}
		})
		if aliasHeld {
			return nil, dnsRecordsError(http.StatusConflict, sidecar.DnsApiCodeRefused, "the network holds a label and an alias in the root already")
		}
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE dns_label SET network_id = $3, kind = $4, state = $5, state_time = $6 WHERE root = $1 AND label = $2`,
		root,
		label,
		networkId,
		kind,
		DnsLabelStateLive,
		server.NowUtc(),
	))
	// the zone with its name, whose write moves the label map's cursor for the network's names
	if _, err := EnsureNetworkZoneInTx(ctx, tx, networkId, root, nil); err != nil && !errors.Is(err, ErrDnsZoneSuspended) {
		return nil, err
	}
	targetId := networkId
	AddAdminEventInTx(ctx, tx, &AdminEvent{
		NetworkId:  networkId,
		ActorKind:  reclaim.ActorKind,
		ActorId:    reclaim.ActorId,
		Action:     AdminEventActionDnsLabelReclaim,
		TargetKind: AdminEventTargetDnsLabel,
		TargetId:   &targetId,
		Result:     AdminEventResultOk,
		RequestId:  reclaim.RequestId,
	})
	return &sidecar.DnsLabelReclaimAnswer{Name: label + "." + strings.TrimSuffix(root, "."), Kind: kind}, nil
}

// Tombstones each ownerless retiring name of any root whose 90 days have passed at now, at most one batch: names
// nobody re-bound after a restore (D-DNS-10). Returns how many, and whether more wait.
func TombstoneUnclaimedDnsLabels(ctx context.Context, now time.Time) (tombstonedCount int, more bool, returnErr error) {
	if !server.SidecarMg2Linked() {
		return 0, false, nil
	}
	settings := DefaultDnsReclaimSettings()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE dns_label
				SET state = $1, state_time = $2
				WHERE (root, label) IN (
					SELECT root, label FROM dns_label
					WHERE network_id IS NULL AND state = $3 AND state_time < $4
					LIMIT $5
				)
			`,
			DnsLabelStateTombstoned,
			now.UTC(),
			DnsLabelStateRetiring,
			now.Add(-settings.ReclaimWindow).UTC(),
			settings.SweepBatchCount,
		))
		tombstonedCount = int(tag.RowsAffected())
	})
	return tombstonedCount, tombstonedCount == settings.SweepBatchCount, nil
}

// Drops the incident record's rows past their 400 days at now, at most one batch. Returns how many, and whether
// more wait.
func PurgeDnsRestoreIncidents(ctx context.Context, now time.Time) (purgedCount int, more bool, returnErr error) {
	if !server.SidecarMg5Linked() {
		return 0, false, nil
	}
	settings := DefaultDnsReclaimSettings()
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM dns_restore_incident
				WHERE (root, incident_id, name, leaf_sha256) IN (
					SELECT root, incident_id, name, leaf_sha256 FROM dns_restore_incident
					WHERE record_time < $1
					LIMIT $2
				)
			`,
			now.Add(-settings.IncidentRetention).UTC(),
			settings.SweepBatchCount,
		))
		purgedCount = int(tag.RowsAffected())
	})
	return purgedCount, purgedCount == settings.SweepBatchCount, nil
}

// The root's last restore's incident, the nonce a reclaim signs; nil before any restore.
func GetDnsRestoreIncidentId(ctx context.Context, root string) (*server.Id, error) {
	state, err := GetDnsRootState(ctx, dnsLabelRoot(root))
	if err != nil {
		return nil, err
	}
	if state.RestoreIncidentId == nil {
		return nil, nil
	}
	incidentId := server.Id(*state.RestoreIncidentId)
	return &incidentId, nil
}

// The names a replay found for root under incidentId, for the incident's listing and its tests.
func GetDnsRestoreIncidentNames(ctx context.Context, root string, incidentId server.Id) (names []string, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	names = []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT DISTINCT name FROM dns_restore_incident WHERE root = $1 AND incident_id = $2 ORDER BY name`,
			dnsLabelRoot(root),
			incidentId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				server.Raise(result.Scan(&name))
				names = append(names, name)
			}
		})
	})
	return names, nil
}
