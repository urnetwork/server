package model

// §12.6's one write path (DNS-M5-09; R-DNS-2, R-DNS-7, D-DNS-3, I-12): every
// record write in a root, from the record API, certs' challenges, §12.7's
// system records, suspensions and a network's deletion, goes through
// DnsWriteInTx, so each is one Postgres transaction that takes one serial and
// one journal row. In order:
//
//  1. the caller's §16.6 gate step, through Gate, in the same transaction
//     (DNS-M5-12), which may answer from a stored result with no write;
//  2. the zone's lock, pg_advisory_xact_lock, transaction-scoped and so safe
//     under PgBouncer's transaction pooling;
//  3. the zone's current rows, a suspension's refusal, the copy rows the write
//     needs (dnsservice.PlanCopyRows) and the zone lint over the result
//     (dnsservice.LintZoneWrite), so a refused write takes nothing;
//  4. dns_root's row lock as late as it can be, since it serializes every
//     write in the root: ErrDnsRestoring while restoring, the compare-and-swap,
//     then serial+1, the rows, one dns_zone_journal row and the zone's version
//     at that serial (plan reading 3), committed together;
//  5. after the commit, DnsWritePost: {dns}serial:<root> raised by a script that
//     never lowers it, then DNS_ZONE_CHANGED hints on PushTopicDns to the
//     network's sidecars (reading 18: no body, the serial in the version). A
//     Redis failure loses nothing, since the feed rereads Postgres at each
//     hold's end.
//
// dnshost.Store's Write is step 4 alone, the transaction dnsservice.DnsStore
// promises, which sidecartest.RunDnsStoreSuite holds to the fake's. The engine
// writes nothing else, and every model writer goes through the whole path.
//
// Each write runs its transaction read committed: the zone lock keeps the
// zone's rows still, and the row lock reads the latest serial, where a
// repeatable-read snapshot would fail every writer queued behind it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
)

var (
	// The zone has no label in the root, so no owner can be placed in it.
	ErrDnsZoneUnlabeled = errors.New("dns zone has no label")
	// An owner the write names is not in the zone it writes.
	ErrDnsOwnerOutsideZone = errors.New("dns owner outside the zone")
	// The zone is suspended (§12.9): nothing is written until Restore.
	ErrDnsZoneSuspended = errors.New("dns zone suspended")
	// An owner the write names is at or below a suspended host (§12.9).
	ErrDnsHostSuspended = errors.New("dns host suspended")
)

// The write path's barriers (§22.8), held by a T2 or T3 schedule: before the
// root's row lock, at the serial taken inside the transaction, and after the
// commit before the raise. Called with the root; nil in production.
var Testing_DnsWriteBarrier func(name string, root string)

// The barrier names.
const (
	DnsWriteBarrierBefore = "model.dns_write.before"
	DnsWriteBarrier       = "model.dns_write"
	DnsWriteBarrierAfter  = "model.dns_write.after"
)

func dnsWriteBarrier(name string, root string) {
	if barrier := Testing_DnsWriteBarrier; barrier != nil {
		barrier(name, root)
	}
}

// One write through the path.
type DnsWriteArgs struct {
	Root string
	// The zone written, or zero for <root>'s own records, which no network
	// holds.
	NetworkId server.Id
	// When set, the write goes ahead only while the zone's version equals this
	// (§12.8's compare-and-swap; plan reading 3).
	ExpectVersion *uint64
	Upserts       []dnsservice.DnsRecordSet
	Deletes       []dnsservice.DnsRecordKey
	// §16.6's gate step, first in the transaction (DNS-M5-12). proceed false
	// ends the write with nothing written and no error: the gate answered from a
	// stored result. An error rolls the transaction back. nil runs no gate.
	Gate func(ctx context.Context, tx server.PgTx) (proceed bool, err error)
	// Added in the write's transaction, as §12.8 has user and key writes do. nil
	// adds none.
	AdminEvent *AdminEvent
}

