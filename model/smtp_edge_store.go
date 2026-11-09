package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// smtpedge's store on Postgres (§14.9; SMTP-M5-06, SMTP-M9-18): sidecar's
// smtpedgeservice.SmtpEdgeStore and SmtpReportLookups over §14.14's tables.
// The report calls, InsertComplaint and InsertAggregate, write MG5's report
// tables (smtp_report_model.go) for mxedge's platform block, where the report
// intake runs (plan-SMTP-report-intake-home); the send, halt and DKIM key calls
// read and write MG9's (smtp_send_model.go, smtp_halt_model.go,
// smtp_dkim_model.go) for the smtpedge block. Every call is off the connection
// path (§14.9): an edge batches its sends and reloads its halts, and the intake
// writes before its listener answers a report's DATA, never inside a
// connection's packets (I-12). A binary that does not link a call's group
// answers it with that group's error, ErrSmtpReportUnlinked or
// ErrSmtpEdgeUnlinked, never an empty answer, which leaves the engine not
// ready rather than reading no halts. Safe for concurrent use.

var _ smtpedgeservice.SmtpEdgeStore = (*SmtpEdgeStore)(nil)
var _ smtpedgeservice.SmtpReportLookups = (*SmtpEdgeStore)(nil)

// Tunables of the store.
type SmtpEdgeStoreSettings struct {
	// The longest one call may take when its caller gave no earlier deadline,
	// beside the engine's own StoreTimeout. Positive.
	CallTimeout time.Duration
	// smtp_send's write tunables. Its days' retention and runway are the
	// registered family's, DefaultSmtpSendSettings', the same in every store.
	Send *SmtpSendSettings
}

func DefaultSmtpEdgeStoreSettings() *SmtpEdgeStoreSettings {
	return &SmtpEdgeStoreSettings{
		CallTimeout: 10 * time.Second,
		Send:        DefaultSmtpSendSettings(),
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
	if settings.Send == nil {
		withSend := *settings
		withSend.Send = DefaultSmtpSendSettings()
		settings = &withSend
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

// Runs call on a context bounded by CallTimeout, after the group check, with
// what the model raised as an error.
func (self *SmtpEdgeStore) call(ctx context.Context, linked func() bool, unlinked error, call func(ctx context.Context) error) error {
	if !linked() {
		return unlinked
	}
	callCtx, cancel := self.callContext(ctx)
	defer cancel()
	var callErr error
	if err := smtpEdgeStoreRun(func() {
		callErr = call(callCtx)
	}); err != nil {
		return err
	}
	return callErr
}

// See smtpedgeservice.SmtpEdgeStore: InsertSmtpSends at the store's MaxFutureSkew.
// A record no row can hold, or of a day past its retention or too far ahead,
// is dropped and counted there and is not an error: the batch's other records
// are stored.
func (self *SmtpEdgeStore) InsertSends(ctx context.Context, sends []*smtpedgeservice.SmtpSendRecord) error {
	return self.call(ctx, server.SidecarMg9Linked, ErrSmtpEdgeUnlinked, func(ctx context.Context) error {
		_, err := insertSmtpSendsAt(ctx, sends, server.NowUtc(), self.settings.Send)
		return err
	})
}

// See smtpedgeservice.SmtpEdgeStore: FindSmtpSends.
func (self *SmtpEdgeStore) FindSends(ctx context.Context, key *smtpedgeservice.SmtpJoinKey) ([]*smtpedgeservice.SmtpSendRecord, error) {
	var sends []*smtpedgeservice.SmtpSendRecord
	err := self.call(ctx, server.SidecarMg9Linked, ErrSmtpEdgeUnlinked, func(ctx context.Context) (err error) {
		sends, err = FindSmtpSends(ctx, key)
		return
	})
	if err != nil {
		return nil, err
	}
	return sends, nil
}

// See smtpedgeservice.SmtpEdgeStore: InsertSmtpComplaint.
func (self *SmtpEdgeStore) InsertComplaint(ctx context.Context, complaint *smtpedgeservice.SmtpComplaint) error {
	return self.call(ctx, server.SidecarMg5Linked, ErrSmtpReportUnlinked, func(ctx context.Context) error {
		return InsertSmtpComplaint(ctx, complaint)
	})
}

// See smtpedgeservice.SmtpEdgeStore: InsertSmtpAggregate.
func (self *SmtpEdgeStore) InsertAggregate(ctx context.Context, report *smtpedgeservice.SmtpAggregateReport) (bool, error) {
	var inserted bool
	err := self.call(ctx, server.SidecarMg5Linked, ErrSmtpReportUnlinked, func(ctx context.Context) (err error) {
		inserted, err = InsertSmtpAggregate(ctx, report)
		return
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// See smtpedgeservice.SmtpEdgeStore: SetSmtpHalt.
func (self *SmtpEdgeStore) SetHalt(ctx context.Context, halt *smtpedgeservice.SmtpHalt) error {
	return self.call(ctx, server.SidecarMg9Linked, ErrSmtpEdgeUnlinked, func(ctx context.Context) error {
		return SetSmtpHalt(ctx, halt)
	})
}

// See smtpedgeservice.SmtpEdgeStore: GetActiveSmtpHalts at the store's time.
func (self *SmtpEdgeStore) ActiveHalts(ctx context.Context) ([]*smtpedgeservice.SmtpHalt, error) {
	var halts []*smtpedgeservice.SmtpHalt
	err := self.call(ctx, server.SidecarMg9Linked, ErrSmtpEdgeUnlinked, func(ctx context.Context) (err error) {
		halts, err = GetActiveSmtpHalts(ctx, server.NowUtc())
		return
	})
	if err != nil {
		return nil, err
	}
	return halts, nil
}

// See smtpedgeservice.SmtpReportLookups: FindSmtpDkimKey.
func (self *SmtpEdgeStore) FindDkimKey(ctx context.Context, selector string, domain string) (*smtpedgeservice.SmtpDkimKey, error) {
	var key *smtpedgeservice.SmtpDkimKey
	err := self.call(ctx, server.SidecarMg9Linked, ErrSmtpEdgeUnlinked, func(ctx context.Context) (err error) {
		key, err = FindSmtpDkimKey(ctx, selector, domain)
		return
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

// See smtpedgeservice.SmtpReportLookups: SmtpRecipientCount.
func (self *SmtpEdgeStore) RecipientCount(ctx context.Context, networkId sidecar.Id, since time.Time) (uint64, error) {
	var count uint64
	err := self.call(ctx, server.SidecarMg9Linked, ErrSmtpEdgeUnlinked, func(ctx context.Context) (err error) {
		count, err = SmtpRecipientCount(ctx, server.Id(networkId), since)
		return
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
