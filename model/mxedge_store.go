package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"

	"github.com/urnetwork/server"
)

// mxedge's store on Postgres (§15.15; MAIL-M7-21): sidecar's
// mxedgeservice.MxEdgeStore over mail_connection_record. The engine calls it
// only from its probe and its 10 s batch goroutine, never from a session, so
// no session waits on Postgres (I-12), and a binary that does not link MG7
// answers every call with an error, which leaves the engine not ready. Safe
// for concurrent use.

var _ mxedgeservice.MxEdgeStore = (*MxEdgeStore)(nil)

// Tunables of the store.
type MxEdgeStoreSettings struct {
	// The longest one call may take when its caller gave no deadline, beside
	// the engine's own StoreTimeout.
	CallTimeout time.Duration
}

func DefaultMxEdgeStoreSettings() *MxEdgeStoreSettings {
	return &MxEdgeStoreSettings{
		CallTimeout: 10 * time.Second,
	}
}

type MxEdgeStore struct {
	settings *MxEdgeStoreSettings
}

// The store an mxedge host passes NewMxEdgeService.
func NewMxEdgeStore(settings *MxEdgeStoreSettings) *MxEdgeStore {
	if settings == nil {
		settings = DefaultMxEdgeStoreSettings()
	}
	return &MxEdgeStore{settings: settings}
}

// A context that ends at the caller's deadline or CallTimeout, whichever is
// first.
func (self *MxEdgeStore) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, self.settings.CallTimeout)
}

// Runs callback and returns what it raised, as an error.
func mxEdgeStoreRun(callback func()) (err error) {
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

// Whether the table answers: the binary links MG7 and mail_connection_record
// is a partitioned table in this database.
func (self *MxEdgeStore) Ping(ctx context.Context) error {
	if !server.SidecarMg7Linked() {
		return ErrMailUnlinked
	}
	callCtx, cancel := self.callContext(ctx)
	defer cancel()
	var partitioned bool
	err := mxEdgeStoreRun(func() {
		partitioned = mailConnectionTablePartitioned(callCtx)
	})
	if err != nil {
		return err
	}
	if !partitioned {
		return errors.New("mail_connection_record is not a partitioned table")
	}
	return nil
}

// One batch of records, stored by WriteMailConnectionRecords. A record no row
// can hold, or one of an hour already retired, is dropped there and counted,
// and is not an error here: the batch's other records are stored.
func (self *MxEdgeStore) InsertConnectionRecords(ctx context.Context, records []*mxedgeservice.MxConnectionRecord) error {
	if !server.SidecarMg7Linked() {
		return ErrMailUnlinked
	}
	callCtx, cancel := self.callContext(ctx)
	defer cancel()
	rows := make([]*MailConnectionRecord, 0, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}
		rows = append(rows, &MailConnectionRecord{
			NetworkId: server.Id(record.NetworkId),
			StartTime: record.StartTime,
			ByteCount: record.ByteCount,
			Result:    record.Result,
		})
	}
	var writeErr error
	err := mxEdgeStoreRun(func() {
		_, writeErr = WriteMailConnectionRecords(callCtx, rows)
	})
	if err != nil {
		return err
	}
	return writeErr
}

// What sidecartest.RunMxEdgeStoreSuite reads back on Postgres: one network's
// records from since on, as the engine's type.
func Testing_ReadMxConnectionRecords(ctx context.Context, networkId sidecar.Id, since time.Time) []*mxedgeservice.MxConnectionRecord {
	records := []*mxedgeservice.MxConnectionRecord{}
	for _, record := range GetMailConnectionRecords(ctx, server.Id(networkId), since, since.AddDate(1, 0, 0)) {
		records = append(records, &mxedgeservice.MxConnectionRecord{
			NetworkId: sidecar.Id(record.NetworkId),
			StartTime: record.StartTime,
			ByteCount: record.ByteCount,
			Result:    record.Result,
		})
	}
	return records
}