// What a write did.
type DnsWriteResult struct {
	// The serial the write took. Zero when the gate answered.
	Serial uint64
	// The gate answered from a stored result, so nothing was written.
	Answered bool
	// The rows the write committed, copy rows included, as the journal carries
	// them.
	Upserts []dnsservice.DnsRecordSet
	Deletes []dnsservice.DnsRecordKey
	// DnsWritePost was registered on the caller's transaction, which runs it
	// after the commit. False on a raw transaction, whose owner calls it.
	PostRegistered bool
}

// The names a zone holds owners under: <root> for its own records, else the
// network's label in root, in any state, then each of its live aliases (§12.5;
// reading 11). Empty when the network has no label in root.
func getDnsZoneApexesInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id) []string {
	if networkId == (server.Id{}) {
		return []string{root}
	}
	if !server.SidecarMg2Linked() {
		return nil
	}
	label := ""
	aliases := []string{}
	result, err := query.Query(
		ctx,
		`
			SELECT label, kind FROM dns_label
			WHERE network_id = $1 AND root = $2 AND (kind = $3 OR state = $4)
			ORDER BY label
		`,
		networkId,
		root,
		DnsLabelKindLabel,
		DnsLabelStateLive,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var name string
			var kind string
			server.Raise(result.Scan(&name, &kind))
			if kind == DnsLabelKindLabel {
				if label == "" {
					label = name
				}
			} else {
				aliases = append(aliases, name+"."+root)
			}
		}
	})
	if label == "" {
		return nil
	}
	return append([]string{label + "." + root}, aliases...)
}

// One stored row and whether a suspension withholds it.
type dnsStoredRow struct {
	set      dnsservice.DnsRecordSet
	withheld bool
}

// Every row a zone holds, withheld or not: a network's, or <root>'s own.
func getDnsZoneRowsInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id) []*dnsStoredRow {
	var result server.PgResult
	var err error
	if networkId == (server.Id{}) {
		result, err = query.Query(
			ctx,
			`
				SELECT owner, rrtype, kind, ttl, rdata, withheld_time IS NOT NULL
				FROM network_dns_record
				WHERE root = $1 AND network_id IS NULL
				ORDER BY owner, rrtype
			`,
			root,
		)
	} else {
		result, err = query.Query(
			ctx,
			`
				SELECT owner, rrtype, kind, ttl, rdata, withheld_time IS NOT NULL
				FROM network_dns_record
				WHERE network_id = $1 AND root = $2
				ORDER BY owner, rrtype
			`,
			networkId,
			root,
		)
	}
	rows := []*dnsStoredRow{}
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var owner string
			var rrType int32
			var kind string
			var ttl int64
			var rdata []string
			var withheld bool
			server.Raise(result.Scan(&owner, &rrType, &kind, &ttl, &rdata, &withheld))
			row := &dnsStoredRow{
				set: dnsservice.DnsRecordSet{
					Owner: owner,
					Type:  uint16(rrType),
					Ttl:   uint32(ttl),
					Kind:  dnsservice.DnsRecordKind(kind),
				},
				withheld: withheld,
			}
			if 0 < len(rdata) {
				row.set.Rdata = rdata
			}
			rows = append(rows, row)
		}
	})
	return rows
}

// Whether owner is name or below it.
func dnsOwnerAtOrBelow(owner string, name string) bool {
	return owner == name || strings.HasSuffix(owner, "."+name)
}

// Whether the zone is suspended: while its reason is set, which only Restore
// clears, a passed until included, so a write never lands between an until and
// the pass that lifts it.
func getDnsZoneSuspendedInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id) bool {
	suspended := false
	result, err := query.Query(
		ctx,
		`SELECT suspend_reason IS NOT NULL FROM network_dns_zone WHERE root = $1 AND network_id = $2`,
		root,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&suspended))
		}
	})
	return suspended
}

