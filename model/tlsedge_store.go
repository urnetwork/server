package model

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
)

// tlsedge's store on Postgres (§13.5, §13.9; TLS-M5-11, on the answer to
// plan-TLS-edge-data-source): sidecar's tlsedgeservice.TlsEdgeStore over
// DNS-M5-10's label map, network_tls_suspension and tls_connection_record.
// The engine calls it only from its names and kills loops and its 10 s record
// batches, never from a connection, so no visitor waits on Postgres (I-12).
// A binary that does not link the migrations these tables come from answers
// every call with an error, which leaves the engine not ready: it never
// serves on an empty map. Safe for concurrent use.

var _ tlsedgeservice.TlsEdgeStore = (*TlsEdgeStore)(nil)

// Tunables of the store.
type TlsEdgeStoreSettings struct {
	// The longest one call may take when its caller gave no sooner deadline.
	CallTimeout time.Duration
}

func DefaultTlsEdgeStoreSettings() *TlsEdgeStoreSettings {
	return &TlsEdgeStoreSettings{
		CallTimeout: 10 * time.Second,
	}
}

type TlsEdgeStore struct {
	settings *TlsEdgeStoreSettings
}

// The store a tlsedge host passes NewTlsEdgeService.
func NewTlsEdgeStore(settings *TlsEdgeStoreSettings) *TlsEdgeStore {
	if settings == nil {
		settings = DefaultTlsEdgeStoreSettings()
	}
	return &TlsEdgeStore{settings: settings}
}

// A context that ends at the caller's deadline or CallTimeout, whichever is
// first.
func (self *TlsEdgeStore) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, self.settings.CallTimeout)
}

