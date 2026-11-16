package model

// §21.9's copied_rows (ABUSE-M5-15; R-ABUSE-5, D-ABUSE-6, I-13, I-18): what drops or is overwritten on its own,
// copied once per hold into its parts 1 and up (plan-ABUSE-metadata-rows #3), so a hold keeps what existed at its
// placement without holding back anyone's partitions for longer than its copy takes.
//
//	rows        each registered table's rows of the held network whose time column precedes the placement, read
//	            by a cursor over (time, key) and appended as gzip-compressed parts of at most PartMaxByteCount,
//	            16 MiB, each value in its text form: tls-3's tls_connection_record here; ABUSE-M7-01's
//	            mail_connection_record and ABUSE-M9-01's smtp_send register theirs
//	soft state  evs-8's {evsl_<repo_id>} lease fields and gkm-10's acks of the network's repos in the hold's
//	            scope, read once from the process's AbuseHoldSoftStateSource; a missing key is no error, what
//	            existed is recorded, and a copy with no source says so in its part
//
// A part's copied_rows is u8(1) || u32 big-endian header length || its header's JSON (HoldCopyPartHeader) || one
// gzip stream of JSON lines, a row's text values or a key's fields each. The header stays outside the stream, so a
// copy reads where it stands from the first bytes of each part alone. It carries the case whose copy wrote the part
// and, for rows, the table and the cursor after its last row, so a copy cut short resumes from its own parts with
// no row twice, and a part that moves to another hold keeps saying where it came from. Once every table is copied,
// part 0's copy_state reads copied.
//
// Only rows existing at placement are copied: a row written later is new collection, which a hold never makes. A
// keyed visitor hash copies as it is, and nothing unhashes it (I-13). copied_rows is never served to the network;
// it is excepted from mx-2's, evs-8's and gkm-10's ttls, its life being its hold's. While a hold placed after a
// range began has not finished copying, AbuseHoldCopyPending keeps that range of a table the hold's network has
// rows in from before the placement, so a stuck copy keeps its own ranges, never another's, and shows up as a
// warning in the range's retirement, never as a lost row. When a hold ends, its parts move to a live hold of the
// network placed after it, which they existed at the placement of; otherwise they go with it.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// One table whose rows drop or are overwritten on their own, which a hold copies.
type HoldCopyTable struct {
	// the table, in schema public
	Table string
	// the column naming the row's network
	NetworkColumn string
	// the timestamp column saying when a row was written, which the table partitions or expires on: a row whose
	// time precedes a hold's placement existed at it
	TimeColumn string
	// with TimeColumn, a column that orders the table's rows uniquely: the copy's cursor
	KeyColumn string
	// the columns a copy keeps, in order, each in its text form
	Columns []string
	// whether this binary holds the table: false copies nothing of it. nil is always.
	Linked func() bool
}

func (self *HoldCopyTable) linked() bool {
	return self.Linked == nil || self.Linked()
}

// The registered tables, by name.
var holdCopyTables = struct {
	stateLock sync.Mutex
	tables    map[string]*HoldCopyTable
}{
	tables: map[string]*HoldCopyTable{},
}

// Adds a table to what every hold copies, from its owner's init. A second registration of one table, or one
// missing a column, panics.
func RegisterHoldCopy(table *HoldCopyTable) {
	holdCopyTables.stateLock.Lock()
	defer holdCopyTables.stateLock.Unlock()
	if table.Table == "" || table.NetworkColumn == "" || table.TimeColumn == "" || table.KeyColumn == "" || len(table.Columns) == 0 {
		panic(fmt.Sprintf("hold copy of %q names no table or column", table.Table))
	}
	if _, taken := holdCopyTables.tables[table.Table]; taken {
		panic(fmt.Sprintf("two hold copies of %q", table.Table))
	}
	holdCopyTables.tables[table.Table] = table
}

// The registered table of a name, or nil.
func holdCopyTableOf(name string) *HoldCopyTable {
	holdCopyTables.stateLock.Lock()
	defer holdCopyTables.stateLock.Unlock()
	return holdCopyTables.tables[name]
}