// Takes the zone's lock to the end of the transaction. Every writer of the zone
// takes it before reading the zone's rows, so they stay still until commit.
func lockDnsZoneInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('dns_zone:' || $1 || ':' || $2::uuid::text, 0))`,
		root,
		networkId,
	))
}

// §12.6's write in the caller's transaction (see the file header), which must
// be read committed or the row lock fails every writer queued behind another.
// A refusal returns its error with nothing written, and the caller rolls back
// any work of its own. A binary that does not link MG5 answers ErrDnsUnlinked.
func DnsWriteInTx(ctx context.Context, tx server.PgTx, args *DnsWriteArgs) (*DnsWriteResult, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	if args.Gate != nil {
		proceed, err := args.Gate(ctx, tx)
		if err != nil {
			return nil, err
		}
		if !proceed {
			return &DnsWriteResult{Answered: true}, nil
		}
	}
	now := server.NowUtc()
	lockDnsZoneInTx(ctx, tx, args.Root, args.NetworkId)

	apexes := getDnsZoneApexesInTx(ctx, tx, args.Root, args.NetworkId)
	if len(apexes) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrDnsZoneUnlabeled, args.Root)
	}
	for _, key := range dnsWriteKeys(args.Upserts, args.Deletes) {
		if !slices.ContainsFunc(apexes, func(apex string) bool { return dns.IsSubDomain(apex, key.Owner) }) {
			return nil, fmt.Errorf("%w: %s", ErrDnsOwnerOutsideZone, key.Owner)
		}
		// <root>'s own records hold nothing in a network's zone
		if args.NetworkId == (server.Id{}) && key.Owner != args.Root && isDnsNetworkZoneOwnerInTx(ctx, tx, args.Root, key.Owner) {
			return nil, fmt.Errorf("%w: %s", ErrDnsOwnerOutsideZone, key.Owner)
		}
	}
	if args.NetworkId != (server.Id{}) && getDnsZoneSuspendedInTx(ctx, tx, args.Root, args.NetworkId) {
		return nil, fmt.Errorf("%w: %s", ErrDnsZoneSuspended, apexes[0])
	}

	stored := getDnsZoneRowsInTx(ctx, tx, args.Root, args.NetworkId)
	before := make([]dnsservice.DnsRecordSet, 0, len(stored))
	for _, row := range stored {
		if row.set.Kind == dnsservice.DnsRecordKindWithheld {
			for _, key := range dnsWriteKeys(args.Upserts, args.Deletes) {
				if dnsOwnerAtOrBelow(key.Owner, row.set.Owner) {
					return nil, fmt.Errorf("%w: %s", ErrDnsHostSuspended, row.set.Owner)
				}
			}
		}
		before = append(before, row.set)
	}
	after := dnsservice.DnsRowsAfter(before, args.Upserts, args.Deletes)
	copyUpserts := []dnsservice.DnsRecordSet{}
	copyDeletes := []dnsservice.DnsRecordKey{}
	for _, apex := range apexes {
		apexUpserts, apexDeletes := dnsservice.PlanCopyRows(apex, after)
		copyUpserts = append(copyUpserts, apexUpserts...)
		copyDeletes = append(copyDeletes, apexDeletes...)
	}
	upserts := slices.Clone(args.Upserts)
	deletes := slices.Clone(args.Deletes)
	written := map[dnsservice.DnsRecordKey]bool{}
	for _, key := range dnsWriteKeys(upserts, deletes) {
		written[key] = true
	}
	for _, set := range copyUpserts {
		if !written[set.Key()] {
			upserts = append(upserts, set)
		}
	}
	held := map[dnsservice.DnsRecordKey]bool{}
	for _, row := range before {
		held[row.Key()] = true
	}
	for _, key := range copyDeletes {
		if !written[key] && held[key] {
			deletes = append(deletes, key)
		}
	}
	// a delete of what the zone does not hold changes nothing, so it leaves the row
	deletes = slices.DeleteFunc(deletes, func(key dnsservice.DnsRecordKey) bool {
		return !held[key]
	})
	if err := dnsservice.LintZoneWrite(args.Root, before, dnsservice.DnsRowsAfter(before, upserts, deletes)); err != nil {
		return nil, err
	}

	serial, err := dnsCommitInTx(ctx, tx, &dnsservice.DnsZoneWrite{
		Root:          args.Root,
		NetworkId:     dnsSidecarId(args.NetworkId),
		ExpectVersion: args.ExpectVersion,
		Upserts:       upserts,
		Deletes:       deletes,
	}, now, nil)
	if err != nil {
		return nil, err
	}
	if args.AdminEvent != nil {
		AddAdminEventInTx(ctx, tx, args.AdminEvent)
	}
	result := &DnsWriteResult{
		Serial:  serial,
		Upserts: upserts,
		Deletes: deletes,
	}
	root := args.Root
	networkId := args.NetworkId
	result.PostRegistered = server.AddTxPostCommit(tx, "dns_write:"+root, func() any {
		DnsWritePost(context.Background(), root, serial, networkId)
		return nil
	})
	return result, nil
}

// Every key a write names, upserts first.
func dnsWriteKeys(upserts []dnsservice.DnsRecordSet, deletes []dnsservice.DnsRecordKey) []dnsservice.DnsRecordKey {
	keys := make([]dnsservice.DnsRecordKey, 0, len(upserts)+len(deletes))
	for index := range upserts {
		keys = append(keys, upserts[index].Key())
	}
	return append(keys, deletes...)
}

// Whether a network's zone holds owner: a name at or below a live label or
// alias of root, which <root>'s own records never write. A retiring or
// tombstoned name holds no zone, and <root>'s own CAA may stand at it (§12.7).
func isDnsNetworkZoneOwnerInTx(ctx context.Context, query server.PgCanQuery, root string, owner string) bool {
	if !server.SidecarMg2Linked() {
		return false
	}
	labels := dns.SplitDomainName(strings.TrimSuffix(owner, root))
	if len(labels) == 0 {
		return false
	}
	held := false
	result, err := query.Query(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM dns_label WHERE root = $1 AND label = $2 AND state = $3)`,
		root,
		labels[len(labels)-1],
		DnsLabelStateLive,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&held))
		}
	})
	return held
}

