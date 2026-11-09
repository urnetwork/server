package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// smtpedge's store on Postgres (§14.9; SMTP-M5-06, SMTP-M9-18): sidecar's
// smtpedgeservice.SmtpEdgeStore over §14.14's tables. The report calls,
// InsertComplaint and InsertAggregate, write MG5's report tables
// (smtp_report_model.go) for mxedge's platform block, where the report intake
// runs (plan-SMTP-report-intake-home). Every call is off the connection path
// (§14.9): an edge batches its sends and reloads its halts, and the intake
// writes before its listener answers a report's DATA, never inside a
// connection's packets (I-12). A binary that does not link a call's group
// answers it with that group's error, which leaves the engine not ready. Safe
// for concurrent use.

var _ smtpedgeservice.SmtpEdgeStore = (*SmtpEdgeStore)(nil)

// What the send, halt and DKIM key calls answer until MG9's tables land
// (SMTP-M9-18): a refusal, never an empty answer, so no edge reads it as no
// halts or no sends.
var ErrSmtpEdgeTablesNotBuilt = errors.New("smtp_send, smtp_halt and smtp_dkim_key are not built yet (SMTP-M9-18)")

// Tunables of the store.
type SmtpEdgeStoreSettings struct {
	// The longest one call may take when its caller gave no earlier deadline,
	// beside the engine's own StoreTimeout. Positive.
	CallTimeout time.Duration
}

func DefaultSmtpEdgeStoreSettings() *SmtpEdgeStoreSettings {
	return &SmtpEdgeStoreSettings{
		CallTimeout: 10 * time.Second,
	}
}

type SmtpEdgeStore struct {
	settings *SmtpEdgeStoreSettings
}

// The store a host passes the smtpedge engine or the report intake. nil
// settings take the defaults.
func NewSmtpEdgeStore(settings *SmtpEdgeStoreSettings) *SmtpEdgeStore {
	if settings == nil {
		settings = DefaultSmtpEdgeStoreSettings()
	}
	return &SmtpEdgeStore{settings: settings}
}

// A context that ends at the caller's deadline or CallTimeout, whichever is
// first.
func (self *SmtpEdgeStore) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, self.settings.CallTimeout)
}

// Runs callback and returns what it raised, as an error.
func smtpEdgeStoreRun(callback func()) (err error) {
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

// See smtpedgeservice.SmtpEdgeStore. Until SMTP-M9-18: ErrSmtpEdgeTablesNotBuilt.
func (self *SmtpEdgeStore) InsertSends(ctx context.Context, sends []*smtpedgeservice.SmtpSendRecord) error {
	return ErrSmtpEdgeTablesNotBuilt
}

// See smtpedgeservice.SmtpEdgeStore. Until SMTP-M9-18: ErrSmtpEdgeTablesNotBuilt.
func (self *SmtpEdgeStore) FindSends(ctx context.Context, key *smtpedgeservice.SmtpJoinKey) ([]*smtpedgeservice.SmtpSendRecord, error) {
	return nil, ErrSmtpEdgeTablesNotBuilt
}

// See smtpedgeservice.SmtpEdgeStore: InsertSmtpComplaint.
func (self *SmtpEdgeStore) InsertComplaint(ctx context.Context, complaint *smtpedgeservice.SmtpComplaint) error {
	if !server.SidecarMg5Linked() {
		return ErrSmtpReportUnlinked
	}
	callCtx, cancel := self.callContext(ctx)
	defer cancel()
	var insertErr error
	err := smtpEdgeStoreRun(func() {
		insertErr = InsertSmtpComplaint(callCtx, complaint)
	})
	if err != nil {
		return err
	}
	return insertErr
}

// See smtpedgeservice.SmtpEdgeStore: InsertSmtpAggregate.
func (self *SmtpEdgeStore) InsertAggregate(ctx context.Context, report *smtpedgeservice.SmtpAggregateReport) (bool, error) {
	if !server.SidecarMg5Linked() {
		return false, ErrSmtpReportUnlinked
	}
	callCtx, cancel := self.callContext(ctx)
	defer cancel()
	var inserted bool
	var insertErr error
	err := smtpEdgeStoreRun(func() {
		inserted, insertErr = InsertSmtpAggregate(callCtx, report)
	})
	if err != nil {
		return false, err
	}
	return inserted, insertErr
}

// See smtpedgeservice.SmtpEdgeStore. Until SMTP-M9-18: ErrSmtpEdgeTablesNotBuilt.
func (self *SmtpEdgeStore) SetHalt(ctx context.Context, halt *smtpedgeservice.SmtpHalt) error {
	return ErrSmtpEdgeTablesNotBuilt
}

// See smtpedgeservice.SmtpEdgeStore. Until SMTP-M9-18: ErrSmtpEdgeTablesNotBuilt.
func (self *SmtpEdgeStore) ActiveHalts(ctx context.Context) ([]*smtpedgeservice.SmtpHalt, error) {
	return nil, ErrSmtpEdgeTablesNotBuilt
}