// Every registered table, by name.
func holdCopyTablesSorted() []*HoldCopyTable {
	holdCopyTables.stateLock.Lock()
	defer holdCopyTables.stateLock.Unlock()
	tables := []*HoldCopyTable{}
	for _, table := range holdCopyTables.tables {
		tables = append(tables, table)
	}
	slices.SortFunc(tables, func(a *HoldCopyTable, b *HoldCopyTable) int {
		return strings.Compare(a.Table, b.Table)
	})
	return tables
}

// tls-3's rows, which drop whole at 24 h (§2.8's header): this task's registration.
func init() {
	RegisterHoldCopy(&HoldCopyTable{
		Table:         tlsConnectionRecordTable,
		NetworkColumn: "network_id",
		TimeColumn:    "end_time",
		KeyColumn:     "connection_id",
		Columns:       []string{"connection_id", "network_id", "host", "start_time", "end_time", "byte_count", "visitor_hash"},
		Linked:        server.SidecarMg5Linked,
	})
}

// Tunables of the copy.
type AbuseHoldCopySettings struct {
	// The most bytes one part's compressed rows may hold: 16 MiB (plan-ABUSE-metadata-rows #3), so a busy
	// network's rows fit §21.16's seven tables. A part over it is refused.
	PartMaxByteCount int
	// The uncompressed bytes past which a part is cut: PartMaxByteCount less deflate's worst-case growth with
	// room to spare, so no cut part can pass PartMaxByteCount.
	PartRawMaxByteCount int
	// The most bytes of a part's header: 4 KiB, past any table's columns and cursor. A format value: a copy and
	// whatever reads its parts share it.
	PartHeaderMaxByteCount int
	// The most rows one read of a table takes, by the cursor.
	ReadBatchRowCount int
	// How often AbuseHoldCopy runs: a minute, so a hold placed while it runs waits at most that for its copy, and
	// a placement kicks it at once.
	PassInterval time.Duration
	// The most pending copies one read of the pass takes.
	PassBatchSize int
	// One pass's bound; a copy cut short resumes from its parts in the next.
	PassMaxTime time.Duration
	// The task runner's bound on one pass, past PassMaxTime.
	PassTaskMaxTime time.Duration
}

func DefaultAbuseHoldCopySettings() *AbuseHoldCopySettings {
	return &AbuseHoldCopySettings{
		PartMaxByteCount:       16 * 1024 * 1024,
		PartRawMaxByteCount:    15 * 1024 * 1024,
		PartHeaderMaxByteCount: 4 * 1024,
		ReadBatchRowCount:      1000,
		PassInterval:           time.Minute,
		PassBatchSize:          100,
		PassMaxTime:            5 * time.Minute,
		PassTaskMaxTime:        10 * time.Minute,
	}
}

// A part's kinds.
const (
	HoldCopyPartKindRows      = "rows"
	HoldCopyPartKindSoftState = "soft_state"
)

// The part format's version, its header's first key.
const holdCopyPartVersion = 1

// A part's first line.
type HoldCopyPartHeader struct {
	Version int `json:"version"`
	// the case whose copy wrote the part, kept when the part moves to another hold
	CaseId server.Id `json:"case_id"`
	Kind   string    `json:"kind"`
	// rows: the table and its columns, in each row's order
	Table   string   `json:"table,omitempty"`
	Columns []string `json:"columns,omitempty"`
	// rows: the cursor after the part's last row, its time and key in their text forms
	LastTime string `json:"last_time,omitempty"`
	LastKey  string `json:"last_key,omitempty"`
	// rows: the table's last part
	Complete bool `json:"complete,omitempty"`
	// soft state: the repo whose keys these are
	RepoId *server.Id `json:"repo_id,omitempty"`
	// soft state no source could read, recorded so no one reads its absence as none existing
	Unavailable bool `json:"unavailable,omitempty"`
	RowCount    int  `json:"row_count"`
}

// A part over its bound, or bytes no copy wrote.
var ErrHoldCopyPartInvalid = errors.New("hold copy part invalid")

// The bytes before a part's header: its version and the header's length.
const holdCopyPartPrefixByteCount = 5