// §12.6's transaction itself, step 4 of the file header: under dns_root's row
// lock, ErrDnsRestoring while restoring, the compare-and-swap, serial+1, the
// rows, one journal row and the zone's version. Every store write ends here. A
// challenge's or canary's row expires DnsModelSettings.ChallengeLifetime after
// now. withholds leave the journal as deletes and stay stored, withheld, for a
// suspension's Restore (§12.9); an upsert of a stored row releases it.
func dnsCommitInTx(ctx context.Context, tx server.PgTx, write *dnsservice.DnsZoneWrite, now time.Time, withholds []dnsservice.DnsRecordKey) (uint64, error) {
	if 0 < len(withholds) {
		write = &dnsservice.DnsZoneWrite{
			Root:          write.Root,
			NetworkId:     write.NetworkId,
			ExpectVersion: write.ExpectVersion,
			Upserts:       write.Upserts,
			Deletes:       append(slices.Clone(write.Deletes), withholds...),
		}
	}
	if err := write.Validate(); err != nil {
		return 0, err
	}
	withheld := map[dnsservice.DnsRecordKey]bool{}
	for _, key := range withholds {
		withheld[key] = true
	}
	networkId := server.Id(write.NetworkId)
	dnsWriteBarrier(DnsWriteBarrierBefore, write.Root)
	result, err := tx.Query(
		ctx,
		`SELECT serial, restoring, apex_version FROM dns_root WHERE root = $1 FOR UPDATE`,
		write.Root,
	)
	found := false
	var serialColumn int64
	var restoring bool
	var apexVersion int64
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&serialColumn, &restoring, &apexVersion))
			found = true
		}
	})
	if !found {
		return 0, fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, write.Root)
	}
	if restoring {
		return 0, fmt.Errorf("%w: %s", dnsservice.ErrDnsRestoring, write.Root)
	}
	if write.ExpectVersion != nil {
		version, err := getDnsZoneVersionInTx(ctx, tx, write.Root, networkId)
		if err != nil {
			return 0, err
		}
		if *write.ExpectVersion != version {
			return 0, fmt.Errorf("%w: %d is not %d", dnsservice.ErrDnsVersionConflict, *write.ExpectVersion, version)
		}
	}
	serial := dnsSerialFromColumn(serialColumn) + 1
	row := write.Row(serial)
	diff, err := json.Marshal(row)
	if err != nil {
		return 0, err
	}
	var networkIdColumn *server.Id
	if networkId != (server.Id{}) {
		networkIdColumn = &networkId
	}
	expireTime := now.Add(DefaultDnsModelSettings().ChallengeLifetime)
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for index := range row.Upserts {
			set := &row.Upserts[index]
			var setExpireTime *time.Time
			if set.Kind == dnsservice.DnsRecordKindChallenge || set.Kind == dnsservice.DnsRecordKindCanary {
				setExpireTime = &expireTime
			}
			rdata := set.Rdata
			if rdata == nil {
				rdata = []string{}
			}
			batch.Queue(
				`
					INSERT INTO network_dns_record (
						root, owner, rrtype, network_id, kind, ttl, rdata, create_time, expire_time, withheld_time
					) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULL)
					ON CONFLICT (root, owner, rrtype) DO UPDATE
					SET
						network_id = EXCLUDED.network_id,
						kind = EXCLUDED.kind,
						ttl = EXCLUDED.ttl,
						rdata = EXCLUDED.rdata,
						expire_time = EXCLUDED.expire_time,
						withheld_time = NULL
				`,
				write.Root,
				set.Owner,
				int32(set.Type),
				networkIdColumn,
				string(set.Kind),
				int64(set.Ttl),
				rdata,
				now,
				setExpireTime,
			)
		}
		for _, key := range row.Deletes {
			if withheld[key] {
				batch.Queue(
					`UPDATE network_dns_record SET withheld_time = $4 WHERE root = $1 AND owner = $2 AND rrtype = $3`,
					write.Root,
					key.Owner,
					int32(key.Type),
					now,
				)
				continue
			}
			batch.Queue(
				`DELETE FROM network_dns_record WHERE root = $1 AND owner = $2 AND rrtype = $3`,
				write.Root,
				key.Owner,
				int32(key.Type),
			)
		}
		batch.Queue(
			`
				INSERT INTO dns_zone_journal (root, serial, network_id, commit_time, format, diff)
				VALUES ($1, $2, $3, $4, $5, $6)
			`,
			write.Root,
			dnsSerialColumn(serial),
			networkIdColumn,
			now,
			row.Format,
			string(diff),
		)
		if networkIdColumn == nil {
			batch.Queue(
				`UPDATE dns_root SET serial = $2, apex_version = $2 WHERE root = $1`,
				write.Root,
				dnsSerialColumn(serial),
			)
		} else {
			batch.Queue(
				`UPDATE dns_root SET serial = $2 WHERE root = $1`,
				write.Root,
				dnsSerialColumn(serial),
			)
			batch.Queue(
				`
					INSERT INTO network_dns_zone (root, network_id, version, create_time)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (root, network_id) DO UPDATE
					SET version = EXCLUDED.version
				`,
				write.Root,
				networkId,
				dnsSerialColumn(serial),
				now,
			)
		}
	})
	dnsWriteBarrier(DnsWriteBarrier, write.Root)
	return serial, nil
}

