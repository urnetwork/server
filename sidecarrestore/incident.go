// Package sidecarrestore is §19.10's restore frame (D-OPS-14, D-OPS-18, R-OPS-9; OPS-M3-03): the one place that fixes
// the order of a Postgres or site recovery's steps, runs each under its incident and keeps the incident's list.
//
// A recovery's identity is its incident id. Every step keys its work by it, so a step that runs again under the same
// incident changes nothing it already changed, and every step appends what it did to the incident's list,
// incident-<id>.tsv, the file xops's bringyour-pg-sidecar-restore.sh starts as it loads the sidecar dump. That script
// writes the list's head, both dumps' snapshot times, and marks the database it loaded with a comment naming the
// incident; the frame refuses a database without that mark, so no step runs over a live database by mistake.
//
// The list holds four columns, as the script's COPY writes them: kind, table, row id and detail, each in COPY's text
// escaping. Only ids, counts, codes and times go in it, never a name or an address (I-13), save the suffixed network
// name the script itself lists. A step's end is a row of its own, step-ended, which is how a later command learns where
// the incident stands; one operator drives an incident, and the file is appended a whole line at a time and synced.
package sidecarrestore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// The kinds of recovery §19.10 runs. A site loss runs every Postgres step and then its own.
type IncidentKind string

const (
	IncidentKindPostgres IncidentKind = "postgres"
	IncidentKindSite     IncidentKind = "site"
)

// Parses an incident kind as an operator types it.
func ParseIncidentKind(kind string) (IncidentKind, error) {
	switch IncidentKind(kind) {
	case IncidentKindPostgres, IncidentKindSite:
		return IncidentKind(kind), nil
	}
	return "", fmt.Errorf("%q is no incident kind: postgres or site", kind)
}

// The list's row kinds the frame reads or writes. A step's own rows use kinds of their own.
const (
	// both dumps' snapshot times, the script's head rows: table "sidecar dump" or "main dump", detail the time
	ListKindSnapshotTime = "snapshot-time"
	// the incident's kind, written once: table the kind
	ListKindIncidentKind = "incident-kind"
	// a step's end: table the step, detail when and how it ended
	ListKindStepEnded = "step-ended"
)

// The script's two snapshot rows' tables.
const (
	listTableSidecarDump = "sidecar dump"
	listTableMainDump    = "main dump"
)

// How a step ended, the start of its step-ended row's detail after the time.
const (
	stepEndedRan    = "ran"
	stepEndedByHand = "by-hand"
)

// The comment xops's restore script sets on the database it loaded, naming the incident.
const restoreMarkerFormat = "sidecar-restore incident=%s"

var (
	// The list is not one the script started for this incident.
	ErrIncidentList = errors.New("sidecar restore incident list invalid")
	// The database carries no restore marker, or another incident's.
	ErrIncidentMarker = errors.New("the database was not restored under this incident")
)

// How the frame reads an incident's list.
type IncidentSettings struct {
	// The longest line the list may hold, past which a read fails rather than guess. The script's lines and the
	// frame's are a few hundred bytes; 1 MiB leaves room for any id list a step writes on one line.
	ListLineMaxByteCount int
}

// The defaults.
func DefaultIncidentSettings() *IncidentSettings {
	return &IncidentSettings{
		ListLineMaxByteCount: 1024 * 1024,
	}
}

// One row of the list.
type ListRow struct {
	Kind   string
	Table  string
	RowId  string
	Detail string
}

// A recovery under way: its id, its list and what the list says so far. Safe for concurrent use by one process; one
// operator drives an incident.
type Incident struct {
	incidentId server.Id
	listPath   string

	stateLock sync.Mutex
	rows      []*ListRow
}

// Opens the list at listPath for incidentId: it must exist, as the script started it, and hold both snapshot times.
// The rows are read now; Append keeps them current. settings may be nil for the defaults.
func OpenIncident(incidentId server.Id, listPath string, settings *IncidentSettings) (*Incident, error) {
	if incidentId == (server.Id{}) {
		return nil, fmt.Errorf("%w: no incident id", ErrIncidentList)
	}
	if settings == nil {
		settings = DefaultIncidentSettings()
	}
	rows, err := readListRows(listPath, settings.ListLineMaxByteCount)
	if err != nil {
		return nil, err
	}
	incident := &Incident{incidentId: incidentId, listPath: listPath, rows: rows}
	if _, _, err := incident.SnapshotTimes(); err != nil {
		return nil, err
	}
	return incident, nil
}