// Runs callback and returns what it raised, as an error.
func tlsEdgeStoreRun(callback func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			if recoveredErr, ok := r.(error); ok {
				err = recoveredErr
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	callback()
	return
}

// The hosts of name's zone in root that owners withholds, relative to its
// apex, as SuspendHost takes them: shop for shop.<name>.<root>. An owner that
// is the apex itself is a zone suspension's marker, which the entry's
// ZoneSuspended carries, and an owner under another of the network's names is
// that name's.
func tlsEdgeWithheldHosts(name string, root string, owners []string) []string {
	hosts := []string{}
	suffix := "." + name + "." + root
	for _, owner := range owners {
		if host, ok := strings.CutSuffix(owner, suffix); ok && host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// The label map as the engine routes by it.
func tlsEdgeNameMap(labelMap *DnsLabelMap) *tlsedgeservice.TlsEdgeNameMap {
	nameMap := &tlsedgeservice.TlsEdgeNameMap{
		Serial:        labelMap.Serial,
		RestoreSerial: labelMap.RestoreSerial,
		Entries:       make([]*tlsedgeservice.TlsEdgeNameEntry, 0, len(labelMap.Entries)),
	}
	for _, entry := range labelMap.Entries {
		nameEntry := &tlsedgeservice.TlsEdgeNameEntry{
			Name:          entry.Label,
			Kind:          tlsedgeservice.TlsEdgeNameKind(entry.Kind),
			State:         tlsedgeservice.TlsEdgeNameState(entry.State),
			ZoneSuspended: entry.SuspendReason != "",
			WithheldHosts: tlsEdgeWithheldHosts(entry.Label, entry.Root, entry.WithheldHosts),
		}
		if entry.NetworkId != nil {
			nameEntry.NetworkId = sidecar.Id(*entry.NetworkId)
		}
		if nameEntry.ZoneSuspended && entry.SuspendUntil != nil {
			nameEntry.ZoneSuspendUntil = entry.SuspendUntil.UTC()
		}
		nameMap.Entries = append(nameMap.Entries, nameEntry)
	}
	return nameMap
}

// Reads the label map with read and converts it.
func (self *TlsEdgeStore) names(ctx context.Context, read func(ctx context.Context) (*DnsLabelMap, error)) (*tlsedgeservice.TlsEdgeNameMap, error) {
	callCtx, cancel := self.callContext(ctx)
	defer cancel()
	var labelMap *DnsLabelMap
	var readErr error
	err := tlsEdgeStoreRun(func() {
		labelMap, readErr = read(callCtx)
	})
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	return tlsEdgeNameMap(labelMap), nil
}

// The whole label map of root: DnsLabelMapSnapshot.
func (self *TlsEdgeStore) NameSnapshot(ctx context.Context, root string) (*tlsedgeservice.TlsEdgeNameMap, error) {
	return self.names(ctx, func(ctx context.Context) (*DnsLabelMap, error) {
		return DnsLabelMapSnapshot(ctx, root)
	})
}

// The names of root whose entry changed above after: DnsLabelMapSince.
func (self *TlsEdgeStore) NamesSince(ctx context.Context, root string, after uint64) (*tlsedgeservice.TlsEdgeNameMap, error) {
	return self.names(ctx, func(ctx context.Context) (*DnsLabelMap, error) {
		return DnsLabelMapSince(ctx, root, after)
	})
}

// Every row of network_tls_suspension: GetTlsSuspensions.
func (self *TlsEdgeStore) Suspensions(ctx context.Context) ([]*sidecar.TlsSuspendArgs, error) {
	callCtx, cancel := self.callContext(ctx)
	defer cancel()
	var suspensions []*sidecar.TlsSuspendArgs
	var readErr error
	err := tlsEdgeStoreRun(func() {
		suspensions, readErr = GetTlsSuspensions(callCtx)
	})
	if err != nil {
		return nil, err
	}
	return suspensions, readErr
}

// One batch of records, stored by WriteTlsConnectionRecords. A record no row
// can hold, or one of an hour already retired, is dropped there and counted,
// and is not an error here: the batch's other records are stored.
func (self *TlsEdgeStore) InsertConnectionRecords(ctx context.Context, records []*tlsedgeservice.TlsConnectionRecord) error {
	callCtx, cancel := self.callContext(ctx)
	defer cancel()
	rows := make([]*TlsConnectionRecord, 0, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}
		rows = append(rows, &TlsConnectionRecord{
			ConnectionId: server.Id(record.ConnectionId),
			NetworkId:    server.Id(record.NetworkId),
			Host:         record.Host,
			StartTime:    record.StartTime,
			EndTime:      record.EndTime,
			ByteCount:    record.ByteCount,
			VisitorHash:  record.VisitorHash,
		})
	}
	var writeErr error
	err := tlsEdgeStoreRun(func() {
		_, writeErr = WriteTlsConnectionRecords(callCtx, rows)
	})
	if err != nil {
		return err
	}
	return writeErr
}

// What sidecartest.RunTlsEdgeStoreSuite reads back on Postgres: one network's
// records ending at or after since, as the engine's type.
func Testing_ReadTlsConnectionRecords(ctx context.Context, networkId sidecar.Id, since time.Time) []*tlsedgeservice.TlsConnectionRecord {
	records := []*tlsedgeservice.TlsConnectionRecord{}
	server.Db(ctx, func(conn server.PgConn) {
		records = []*tlsedgeservice.TlsConnectionRecord{}
		result, err := conn.Query(
			ctx,
			`
				SELECT connection_id, host, start_time, end_time, byte_count, visitor_hash
				FROM tls_connection_record
				WHERE network_id = $1 AND $2 <= end_time
				ORDER BY end_time, connection_id
			`,
			server.Id(networkId),
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var connectionId server.Id
				record := &tlsedgeservice.TlsConnectionRecord{NetworkId: networkId}
				server.Raise(result.Scan(&connectionId, &record.Host, &record.StartTime, &record.EndTime, &record.ByteCount, &record.VisitorHash))
				record.ConnectionId = sidecar.Id(connectionId)
				record.StartTime = record.StartTime.UTC()
				record.EndTime = record.EndTime.UTC()
				records = append(records, record)
			}
		})
	})
	return records
}