// One part's bytes (see the file header), refused past PartHeaderMaxByteCount or PartMaxByteCount.
func encodeHoldCopyPart(header *HoldCopyPartHeader, lines [][]byte, settings *AbuseHoldCopySettings) ([]byte, error) {
	headerJson, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	if settings.PartHeaderMaxByteCount < len(headerJson) {
		return nil, fmt.Errorf("%w: a header of %d bytes", ErrHoldCopyPartInvalid, len(headerJson))
	}
	compressed := &bytes.Buffer{}
	writer := gzip.NewWriter(compressed)
	for _, line := range lines {
		writer.Write(line)
		writer.Write([]byte("\n"))
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if settings.PartMaxByteCount < compressed.Len() {
		return nil, fmt.Errorf("%w: %d compressed bytes", ErrHoldCopyPartInvalid, compressed.Len())
	}
	body := make([]byte, 0, holdCopyPartPrefixByteCount+len(headerJson)+compressed.Len())
	body = append(body, holdCopyPartVersion)
	body = binary.BigEndian.AppendUint32(body, uint32(len(headerJson)))
	body = append(body, headerJson...)
	body = append(body, compressed.Bytes()...)
	return body, nil
}

// A part's header, from its first bytes, and where its stream starts.
func readHoldCopyPartHeader(prefix []byte, settings *AbuseHoldCopySettings) (*HoldCopyPartHeader, int, error) {
	if len(prefix) < holdCopyPartPrefixByteCount || prefix[0] != holdCopyPartVersion {
		return nil, 0, fmt.Errorf("%w: no prefix", ErrHoldCopyPartInvalid)
	}
	headerByteCount := int(binary.BigEndian.Uint32(prefix[1:holdCopyPartPrefixByteCount]))
	if settings.PartHeaderMaxByteCount < headerByteCount || len(prefix) < holdCopyPartPrefixByteCount+headerByteCount {
		return nil, 0, fmt.Errorf("%w: a header of %d bytes", ErrHoldCopyPartInvalid, headerByteCount)
	}
	header := &HoldCopyPartHeader{}
	if err := json.Unmarshal(prefix[holdCopyPartPrefixByteCount:holdCopyPartPrefixByteCount+headerByteCount], header); err != nil || header.Version != holdCopyPartVersion {
		return nil, 0, fmt.Errorf("%w: header", ErrHoldCopyPartInvalid)
	}
	return header, holdCopyPartPrefixByteCount + headerByteCount, nil
}

// A part's header and lines, its stream read within PartRawMaxByteCount uncompressed bytes and a little more:
// what counsel's export and a test read back.
func ReadHoldCopyPart(body []byte, settings *AbuseHoldCopySettings) (*HoldCopyPartHeader, [][]byte, error) {
	header, streamStart, err := readHoldCopyPartHeader(body, settings)
	if err != nil {
		return nil, nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(body[streamStart:]))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: not gzip", ErrHoldCopyPartInvalid)
	}
	maxRawByteCount := settings.PartMaxByteCount
	raw, err := io.ReadAll(io.LimitReader(reader, int64(maxRawByteCount)+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrHoldCopyPartInvalid, err)
	}
	if maxRawByteCount < len(raw) {
		return nil, nil, fmt.Errorf("%w: past %d bytes", ErrHoldCopyPartInvalid, maxRawByteCount)
	}
	lines := [][]byte{}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if 0 < len(line) {
			lines = append(lines, line)
		}
	}
	return header, lines, nil
}

// One key of a repo's soft state as it existed: its name and fields.
type AbuseHoldSoftStateKey struct {
	Key    string            `json:"key"`
	Fields map[string]string `json:"fields"`
}

// Where a copy reads the Redis soft state existing at placement: the network's repos, and each repo's lease fields
// and its group's acks. A key that does not exist is left out, never an error.
type AbuseHoldSoftStateSource interface {
	NetworkRepoIds(ctx context.Context, networkId server.Id) ([]server.Id, error)
	RepoSoftState(ctx context.Context, repoId server.Id) ([]*AbuseHoldSoftStateKey, error)
}

// This process's soft state source, which taskworker installs with its evs host's Redis and projection reads.
// Until then a copy records its soft state as unavailable.
var abuseHoldSoftStateSource atomic.Pointer[AbuseHoldSoftStateSource]

// Installs this process's soft state source; nil removes it.
func SetAbuseHoldSoftStateSource(source AbuseHoldSoftStateSource) {
	if source == nil {
		abuseHoldSoftStateSource.Store(nil)
		return
	}
	abuseHoldSoftStateSource.Store(&source)
}