// The incident's id.
func (self *Incident) IncidentId() server.Id {
	return self.incidentId
}

// Where its list is.
func (self *Incident) ListPath() string {
	return self.listPath
}

// A copy of the list's rows, in file order.
func (self *Incident) Rows() []*ListRow {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	rows := make([]*ListRow, 0, len(self.rows))
	for _, row := range self.rows {
		copied := *row
		rows = append(rows, &copied)
	}
	return rows
}

// The rows of one kind, in file order.
func (self *Incident) RowsOfKind(kind string) []*ListRow {
	rows := []*ListRow{}
	for _, row := range self.Rows() {
		if row.Kind == kind {
			rows = append(rows, row)
		}
	}
	return rows
}

// Both dumps' snapshot times, the sidecar dump's and the main dump's copy end, as the script's head rows give them.
func (self *Incident) SnapshotTimes() (sidecarTime time.Time, mainTime time.Time, returnErr error) {
	found := map[string]time.Time{}
	for _, row := range self.RowsOfKind(ListKindSnapshotTime) {
		parsed, err := time.Parse(time.RFC3339Nano, row.Detail)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("%w: the %s's snapshot time %q is no time", ErrIncidentList, row.Table, row.Detail)
		}
		if row.RowId != self.incidentId.String() {
			return time.Time{}, time.Time{}, fmt.Errorf("%w: the list's snapshot times are another incident's", ErrIncidentList)
		}
		found[row.Table] = parsed.UTC()
	}
	sidecarTime, sidecarOk := found[listTableSidecarDump]
	mainTime, mainOk := found[listTableMainDump]
	if !sidecarOk || !mainOk {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: the list does not open with both dumps' snapshot times, so the restore script did not finish under this incident", ErrIncidentList)
	}
	return sidecarTime, mainTime, nil
}

// The older of the two snapshot times, from which every later removal is re-applied (§19.10's api step).
func (self *Incident) OlderSnapshotTime() (time.Time, error) {
	sidecarTime, mainTime, err := self.SnapshotTimes()
	if err != nil {
		return time.Time{}, err
	}
	if sidecarTime.Before(mainTime) {
		return sidecarTime, nil
	}
	return mainTime, nil
}

// The kind the list records, false while none is recorded.
func (self *Incident) Kind() (IncidentKind, bool, error) {
	rows := self.RowsOfKind(ListKindIncidentKind)
	if len(rows) == 0 {
		return "", false, nil
	}
	kind, err := ParseIncidentKind(rows[0].Table)
	if err != nil {
		return "", false, fmt.Errorf("%w: %v", ErrIncidentList, err)
	}
	for _, row := range rows[1:] {
		if row.Table != rows[0].Table {
			return "", false, fmt.Errorf("%w: the list records two kinds", ErrIncidentList)
		}
	}
	return kind, true, nil
}

// Records the incident's kind once. A second kind is refused.
func (self *Incident) SetKind(kind IncidentKind) error {
	recorded, ok, err := self.Kind()
	if err != nil {
		return err
	}
	if ok {
		if recorded != kind {
			return fmt.Errorf("%w: the incident is a %s incident, not %s", ErrIncidentList, recorded, kind)
		}
		return nil
	}
	return self.Append(&ListRow{Kind: ListKindIncidentKind, Table: string(kind), RowId: self.incidentId.String(), Detail: "recorded"})
}

// Whether step's end is recorded.
func (self *Incident) StepEnded(step string) bool {
	for _, row := range self.RowsOfKind(ListKindStepEnded) {
		if row.Table == step {
			return true
		}
	}
	return false
}

