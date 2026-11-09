package model

// §12.14's dns_root and dns_zone_journal, and {dns}serial:<root> (DNS-M5-09;
// §12.6, D-DNS-3): the store half of §12.6's one write path, which
// dns_write_model.go wraps with the per-network lock, the lint and the copy
// rows. dnshost.Store serves these to the dns engine as dnsservice.DnsStore, so
// sidecartest.RunDnsStoreSuite holds them to the fake's promises (D-TEST-2).
//
//   - A root's serial rises only in a write's transaction, under dns_root's row
//     lock, by one, with the write's rows and its one journal row; or in a
//     restore's transaction, by DnsRestoreSerialJump above everything it could
//     have missed. So serial order is commit order, and a reader at any snapshot
//     sees a prefix of the journal.
//   - {dns}serial:<root> projects dns_root.serial for the feed's wake. It is
//     raised after the commit by a script that never lowers it, comparing digit
//     strings as pushservice.CompareVersion does, so no serial passes through
//     a Lua number. A lost raise costs a feed seconds, never a write, since the
//     feed rereads Postgres at each hold's end (§12.6). PgBouncer's transaction
//     pooling bars LISTEN/NOTIFY and session locks, so neither appears here.
//   - While restoring, every write answers ErrDnsRestoring and takes no serial.
//
// Refusals are errors; a failed statement raises, as the rest of this package
// does. Nothing here reads or writes the tables unless the binary links MG5
// (server.SidecarMg5Linked), and then each call answers ErrDnsUnlinked.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
)

// The binary does not link MG5, so §12.14's tables do not exist for it.
var ErrDnsUnlinked = errors.New("dns tables are not linked in this binary")

// A serial past what a bigint holds. Unreachable by writes and 2^24 restore
// jumps, so a sign of a corrupt value, refused rather than wrapped.
var ErrDnsSerialRange = errors.New("dns serial out of range")

// Tunables of the dns model.
type DnsModelSettings struct {
	// How long a journal row is kept after its commit: §2.8 dns-1's 7 days. A
	// longer value breaks that published ttl. Non-positive keeps nothing past the
	// next purge.
	JournalRetention time.Duration
	// How long a challenge or canary row lives before taskworker removes it:
	// D-DNS-8's 1 h and dns-1's ttl.
	ChallengeLifetime time.Duration
	// The most rows one purge statement deletes, so a backlog after an outage
	// costs bounded passes.
	PurgeBatchSize int
	// How many rows a snapshot's reader may hold unread before the stream waits
	// for its consumer.
	SnapshotBufferSize int
	// The most expired challenges and canaries one taskworker pass removes, so a
	// backlog costs bounded passes; a full pass asks for the next sooner.
	ExpiryBatchSize int
}

func DefaultDnsModelSettings() *DnsModelSettings {
	return &DnsModelSettings{
		JournalRetention:   7 * 24 * time.Hour,
		ChallengeLifetime:  1 * time.Hour,
		PurgeBatchSize:     10000,
		SnapshotBufferSize: 256,
		ExpiryBatchSize:    1000,
	}
}

// The redis key whose value is root's last raised serial (§12.14). One hash tag
// for every root, so the family is one slot.
func DnsSerialKey(root string) string {
	return "{dns}serial:" + root
}

// The family a process's one key-event subscriber watches for raises.
const DnsSerialKeyPattern = "{dns}serial:*"

// The root a key DnsSerialKey made names, or false.
func ParseDnsSerialKey(key string) (string, bool) {
	const prefix = "{dns}serial:"
	if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
		return "", false
	}
	return key[len(prefix):], true
}