func currentAbuseHoldSoftStateSource() AbuseHoldSoftStateSource {
	if source := abuseHoldSoftStateSource.Load(); source != nil {
		return *source
	}
	return nil
}

// One part as the table holds it.
type holdCopyPart struct {
	part   int
	header *HoldCopyPartHeader
}

// The hold's parts' headers, in part order, and its highest part number, each read from its part's first bytes.
func readHoldCopyParts(ctx context.Context, conn server.PgCanQuery, networkId server.Id, caseId server.Id, settings *AbuseHoldCopySettings) (parts []*holdCopyPart, maxPart int) {
	result, err := conn.Query(
		ctx,
		`
			SELECT part, substring(copied_rows FROM 1 FOR $3)
			FROM abuse_legal_hold
			WHERE network_id = $1 AND case_id = $2 AND part > 0
			ORDER BY part
		`,
		networkId,
		caseId,
		holdCopyPartPrefixByteCount+settings.PartHeaderMaxByteCount,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var part int
			var prefix []byte
			server.Raise(result.Scan(&part, &prefix))
			maxPart = max(maxPart, part)
			header, _, err := readHoldCopyPartHeader(prefix, settings)
			if err != nil {
				// a part no copy wrote is kept and read as nothing's progress
				continue
			}
			parts = append(parts, &holdCopyPart{part: part, header: header})
		}
	})
	return
}

// Appends one part after the hold's highest, in its own transaction. A part number another writer took first
// refuses with no write, which the copy's next run resumes past.
func appendHoldCopyPart(ctx context.Context, networkId server.Id, caseId server.Id, part int, body []byte) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO abuse_legal_hold (network_id, case_id, part, copied_rows)
				VALUES ($1, $2, $3, $4)
			`,
			networkId,
			caseId,
			part,
			body,
		))
	})
}

// Called before each part's write with the table or kind it holds: how a test ends a copy midway, as a crash
// would. nil in production.
var Testing_HoldCopyBeforePart func(what string)

// A table's progress in one hold's copy: done, or the cursor its next read starts after.
type holdCopyTableProgress struct {
	complete bool
	lastTime string
	lastKey  string
}

// Copies what the hold keeps that drops on its own (see the file header), resuming from its own parts, until done
// or deadline. complete is true once part 0 reads copied. A table that fails to read is left for the next run and
// the others go on; the copy completes only once every linked table has. A hold not there, past its expiry or
// copied already is complete with nothing to do.
func RunAbuseHoldCopy(ctx context.Context, networkId server.Id, caseId server.Id, now time.Time, deadline time.Time, settings *AbuseHoldCopySettings) (complete bool, returnErr error) {
	if !server.SidecarMg5Linked() {
		return true, nil
	}
	hold := GetLegalHold(ctx, networkId, caseId)
	if hold == nil || !hold.Live(now) || hold.CopyState == LegalHoldCopyStateCopied {
		return true, nil
	}
	var parts []*holdCopyPart
	var maxPart int
	server.Db(ctx, func(conn server.PgConn) {
		parts, maxPart = readHoldCopyParts(ctx, conn, networkId, caseId, settings)
	})
	softStateDone := false
	tableProgress := map[string]*holdCopyTableProgress{}
	for _, part := range parts {
		if part.header.CaseId != caseId {
			// moved here from an ended hold: not this copy's progress
			continue
		}
		switch part.header.Kind {
		case HoldCopyPartKindSoftState:
			softStateDone = true
		case HoldCopyPartKindRows:
			tableProgress[part.header.Table] = &holdCopyTableProgress{
				complete: part.header.Complete,
				lastTime: part.header.LastTime,
				lastKey:  part.header.LastKey,
			}
		}
	}
	nextPart := func() int {
		maxPart += 1
		return maxPart
	}

	if !softStateDone {
		if err := copyHoldSoftState(ctx, hold, settings, nextPart); err != nil {
			return false, err
		}
	}

	var tableErr error
	for _, table := range holdCopyTablesSorted() {
		if !table.linked() {
			continue
		}
		progress := tableProgress[table.Table]
		if progress != nil && progress.complete {
			continue
		}
		if progress == nil {
			progress = &holdCopyTableProgress{}
		}
		done, err := copyHoldTable(ctx, hold, table, progress, deadline, settings, nextPart)
		if err != nil {
			// this table waits for the next run; the others go on
			tableErr = errors.Join(tableErr, fmt.Errorf("%s: %w", table.Table, err))
			continue
		}
		if !done {
			return false, tableErr
		}
	}
	if tableErr != nil {
		return false, tableErr
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE abuse_legal_hold
				SET copy_state = $3
				WHERE network_id = $1 AND case_id = $2 AND part = 0
			`,
			networkId,
			caseId,
			string(LegalHoldCopyStateCopied),
		))
	})
	return true, nil
}