// §12.6's write in a transaction of its own, then its post, which the
// transaction runs once it commits: the path for a caller holding no
// transaction. A refusal rolls back the gate's work with the rest.
func DnsWrite(ctx context.Context, args *DnsWriteArgs) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = DnsWriteInTx(ctx, tx, args)
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

// A refusal carried out of server.Tx, which rolls it back.
type dnsWriteRefusal struct {
	err error
}

// Runs callback, turning a dnsWriteRefusal's panic back into its error. Any
// other panic passes on.
func dnsCatchRefusal(callback func()) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if refusal, ok := recovered.(dnsWriteRefusal); ok {
				returnErr = refusal.err
				return
			}
			panic(recovered)
		}
	}()
	callback()
	return nil
}

// After a write commits at serial: raises {dns}serial:<root>, never lowering
// it, then hints the network's sidecars on PushTopicDns. A failure of either is
// logged and lost, since the feed rereads Postgres at each hold's end and a
// client resyncs within the hour (§6.8). Holds no Postgres connection across
// Redis.
func DnsWritePost(ctx context.Context, root string, serial uint64, networkId server.Id) {
	dnsWriteBarrier(DnsWriteBarrierAfter, root)
	if _, err := RaiseDnsSerial(ctx, nil, root, serial); err != nil {
		glog.Warningf("[dns]raise lost, the feed rereads at its hold's end: %v\n", err)
	}
	if networkId == (server.Id{}) {
		return
	}
	if err := hintDnsZoneChanged(ctx, networkId, serial); err != nil {
		glog.Warningf("[dns]zone hint lost, clients resync: %v\n", err)
	}
}