// Raises {dns}serial:<root> to ARGV[1] only when that is higher, and returns
// what the key holds after. A value is a canonical decimal, compared as
// pushservice's hint script compares versions: lengths, then the digits before
// the last 10 and the last 10, each run exact as a double, so no serial ever
// passes through tonumber whole (D-PROTO-12). A stored value that is not
// canonical is replaced. The key has no ttl: it projects dns_root.serial, and a
// lost key reads zero until the next raise.
var DnsRaiseSerialScript = &sidecar.RedisScript{
	Name: "model.dns_raise_serial",
	Source: `
local key = KEYS[1]
local serial = ARGV[1]

-- 1 to 20 digits, the first not 0
local function canonical(text)
  return #text <= 20 and string.find(text, '^[1-9][0-9]*$') ~= nil
end

-- a above b, both canonical, by runs of at most 10 digits
local function higher(a, b)
  if #a ~= #b then
    return #a > #b
  end
  if #a > 10 then
    local aHigh = tonumber(string.sub(a, 1, #a - 10))
    local bHigh = tonumber(string.sub(b, 1, #b - 10))
    if aHigh ~= bHigh then
      return aHigh > bHigh
    end
    return tonumber(string.sub(a, -10)) > tonumber(string.sub(b, -10))
  end
  return tonumber(a) > tonumber(b)
end

if not canonical(serial) then
  return redis.error_reply('ERR dns serial not canonical')
end
local stored = redis.call('GET', key)
if stored and canonical(stored) and not higher(serial, stored) then
  return stored
end
redis.call('SET', key, serial)
return serial
`,
}

// What {dns}serial:<root> needs of Redis: the raise script and a read. A
// host's sidecar.Redis is one.
type DnsSerialRedis interface {
	RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error)
	Get(ctx context.Context, key string) (value []byte, found bool, err error)
}

// The Redis model calls raise and read through, made at first use: server's
// pools behind the hint writer's adapter.
var dnsDefaultRedis = sync.OnceValue(func() DnsSerialRedis {
	return NewSidecarPushRedis(nil)
})

// Raises root's key to serial through redis, never lowering it, and returns
// what the key holds after. redis nil is the process's default.
func RaiseDnsSerial(ctx context.Context, redis DnsSerialRedis, root string, serial uint64) (uint64, error) {
	if redis == nil {
		redis = dnsDefaultRedis()
	}
	reply, err := redis.RunScript(ctx, DnsRaiseSerialScript, []string{DnsSerialKey(root)}, []string{pushservice.FormatVersion(serial)})
	if err != nil {
		return 0, err
	}
	if reply == nil || reply.Kind != sidecar.RedisReplyBulk {
		return 0, fmt.Errorf("dns raise: a reply that is no serial")
	}
	return pushservice.ParseVersion(string(reply.Bulk))
}

// What root's key holds, zero when it is absent or not a serial. redis nil is
// the process's default.
func GetDnsRaisedSerial(ctx context.Context, redis DnsSerialRedis, root string) (uint64, error) {
	if redis == nil {
		redis = dnsDefaultRedis()
	}
	value, found, err := redis.Get(ctx, DnsSerialKey(root))
	if err != nil || !found {
		return 0, err
	}
	serial, err := pushservice.ParseVersion(string(value))
	if err != nil {
		// soft state that is not a serial reads as none, and the next raise
		// replaces it
		return 0, nil
	}
	return serial, nil
}

// A serial as the bigint column holds it.
func dnsSerialColumn(serial uint64) int64 {
	if math.MaxInt64 < serial {
		panic(fmt.Errorf("%w: %d", ErrDnsSerialRange, serial))
	}
	return int64(serial)
}

// A bigint column's value as a serial. A negative one is corrupt.
func dnsSerialFromColumn(value int64) uint64 {
	if value < 0 {
		panic(fmt.Errorf("%w: %d", ErrDnsSerialRange, value))
	}
	return uint64(value)
}

// A network id as dnsservice holds it.
func dnsSidecarId(id server.Id) sidecar.Id {
	return sidecar.Id(id)
}