// Records that step ended at endTime, how saying ran or by-hand with the operator's statement.
func (self *Incident) recordStepEnded(step string, endTime time.Time, how string) error {
	if self.StepEnded(step) {
		return nil
	}
	return self.Append(&ListRow{
		Kind:   ListKindStepEnded,
		Table:  step,
		RowId:  self.incidentId.String(),
		Detail: endTime.UTC().Format(time.RFC3339) + " " + how,
	})
}

// Appends row to the list, unless the list holds the same row already, so a step that runs again under the incident
// lists nothing twice. The line is written whole and synced before this returns.
func (self *Incident) Append(row *ListRow) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if slices.ContainsFunc(self.rows, func(held *ListRow) bool { return *held == *row }) {
		return nil
	}
	line := strings.Join([]string{
		escapeListField(row.Kind),
		escapeListField(row.Table),
		escapeListField(row.RowId),
		escapeListField(row.Detail),
	}, "\t") + "\n"
	file, err := os.OpenFile(self.listPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(line); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	copied := *row
	self.rows = append(self.rows, &copied)
	return nil
}

// Reads every row of the list at path, each line at most lineMaxByteCount bytes.
func readListRows(path string, lineMaxByteCount int) ([]*ListRow, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIncidentList, err)
	}
	defer file.Close()
	rows := []*ListRow{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, lineMaxByteCount)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber += 1
		line := scanner.Text()
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("%w: line %d holds %d fields, not 4", ErrIncidentList, lineNumber, len(fields))
		}
		row := &ListRow{}
		for index, field := range []*string{&row.Kind, &row.Table, &row.RowId, &row.Detail} {
			value, err := unescapeListField(fields[index])
			if err != nil {
				return nil, fmt.Errorf("%w: line %d: %v", ErrIncidentList, lineNumber, err)
			}
			*field = value
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIncidentList, err)
	}
	return rows, nil
}

// A value in COPY's text escaping, as the script's rows are.
func escapeListField(value string) string {
	var escaped strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			escaped.WriteString(`\\`)
		case '\t':
			escaped.WriteString(`\t`)
		case '\n':
			escaped.WriteString(`\n`)
		case '\r':
			escaped.WriteString(`\r`)
		default:
			escaped.WriteRune(r)
		}
	}
	return escaped.String()
}

// The value of a field in COPY's text escaping. COPY's octal and hex escapes do not occur in the script's rows, and
// are refused rather than guessed.
func unescapeListField(field string) (string, error) {
	if !strings.Contains(field, `\`) {
		return field, nil
	}
	var value strings.Builder
	for index := 0; index < len(field); index += 1 {
		if field[index] != '\\' {
			value.WriteByte(field[index])
			continue
		}
		if index+1 == len(field) {
			return "", errors.New("a field ends in a lone backslash")
		}
		index += 1
		switch field[index] {
		case '\\':
			value.WriteByte('\\')
		case 't':
			value.WriteByte('\t')
		case 'n':
			value.WriteByte('\n')
		case 'r':
			value.WriteByte('\r')
		default:
			return "", fmt.Errorf("an escape \\%c the list does not use", field[index])
		}
	}
	return value.String(), nil
}

// Refuses a database the restore script did not load under incidentId: the script's comment on the database, the
// one the env's pool reaches, must name it exactly.
func CheckRestoreMarker(ctx context.Context, incidentId server.Id) (returnErr error) {
	var comment string
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
					SELECT coalesce(pg_catalog.shobj_description(d.oid, 'pg_database'), '')
					FROM pg_catalog.pg_database AS d
					WHERE d.datname = current_database()
				`,
			).Scan(&comment))
		})
	}); r != nil {
		return fmt.Errorf("the restore marker could not be read: %v", r)
	}
	if comment != RestoreMarker(incidentId) {
		if comment == "" || !strings.HasPrefix(comment, "sidecar-restore ") {
			return fmt.Errorf("%w: it carries no restore marker, so it is no database bringyour-pg-sidecar-restore.sh loaded", ErrIncidentMarker)
		}
		return fmt.Errorf("%w: its marker names another incident", ErrIncidentMarker)
	}
	return nil
}

// The comment the restore script sets on a database it loaded under incidentId.
func RestoreMarker(incidentId server.Id) string {
	return fmt.Sprintf(restoreMarkerFormat, incidentId)
}