// Every current client of the network's active sidecars: each identity's
// newest binding (§5.15). None in a binary that does not link MG1.
func getNetworkSidecarClientIds(ctx context.Context, networkId server.Id) []server.Id {
	clientIds := []server.Id{}
	if !server.SidecarMg1Linked() {
		return clientIds
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT ON (sidecar_identity_binding.sidecar_id) sidecar_identity_binding.client_id
				FROM sidecar_identity
				INNER JOIN sidecar_identity_binding ON sidecar_identity_binding.sidecar_id = sidecar_identity.sidecar_id
				WHERE sidecar_identity.network_id = $1 AND sidecar_identity.status = $2
				ORDER BY sidecar_identity_binding.sidecar_id, sidecar_identity_binding.generation DESC
			`,
			networkId,
			SidecarIdentityStatusActive,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	return clientIds
}

// DNS_ZONE_CHANGED to the network's sidecars, versioned by the serial, with no
// body (plan reading 18).
func hintDnsZoneChanged(ctx context.Context, networkId server.Id, serial uint64) error {
	clientIds := getNetworkSidecarClientIds(ctx, networkId)
	if len(clientIds) == 0 {
		return nil
	}
	writer, err := sidecarHintWriter()
	if err != nil {
		return err
	}
	connectClientIds := make([]connect.Id, len(clientIds))
	for index, clientId := range clientIds {
		connectClientIds[index] = connect.Id(clientId)
	}
	return writer.Hint(ctx, connectClientIds, &sidecar.ClientHint{
		Topic:   sidecar.PushTopicDns,
		Version: serial,
		Kind:    uint32(protocol.MessageType_DNS_ZONE_CHANGED),
	})
}

// dnsservice.DnsStore.Write for dnshost.Store: the transaction of step 4 alone,
// with no lint, copy rows or suspension, which the engine never needs since it
// writes through the model's own callers, then {dns}serial:<root> raised through
// redis. redis nil is the process's default. A lost raise costs the feed a
// hold's length, so it is no error.
func DnsStoreWrite(ctx context.Context, redis DnsSerialRedis, write *dnsservice.DnsZoneWrite) (uint64, error) {
	if !server.SidecarMg5Linked() {
		return 0, ErrDnsUnlinked
	}
	var serial uint64
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			serial, returnErr = dnsCommitInTx(ctx, tx, write, server.NowUtc(), nil)
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return 0, err
	}
	dnsWriteBarrier(DnsWriteBarrierAfter, write.Root)
	if _, err := RaiseDnsSerial(ctx, redis, write.Root, serial); err != nil {
		glog.Warningf("[dns]raise lost, the feed rereads at its hold's end: %v\n", err)
	}
	return serial, nil
}