// Adds root's dns_root row at serial zero, and leaves an existing one as it is.
func EnsureDnsRoot(ctx context.Context, root string) error {
	if !server.SidecarMg5Linked() {
		return ErrDnsUnlinked
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO dns_root (root) VALUES ($1) ON CONFLICT (root) DO NOTHING`,
			root,
		))
	})
	return nil
}

// One read of root's row, or ErrDnsRootUnknown.
func GetDnsRootState(ctx context.Context, root string) (*dnsservice.DnsRootState, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	var state *dnsservice.DnsRootState
	server.Db(ctx, func(conn server.PgConn) {
		state = getDnsRootStateInTx(ctx, conn, root)
	})
	if state == nil {
		return nil, fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
	}
	return state, nil
}

// root's row as query sees it, or nil.
func getDnsRootStateInTx(ctx context.Context, query server.PgCanQuery, root string) *dnsservice.DnsRootState {
	var state *dnsservice.DnsRootState
	result, err := query.Query(
		ctx,
		`SELECT serial, restore_serial, restoring, restore_incident_id FROM dns_root WHERE root = $1`,
		root,
	)
	server.WithPgResult(result, err, func() {
		if !result.Next() {
			return
		}
		var serial int64
		var restoreSerial int64
		var restoring bool
		var restoreIncidentId *server.Id
		server.Raise(result.Scan(&serial, &restoreSerial, &restoring, &restoreIncidentId))
		state = &dnsservice.DnsRootState{
			Root:          root,
			Serial:        dnsSerialFromColumn(serial),
			RestoreSerial: dnsSerialFromColumn(restoreSerial),
			Restoring:     restoring,
		}
		if restoreIncidentId != nil {
			incidentId := dnsSidecarId(*restoreIncidentId)
			state.RestoreIncidentId = &incidentId
		}
	})
	return state
}

// Journal rows above after, at most limit, in serial order and stopping at the
// first hole (dnsservice.DnsStore.Journal).
func GetDnsJournal(ctx context.Context, root string, after uint64, limit int) (*dnsservice.DnsJournalRead, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	if limit <= 0 {
		return nil, fmt.Errorf("journal limit %d is not positive", limit)
	}
	var read *dnsservice.DnsJournalRead
	var returnErr error
	// one snapshot for the serial and the rows, so the gap rule reads both alike
	server.Tx(ctx, func(tx server.PgTx) {
		read = nil
		returnErr = nil
		state := getDnsRootStateInTx(ctx, tx, root)
		if state == nil {
			returnErr = fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
			return
		}
		read = &dnsservice.DnsJournalRead{Serial: state.Serial}
		if math.MaxInt64 <= after {
			// nothing can follow it
			return
		}
		result, err := tx.Query(
			ctx,
			`
				SELECT serial, diff
				FROM dns_zone_journal
				WHERE root = $1 AND $2 < serial
				ORDER BY serial
				LIMIT $3
			`,
			root,
			dnsSerialColumn(after),
			limit,
		)
		server.WithPgResult(result, err, func() {
			next := after + 1
			for result.Next() {
				var serial int64
				var diff []byte
				server.Raise(result.Scan(&serial, &diff))
				if dnsSerialFromColumn(serial) != next {
					// a hole: the rest waits for a reload
					break
				}
				row := dnsservice.DnsJournalRow{}
				// a row of a later format passes through whole, so the edge stops
				// at it rather than applying half (wire.go)
				server.Raise(json.Unmarshal(diff, &row))
				read.Rows = append(read.Rows, row)
				next += 1
			}
		})
		read.Gap = after < read.Serial && len(read.Rows) == 0
	}, pgx.ReadOnly, pgx.RepeatableRead)
	if returnErr != nil {
		return nil, returnErr
	}
	return read, nil
}

// The serial of a zone's last write, or zero for one never written (plan
// reading 3). networkId zero names <root>'s own records.
func GetDnsZoneVersion(ctx context.Context, root string, networkId server.Id) (uint64, error) {
	if !server.SidecarMg5Linked() {
		return 0, ErrDnsUnlinked
	}
	var version uint64
	var returnErr error
	server.Db(ctx, func(conn server.PgConn) {
		version, returnErr = getDnsZoneVersionInTx(ctx, conn, root, networkId)
	})
	return version, returnErr
}

func getDnsZoneVersionInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id) (uint64, error) {
	var version int64
	found := false
	if networkId == (server.Id{}) {
		result, err := query.Query(ctx, `SELECT apex_version FROM dns_root WHERE root = $1`, root)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&version))
				found = true
			}
		})
		if !found {
			return 0, fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
		}
		return dnsSerialFromColumn(version), nil
	}
	result, err := query.Query(
		ctx,
		`SELECT version FROM network_dns_zone WHERE root = $1 AND network_id = $2`,
		root,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&version))
			found = true
		}
	})
	if !found {
		if getDnsRootStateInTx(ctx, query, root) == nil {
			return 0, fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
		}
		return 0, nil
	}
	return dnsSerialFromColumn(version), nil
}

// Deletes root's journal rows committed before before, oldest first, at most a
// batch a statement, and returns how many went. Readers below the purge then
// see a gap and reload (§12.6).
func PurgeDnsJournal(ctx context.Context, root string, before time.Time) (int, error) {
	if !server.SidecarMg5Linked() {
		return 0, ErrDnsUnlinked
	}
	settings := DefaultDnsModelSettings()
	purgedCount := 0
	for {
		var batchCount int64
		known := true
		server.Tx(ctx, func(tx server.PgTx) {
			known = getDnsRootStateInTx(ctx, tx, root) != nil
			if !known {
				return
			}
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					DELETE FROM dns_zone_journal
					WHERE root = $1 AND serial IN (
						SELECT serial FROM dns_zone_journal
						WHERE root = $1 AND commit_time < $2
						ORDER BY serial
						LIMIT $3
					)
				`,
				root,
				before.UTC(),
				settings.PurgeBatchSize,
			))
			batchCount = tag.RowsAffected()
		})
		if !known {
			return purgedCount, fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
		}
		purgedCount += int(batchCount)
		if batchCount < int64(settings.PurgeBatchSize) {
			return purgedCount, nil
		}
	}
}