// The soft state of the hold's repos in scope, one part per repo with any key, or one part saying no source could
// read it.
func copyHoldSoftState(ctx context.Context, hold *LegalHold, settings *AbuseHoldCopySettings, nextPart func() int) error {
	source := currentAbuseHoldSoftStateSource()
	if source == nil {
		body, err := encodeHoldCopyPart(&HoldCopyPartHeader{
			Version:     holdCopyPartVersion,
			CaseId:      hold.CaseId,
			Kind:        HoldCopyPartKindSoftState,
			Unavailable: true,
		}, nil, settings)
		if err != nil {
			return err
		}
		if hook := Testing_HoldCopyBeforePart; hook != nil {
			hook(HoldCopyPartKindSoftState)
		}
		appendHoldCopyPart(ctx, hold.NetworkId, hold.CaseId, nextPart(), body)
		return nil
	}
	repoIds, err := source.NetworkRepoIds(ctx, hold.NetworkId)
	if err != nil {
		return err
	}
	type repoKeys struct {
		repoId server.Id
		lines  [][]byte
	}
	repos := []*repoKeys{}
	for _, repoId := range repoIds {
		if !hold.Scope.Covers(sidecar.Id(repoId)) {
			continue
		}
		keys, err := source.RepoSoftState(ctx, repoId)
		if err != nil {
			return err
		}
		lines := [][]byte{}
		for _, key := range keys {
			line, err := json.Marshal(key)
			if err != nil {
				return err
			}
			lines = append(lines, line)
		}
		repos = append(repos, &repoKeys{repoId: repoId, lines: lines})
	}
	// every repo read before any part is written, so a failed read leaves no partial record
	written := 0
	for _, repo := range repos {
		if len(repo.lines) == 0 {
			continue
		}
		repoId := repo.repoId
		body, err := encodeHoldCopyPart(&HoldCopyPartHeader{
			Version:  holdCopyPartVersion,
			CaseId:   hold.CaseId,
			Kind:     HoldCopyPartKindSoftState,
			RepoId:   &repoId,
			RowCount: len(repo.lines),
		}, repo.lines, settings)
		if err != nil {
			return err
		}
		if hook := Testing_HoldCopyBeforePart; hook != nil {
			hook(HoldCopyPartKindSoftState)
		}
		appendHoldCopyPart(ctx, hold.NetworkId, hold.CaseId, nextPart(), body)
		written += 1
	}
	if written == 0 {
		// a part all the same, so the copy knows its soft state is read: nothing existed
		body, err := encodeHoldCopyPart(&HoldCopyPartHeader{
			Version: holdCopyPartVersion,
			CaseId:  hold.CaseId,
			Kind:    HoldCopyPartKindSoftState,
		}, nil, settings)
		if err != nil {
			return err
		}
		if hook := Testing_HoldCopyBeforePart; hook != nil {
			hook(HoldCopyPartKindSoftState)
		}
		appendHoldCopyPart(ctx, hold.NetworkId, hold.CaseId, nextPart(), body)
	}
	return nil
}

// The cursor's floor: before every row.
const holdCopyCursorFloorTime = "0001-01-01 00:00:00"