// Every root's journal rows past the retention at now, for taskworker
// (§12.14's 7 days). Returns how many went.
func PurgeDnsJournals(ctx context.Context, now time.Time) (int, error) {
	if !server.SidecarMg5Linked() {
		return 0, nil
	}
	roots := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT root FROM dns_root ORDER BY root`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var root string
				server.Raise(result.Scan(&root))
				roots = append(roots, root)
			}
		})
	})
	before := now.Add(-DefaultDnsModelSettings().JournalRetention)
	purgedCount := 0
	for _, root := range roots {
		count, err := PurgeDnsJournal(ctx, root, before)
		purgedCount += count
		if err != nil {
			return purgedCount, err
		}
	}
	return purgedCount, nil
}

// Sets or clears dns_root.restoring (§19.10). Setting it while clear forgets
// the last restore's incident, so a reload waits for this restore's own
// transaction (plan reading 8), and setting it while set keeps what the
// transaction recorded, so a rerun after a crash changes nothing.
func SetDnsRestoring(ctx context.Context, root string, restoring bool) error {
	if !server.SidecarMg5Linked() {
		return ErrDnsUnlinked
	}
	var tag int64
	server.Tx(ctx, func(tx server.PgTx) {
		tag = server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE dns_root
				SET
					restore_incident_id = CASE WHEN $2 AND NOT restoring THEN NULL ELSE restore_incident_id END,
					restoring = $2
				WHERE root = $1
			`,
			root,
			restoring,
		)).RowsAffected()
	}, server.TxReadCommitted)
	if tag == 0 {
		return fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
	}
	return nil
}

// §12.6's restore transaction (dnsservice.DnsStore.CommitRestore): serial and
// restore_serial become the highest of the restored serial, observedSerial and
// {dns}serial:<root>, plus DnsRestoreSerialJump, recording incidentId, and the
// key is raised after the commit. Run again for the incident it recorded, it
// changes nothing and answers the same serial. redis nil is the process's
// default.
func CommitDnsRestore(ctx context.Context, redis DnsSerialRedis, root string, incidentId server.Id, observedSerial uint64) (uint64, error) {
	if !server.SidecarMg5Linked() {
		return 0, ErrDnsUnlinked
	}
	// soft state, read before the transaction, so no Redis call waits inside it;
	// a raise after this read is a later write's, which restoring refuses
	raisedSerial, err := GetDnsRaisedSerial(ctx, redis, root)
	if err != nil {
		return 0, err
	}
	var restoreSerial uint64
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		restoreSerial = 0
		returnErr = nil
		result, err := tx.Query(
			ctx,
			`SELECT serial, restore_serial, restoring, restore_incident_id FROM dns_root WHERE root = $1 FOR UPDATE`,
			root,
		)
		found := false
		var serial int64
		var storedRestoreSerial int64
		var restoring bool
		var storedIncidentId *server.Id
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&serial, &storedRestoreSerial, &restoring, &storedIncidentId))
				found = true
			}
		})
		switch {
		case !found:
			returnErr = fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
			return
		case !restoring:
			returnErr = fmt.Errorf("%w: %s", dnsservice.ErrDnsNotRestoring, root)
			return
		case storedIncidentId != nil && *storedIncidentId != incidentId:
			returnErr = fmt.Errorf("%w: %s", dnsservice.ErrDnsRestoreIncident, root)
			return
		case storedIncidentId != nil:
			restoreSerial = dnsSerialFromColumn(storedRestoreSerial)
			return
		}
		restoreSerial = max(dnsSerialFromColumn(serial), observedSerial, raisedSerial) + dnsservice.DnsRestoreSerialJump
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE dns_root
				SET serial = $2, restore_serial = $2, restore_incident_id = $3
				WHERE root = $1
			`,
			root,
			dnsSerialColumn(restoreSerial),
			incidentId,
		))
	}, server.TxReadCommitted)
	if returnErr != nil {
		return 0, returnErr
	}
	if _, err := RaiseDnsSerial(ctx, redis, root, restoreSerial); err != nil {
		// the restore stands; a lost raise costs the feed a hold's length
		return restoreSerial, nil
	}
	return restoreSerial, nil
}

// Keeps an edge's posted diff for the restore step's lists, with when it came,
// which PurgeDnsRestoreLists holds to 400 days. Until dns_restore_incident has
// its §2.8 row (plan-DNS-metadata-rows #2) the diffs ride dns_root, the
// restore's incident record dns-1 names.
func AddDnsReloadDiff(ctx context.Context, diff *dnsservice.DnsReloadDiff) error {
	if !server.SidecarMg5Linked() {
		return ErrDnsUnlinked
	}
	postTime := server.NowUtc()
	diffJson, err := json.Marshal([]*dnsReloadDiffRow{{DnsReloadDiff: diff, PostTime: &postTime}})
	if err != nil {
		return err
	}
	var tag int64
	server.Tx(ctx, func(tx server.PgTx) {
		tag = server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE dns_root SET reload_diffs = reload_diffs || $2::jsonb WHERE root = $1`,
			diff.Root,
			string(diffJson),
		)).RowsAffected()
	}, server.TxReadCommitted)
	if tag == 0 {
		return fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, diff.Root)
	}
	return nil
}