// Copies one table's rows of the hold's network from before its placement, from progress's cursor, as parts. done
// is false when deadline came first, with the cursor in the last part written.
func copyHoldTable(
	ctx context.Context,
	hold *LegalHold,
	table *HoldCopyTable,
	progress *holdCopyTableProgress,
	deadline time.Time,
	settings *AbuseHoldCopySettings,
	nextPart func() int,
) (done bool, returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			returnErr = fmt.Errorf("%v", r)
		}
	}()
	selected := []string{}
	for _, column := range table.Columns {
		selected = append(selected, fmt.Sprintf("copied.%s::text", pgx.Identifier{column}.Sanitize()))
	}
	timeColumn := "copied." + pgx.Identifier{table.TimeColumn}.Sanitize()
	keyColumn := "copied." + pgx.Identifier{table.KeyColumn}.Sanitize()
	sql := fmt.Sprintf(
		`
			SELECT %[1]s, %[2]s::text, %[3]s::text
			FROM public.%[4]s copied
			WHERE
				copied.%[5]s = $1 AND
				%[2]s < $2 AND
				(%[2]s, %[3]s::text) > ($3::timestamp, $4::text)
			ORDER BY %[2]s, %[3]s::text
			LIMIT $5
		`,
		strings.Join(selected, ", "),
		timeColumn,
		keyColumn,
		pgx.Identifier{table.Table}.Sanitize(),
		pgx.Identifier{table.NetworkColumn}.Sanitize(),
	)

	lastTime, lastKey := progress.lastTime, progress.lastKey
	if lastTime == "" {
		lastTime = holdCopyCursorFloorTime
	}
	lines := [][]byte{}
	rawByteCount := 0
	// whether this table has a part of this copy's, which then needs a last one saying it is complete
	hasPart := progress.lastTime != ""
	flush := func(complete bool) error {
		body, err := encodeHoldCopyPart(&HoldCopyPartHeader{
			Version:  holdCopyPartVersion,
			CaseId:   hold.CaseId,
			Kind:     HoldCopyPartKindRows,
			Table:    table.Table,
			Columns:  slices.Clone(table.Columns),
			LastTime: lastTime,
			LastKey:  lastKey,
			Complete: complete,
			RowCount: len(lines),
		}, lines, settings)
		if err != nil {
			return err
		}
		if hook := Testing_HoldCopyBeforePart; hook != nil {
			hook(table.Table)
		}
		appendHoldCopyPart(ctx, hold.NetworkId, hold.CaseId, nextPart(), body)
		lines, rawByteCount, hasPart = [][]byte{}, 0, true
		return nil
	}
	for {
		if !time.Now().Before(deadline) {
			if 0 < len(lines) {
				if err := flush(false); err != nil {
					return false, err
				}
			}
			return false, nil
		}
		readCount := 0
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, sql, hold.NetworkId, hold.PlaceTime.UTC(), lastTime, lastKey, settings.ReadBatchRowCount)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					values := make([]*string, len(table.Columns)+2)
					targets := make([]any, len(values))
					for i := range values {
						targets[i] = &values[i]
					}
					server.Raise(result.Scan(targets...))
					line, err := json.Marshal(values[:len(table.Columns)])
					server.Raise(err)
					lines = append(lines, line)
					rawByteCount += len(line) + 1
					lastTime, lastKey = *values[len(table.Columns)], *values[len(table.Columns)+1]
					readCount += 1
				}
			})
		})
		if settings.PartRawMaxByteCount <= rawByteCount {
			if err := flush(false); err != nil {
				return false, err
			}
		}
		if readCount < settings.ReadBatchRowCount {
			if 0 < len(lines) || hasPart {
				if err := flush(true); err != nil {
					return false, err
				}
			}
			return true, nil
		}
	}
}

// Where a pass over pending copies stands.
type HoldCopyCursor struct {
	NetworkId server.Id
	CaseId    server.Id
}

// Up to limit live holds whose copy is not complete, after cursor by network and case.
func ListPendingHoldCopies(ctx context.Context, now time.Time, cursor HoldCopyCursor, limit int) (holds []*LegalHold, next HoldCopyCursor) {
	holds = []*LegalHold{}
	next = cursor
	if !server.SidecarMg5Linked() || limit <= 0 {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+legalHoldColumns+`
				FROM abuse_legal_hold
				WHERE
					abuse_legal_hold.part = 0 AND
					(abuse_legal_hold.copy_state IS NULL OR abuse_legal_hold.copy_state <> $1) AND
					(abuse_legal_hold.expire_time IS NULL OR $2 < abuse_legal_hold.expire_time) AND
					(abuse_legal_hold.network_id, abuse_legal_hold.case_id) > ($3, $4)
				ORDER BY abuse_legal_hold.network_id, abuse_legal_hold.case_id
				LIMIT $5
			`,
			string(LegalHoldCopyStateCopied),
			now.UTC(),
			cursor.NetworkId,
			cursor.CaseId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				holds = append(holds, scanLegalHold(result))
			}
		})
	})
	if 0 < len(holds) {
		last := holds[len(holds)-1]
		next = HoldCopyCursor{NetworkId: last.NetworkId, CaseId: last.CaseId}
	}
	return
}