// Every diff kept for root, oldest first.
func GetDnsReloadDiffs(ctx context.Context, root string) ([]*dnsservice.DnsReloadDiff, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	var diffJson []byte
	found := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT reload_diffs FROM dns_root WHERE root = $1`, root)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&diffJson))
				found = true
			}
		})
	})
	if !found {
		return nil, fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
	}
	diffs := []*dnsservice.DnsReloadDiff{}
	if err := json.Unmarshal(diffJson, &diffs); err != nil {
		return nil, err
	}
	return diffs, nil
}

// Every visible row of root at one serial (dnsservice.DnsStore.Snapshot): one
// read-only repeatable-read transaction reads the serial and streams the rows in
// owner then type order, so a write landing during the read changes nothing it
// returns and neither end holds the zone. Withheld rows are left out (§12.9).
// The caller closes what comes back, which ends the transaction.
func OpenDnsSnapshot(ctx context.Context, root string) (dnsservice.DnsSnapshotRows, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	settings := DefaultDnsModelSettings()
	streamCtx, cancel := context.WithCancel(ctx)
	self := &dnsSnapshotStream{
		cancel: cancel,
		rows:   make(chan *dnsservice.DnsRecordSet, settings.SnapshotBufferSize),
		done:   make(chan struct{}),
	}
	header := make(chan *dnsservice.DnsSnapshot, 1)
	go func() {
		defer close(self.done)
		defer close(self.rows)
		err := dnsCatch(func() error {
			var streamErr error
			server.Tx(streamCtx, func(tx server.PgTx) {
				state := getDnsRootStateInTx(streamCtx, tx, root)
				if state == nil {
					streamErr = fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
					return
				}
				header <- &dnsservice.DnsSnapshot{
					Format:        dnsservice.DnsFormat,
					Root:          root,
					Serial:        state.Serial,
					RestoreSerial: state.RestoreSerial,
				}
				result, err := tx.Query(
					streamCtx,
					`
						SELECT owner, rrtype, kind, ttl, rdata
						FROM network_dns_record
						WHERE root = $1 AND withheld_time IS NULL
						ORDER BY owner, rrtype
					`,
					root,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						set := scanDnsRecordSet(result)
						select {
						case self.rows <- set:
						case <-streamCtx.Done():
							panic(context.Cause(streamCtx))
						}
					}
				})
			}, server.OptNoRetry(), pgx.ReadOnly, pgx.RepeatableRead)
			return streamErr
		})
		if err != nil {
			self.err = err
			select {
			case header <- nil:
			default:
			}
		}
	}()
	snapshot := <-header
	if snapshot == nil {
		<-self.done
		return nil, self.err
	}
	self.snapshot = *snapshot
	return self, nil
}

// Runs callback, turning a raise into its error.
func dnsCatch(callback func() error) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%v", recovered)
			}
		}
	}()
	return callback()
}

// One row of network_dns_record as wire.go holds it, from owner, rrtype, kind,
// ttl and rdata.
func scanDnsRecordSet(result server.PgResult) *dnsservice.DnsRecordSet {
	var owner string
	var rrType int32
	var kind string
	var ttl int64
	var rdata []string
	server.Raise(result.Scan(&owner, &rrType, &kind, &ttl, &rdata))
	set := &dnsservice.DnsRecordSet{
		Owner: owner,
		Type:  uint16(rrType),
		Ttl:   uint32(ttl),
		Kind:  dnsservice.DnsRecordKind(kind),
	}
	if 0 < len(rdata) {
		set.Rdata = rdata
	}
	return set
}

// A snapshot read from its transaction's goroutine. Not safe for concurrent use.
type dnsSnapshotStream struct {
	snapshot dnsservice.DnsSnapshot
	cancel   context.CancelFunc
	rows     chan *dnsservice.DnsRecordSet
	done     chan struct{}
	// set before rows closes, when the stream ended early
	err    error
	closed bool
}

func (self *dnsSnapshotStream) Snapshot() dnsservice.DnsSnapshot {
	return self.snapshot
}

// The next row, io.EOF after the last, or why the stream ended early.
func (self *dnsSnapshotStream) Next() (*dnsservice.DnsRecordSet, error) {
	if self.closed {
		return nil, io.EOF
	}
	set, ok := <-self.rows
	if ok {
		return set, nil
	}
	<-self.done
	if self.err != nil {
		return nil, self.err
	}
	return nil, io.EOF
}

// Ends the transaction and waits for its goroutine. Safe more than once.
func (self *dnsSnapshotStream) Close() error {
	if self.closed {
		return nil
	}
	self.closed = true
	self.cancel()
	// drain, so the goroutine is never parked on a full channel
	for range self.rows {
	}
	<-self.done
	return nil
}