// What one pass of the copies did.
type AbuseHoldCopyPassResult struct {
	CompletedCount int
	// copies cut short by the pass's bound, resumed in the next
	PendingCount int
	// copies a table of which failed this time
	FailedCount int
}

// One pass over pending copies, oldest key first, until each is complete, PassMaxTime has passed or ctx ends.
func RunAbuseHoldCopies(ctx context.Context, now time.Time, settings *AbuseHoldCopySettings) *AbuseHoldCopyPassResult {
	result := &AbuseHoldCopyPassResult{}
	if !server.SidecarMg5Linked() {
		return result
	}
	deadline := time.Now().Add(settings.PassMaxTime)
	cursor := HoldCopyCursor{}
	for ctx.Err() == nil && time.Now().Before(deadline) {
		holds, next := ListPendingHoldCopies(ctx, now, cursor, settings.PassBatchSize)
		if len(holds) == 0 {
			return result
		}
		cursor = next
		for _, hold := range holds {
			if ctx.Err() != nil || !time.Now().Before(deadline) {
				result.PendingCount += 1
				continue
			}
			complete, err := RunAbuseHoldCopy(ctx, hold.NetworkId, hold.CaseId, now, deadline, settings)
			switch {
			case err != nil:
				result.FailedCount += 1
			case complete:
				result.CompletedCount += 1
			default:
				result.PendingCount += 1
			}
		}
	}
	return result
}

// The copy pass's run-once key, which a placement kicks.
const AbuseHoldCopyRunOnceKey = "abuse_hold_copy"

// Pulls the pending copy pass to now: what a placement does once its hold stands, so its copy starts at once and
// the Redis soft state it reads is as close to the placement as it can be. A kick that fails changes nothing the
// pass's own interval does not bound.
func KickAbuseHoldCopy(ctx context.Context) {
	server.HandleError(func() {
		task.KickTasks(ctx, AbuseHoldCopyRunOnceKey)
	})
}

// Why range r of table, whose leaf is leaf, must stay (PartitionFamily.DropPrecondition's answer), or "" when it
// may drop: a live hold placed after r began, whose copy is not complete, whose network has a row in leaf from
// before the placement. A part 0 with no placement, expiry or copy state reads as live, covering and not copied,
// since a hold that cannot say what it spares spares nothing. A table no copy is registered for keeps the range
// while such a hold lives, since no copy will ever take its rows. leaf may be attached or, inside the drop's
// transaction, just detached, so the answer holds across the detach. An open range is never judged droppable.
func AbuseHoldCopyPending(ctx context.Context, conn server.PgQuerier, table string, leaf string, r server.PartitionRange, now time.Time) string {
	if r.Lower.IsZero() {
		return fmt.Sprintf("%s has no lower bound to judge holds against", leaf)
	}
	if !server.SidecarMg5Linked() {
		return ""
	}
	registered := holdCopyTableOf(table)
	if registered == nil {
		// no copy will take this table's rows: any live hold placed after r began keeps it
		return unregisteredHoldCopyPending(ctx, conn, table, leaf, r, now)
	}
	copiedClause := `(hold.copy_state IS NULL OR hold.copy_state <> $3)`
	var caseId server.Id
	var found bool
	result, err := conn.Query(
		ctx,
		fmt.Sprintf(
			`
			SELECT hold.case_id
			FROM abuse_legal_hold hold
			WHERE
				hold.part = 0 AND
				(hold.place_time IS NULL OR $1 < hold.place_time) AND
				(hold.expire_time IS NULL OR $2 < hold.expire_time) AND
				%[4]s AND
				EXISTS (
					SELECT 1
					FROM public.%[1]s record
					WHERE
						record.%[2]s = hold.network_id AND
						(hold.place_time IS NULL OR record.%[3]s < hold.place_time)
				)
			ORDER BY hold.case_id
			LIMIT 1
			`,
			pgx.Identifier{leaf}.Sanitize(),
			pgx.Identifier{registered.NetworkColumn}.Sanitize(),
			pgx.Identifier{registered.TimeColumn}.Sanitize(),
			copiedClause,
		),
		r.Lower.UTC(),
		now.UTC(),
		string(LegalHoldCopyStateCopied),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&caseId))
			found = true
		}
	})
	if !found {
		return ""
	}
	// the case id is an opaque id, which a log line may carry (I-13)
	return fmt.Sprintf("hold of case %s has not copied its network's rows of %s", caseId, leaf)
}

// AbuseHoldCopyPending's answer for a table no copy is registered for, which knows neither its network nor its time
// column: a live hold placed after r began keeps the range, whichever network it holds. A caller that knows them
// judges more finely (connectionRecordRangeHold).
func unregisteredHoldCopyPending(ctx context.Context, conn server.PgQuerier, table string, leaf string, r server.PartitionRange, now time.Time) string {
	var caseId server.Id
	var found bool
	result, err := conn.Query(
		ctx,
		`
			SELECT hold.case_id
			FROM abuse_legal_hold hold
			WHERE
				hold.part = 0 AND
				(hold.place_time IS NULL OR $1 < hold.place_time) AND
				(hold.expire_time IS NULL OR $2 < hold.expire_time)
			ORDER BY hold.case_id
			LIMIT 1
		`,
		r.Lower.UTC(),
		now.UTC(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&caseId))
			found = true
		}
	})
	if !found {
		return ""
	}
	return fmt.Sprintf("hold of case %s stands and no copy is registered for %s, so %s stays", caseId, table, leaf)
}

// Whether a copy is registered for table.
func HoldCopyRegistered(table string) bool {
	return holdCopyTableOf(table) != nil
}

// Moves an ending hold's parts to the live hold of its network placed after it with the latest expiry, in the
// ending transaction: they existed at that hold's placement, so they are its to keep (§21.9's overlap). A soft
// state part moves only to a hold whose scope covers its repo; rows are server rows, which every hold of the
// network covers (plan-ABUSE-hold-format #1). What does not move goes with the hold.
func moveLegalHoldCopiesInTx(ctx context.Context, tx server.PgTx, hold *LegalHold, now time.Time) {
	var receiver *LegalHold
	result, err := tx.Query(
		ctx,
		`
			SELECT `+legalHoldColumns+`
			FROM abuse_legal_hold
			WHERE
				abuse_legal_hold.network_id = $1 AND
				abuse_legal_hold.case_id <> $2 AND
				abuse_legal_hold.part = 0 AND
				$3 < abuse_legal_hold.place_time AND
				(abuse_legal_hold.expire_time IS NULL OR $4 < abuse_legal_hold.expire_time)
			ORDER BY abuse_legal_hold.expire_time DESC NULLS FIRST, abuse_legal_hold.case_id
			LIMIT 1
			FOR UPDATE
		`,
		hold.NetworkId,
		hold.CaseId,
		hold.PlaceTime.UTC(),
		now.UTC(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			receiver = scanLegalHold(result)
		}
	})
	if receiver == nil {
		return
	}
	parts, _ := readHoldCopyParts(ctx, tx, hold.NetworkId, hold.CaseId, DefaultAbuseHoldCopySettings())
	var receiverMaxPart int
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT COALESCE(MAX(part), 0) FROM abuse_legal_hold WHERE network_id = $1 AND case_id = $2`,
		receiver.NetworkId,
		receiver.CaseId,
	).Scan(&receiverMaxPart))
	for _, part := range parts {
		if part.header.Kind == HoldCopyPartKindSoftState && part.header.RepoId != nil && !receiver.Scope.Covers(sidecar.Id(*part.header.RepoId)) {
			continue
		}
		receiverMaxPart += 1
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE abuse_legal_hold
				SET case_id = $4, part = $5
				WHERE network_id = $1 AND case_id = $2 AND part = $3
			`,
			hold.NetworkId,
			hold.CaseId,
			part.part,
			receiver.CaseId,
			receiverMaxPart,
		))
	}
}

func init() {
	endLegalHoldCopiesInTx = moveLegalHoldCopiesInTx
}
