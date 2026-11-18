package model

// §21.10's access (ABUSE-M5-12; D-ABUSE-7, R-ABUSE-6, I-6, I-13; QD-126): the abuse role's only way into a case's
// rows. An AbuseReader is built from a staff member's opaque reviewer id, which the staff route takes from vault's
// abuse-staff.yml (controller/abuse_staff_controller.go), and each of its reads writes one abuse_access_log row, in a
// transaction of its own, before it reads anything: a read whose row cannot be written returns nothing, so no answer
// leaves without its log. It looks up by case id or by join key and by nothing else: no list, no date, no network
// filter, and at most AbuseSettings.LookupRowLimit rows of any kind (§21.10's "never in bulk"). A join lookup's key is
// never kept (api-14): its row says that a join lookup ran, by whom and when, and names no case.
//
// The reader answers rows only. Reviewers never see a sealed report: no table it reads holds one (§21.8).
//
// Machine reads, the action pass's, intake's and the ladders' (GetAbuseCase, GetAbuseCaseActions,
// GetNetworkAbuseHistory and their kin), are not role reads and stay on functions of their own, which no route
// reaches; TestEveryAbuseReadLogs (controller/abuse_staff_controller_test.go) holds every abuse route and the staff
// controller to that, so a new read path that skips the reader fails a test.
//
// Nothing here reads or writes the tables unless the binary links MG5 (server.SidecarMg5Linked).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// What an abuse-role read looked up by, as abuse_access_log stores it.
type AbuseLookupKind string

const (
	AbuseLookupKindCase AbuseLookupKind = "case"
	AbuseLookupKindJoin AbuseLookupKind = "join"
)

// A reader with no reviewer: no read is ever anyone's.
var ErrAbuseReaderInvalid = errors.New("abuse reader invalid")

// A lookup with no key, which would be a read in bulk.
var ErrAbuseLookupInvalid = errors.New("abuse lookup invalid")

// The read's access log row could not be written, so the read returned nothing.
var ErrAbuseAccessLogFailed = errors.New("abuse access log failed")

// The steps around a read's log, which a test hook sees in this order.
const (
	AbuseAccessStepLogBefore = "abuse.access.log.before"
	AbuseAccessStepLogAfter  = "abuse.access.log.after"
)

// The reader's seams, which a test replaces.
var abuseAccessDeps = struct {
	stateLock sync.Mutex
	now       func() time.Time
	// an error from the hook at AbuseAccessStepLogBefore stands for a log that fails
	hook func(step string) error
}{
	now: server.NowUtc,
}

// Calls hook at each AbuseAccessStep until the returned restore: how a test fails a log, or checks that the row is
// there before the read.
func Testing_SetAbuseAccessHook(hook func(step string) error) (restore func()) {
	abuseAccessDeps.stateLock.Lock()
	defer abuseAccessDeps.stateLock.Unlock()
	previous := abuseAccessDeps.hook
	abuseAccessDeps.hook = hook
	return func() {
		abuseAccessDeps.stateLock.Lock()
		defer abuseAccessDeps.stateLock.Unlock()
		abuseAccessDeps.hook = previous
	}
}

// The clock and hook in force now.
func currentAbuseAccessDeps() (func() time.Time, func(step string) error) {
	abuseAccessDeps.stateLock.Lock()
	defer abuseAccessDeps.stateLock.Unlock()
	return abuseAccessDeps.now, abuseAccessDeps.hook
}

// The abuse role's reads for one staff member, the file header's. Safe for concurrent use.
type AbuseReader struct {
	reviewerId server.Id
	settings   *AbuseSettings
}

// A reader for the reviewer, refused for the zero id.
func NewAbuseReader(reviewerId server.Id, settings *AbuseSettings) (*AbuseReader, error) {
	if reviewerId == (server.Id{}) {
		return nil, ErrAbuseReaderInvalid
	}
	return &AbuseReader{reviewerId: reviewerId, settings: settings}, nil
}

// The reviewer the reader logs every read under.
func (self *AbuseReader) ReviewerId() server.Id {
	return self.reviewerId
}

// Writes the read's access log row in a transaction of its own, before the read: an error, then, is
// ErrAbuseAccessLogFailed, and the caller reads nothing.
func (self *AbuseReader) logRead(ctx context.Context, lookupKind AbuseLookupKind, caseId *server.Id) (returnErr error) {
	now, hook := currentAbuseAccessDeps()
	if hook != nil {
		if err := hook(AbuseAccessStepLogBefore); err != nil {
			return fmt.Errorf("%w: %v", ErrAbuseAccessLogFailed, err)
		}
	}
	if r := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO abuse_access_log (log_id, reviewer_id, lookup_kind, case_id, access_time)
					VALUES ($1, $2, $3, $4, $5)
				`,
				server.NewId(),
				self.reviewerId,
				string(lookupKind),
				caseId,
				now().UTC(),
			))
		})
	}); r != nil {
		if server.IsDoneError(r) {
			panic(r)
		}
		return fmt.Errorf("%w: %v", ErrAbuseAccessLogFailed, r)
	}
	if hook != nil {
		if err := hook(AbuseAccessStepLogAfter); err != nil {
			return fmt.Errorf("%w: %v", ErrAbuseAccessLogFailed, err)
		}
	}
	return nil
}

// One case as the abuse role reads it: the row, its actions and lifts oldest first, and its appeals oldest first.
type AbuseCaseFile struct {
	Case    *AbuseCase
	Actions []*AbuseAction
	Appeals []*AbuseAppeal
	// the case holds more actions or appeals than LookupRowLimit let through
	More bool
}

// The case by its id, its read logged first. ErrAbuseCaseNotFound after the log for an id no case has, since the
// read was made; ErrAbuseLookupInvalid, with no log, for the zero id.
func (self *AbuseReader) LookupAbuseCase(ctx context.Context, caseId server.Id) (*AbuseCaseFile, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if caseId == (server.Id{}) {
		return nil, fmt.Errorf("%w: no case id", ErrAbuseLookupInvalid)
	}
	if err := self.logRead(ctx, AbuseLookupKindCase, &caseId); err != nil {
		return nil, err
	}
	limit := max(self.settings.LookupRowLimit, 0)
	var caseFile *AbuseCaseFile
	server.Tx(ctx, func(tx server.PgTx) {
		caseFile = nil
		abuseCase := getAbuseCaseInTx(ctx, tx, caseId, false)
		if abuseCase == nil {
			return
		}
		caseFile = &AbuseCaseFile{Case: abuseCase, Actions: []*AbuseAction{}, Appeals: []*AbuseAppeal{}}
		result, err := tx.Query(
			ctx,
			`
				SELECT `+abuseActionColumns+`
				FROM abuse_action
				WHERE abuse_action.case_id = $1
				ORDER BY abuse_action.create_time, abuse_action.action_id
				LIMIT $2
			`,
			caseId,
			limit+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				caseFile.Actions = append(caseFile.Actions, scanAbuseAction(result))
			}
		})
		result, err = tx.Query(
			ctx,
			`
				SELECT `+abuseAppealColumns+`
				FROM abuse_appeal
				WHERE abuse_appeal.case_id = $1
				ORDER BY abuse_appeal.create_time, abuse_appeal.appeal_id
				LIMIT $2
			`,
			caseId,
			limit+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				caseFile.Appeals = append(caseFile.Appeals, scanAbuseAppeal(result))
			}
		})
		if limit < len(caseFile.Actions) {
			caseFile.Actions = caseFile.Actions[:limit]
			caseFile.More = true
		}
		if limit < len(caseFile.Appeals) {
			caseFile.Appeals = caseFile.Appeals[:limit]
			caseFile.More = true
		}
	})
	if caseFile == nil {
		return nil, ErrAbuseCaseNotFound
	}
	return caseFile, nil
}

// A join lookup's cases, by case id, at most LookupRowLimit.
type AbuseJoinLookup struct {
	Cases []*AbuseCase
	// more cases share the key than LookupRowLimit let through
	More bool
}

// A join key as abuse_case stores it: a name lowercase with no trailing dot, an id in its canonical form. ok is false
// for a key no row can hold.
func abuseJoinLookupKey(joinKind AbuseJoinKind, joinKey string) (string, bool) {
	joinKey = strings.TrimSpace(joinKey)
	if joinKey == "" || abuseJoinKeyMaxLength < len(joinKey) {
		return "", false
	}
	switch joinKind {
	case AbuseJoinKindHost, AbuseJoinKindAddress, AbuseJoinKindHeaders:
		name := abuseName(joinKey)
		return name, name != ""
	case AbuseJoinKindConnection, AbuseJoinKindNetwork:
		id, err := server.ParseId(joinKey)
		if err != nil {
			return "", false
		}
		return id.String(), true
	case AbuseJoinKindNotice, AbuseJoinKindBreaker:
		// a notice's class or a breaker's name: a fixed word
		word := abuseName(joinKey)
		return word, word != ""
	}
	return "", false
}

// The cases a join key names, oldest first by id, their read logged first with no key and no case (api-14).
// ErrAbuseLookupInvalid, with no log, for an unknown kind or a key no row can hold, an empty one above all.
func (self *AbuseReader) LookupAbuseCasesByJoinKey(ctx context.Context, joinKind AbuseJoinKind, joinKey string) (*AbuseJoinLookup, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	key, ok := abuseJoinLookupKey(joinKind, joinKey)
	if !ok {
		return nil, fmt.Errorf("%w: no join key", ErrAbuseLookupInvalid)
	}
	if err := self.logRead(ctx, AbuseLookupKindJoin, nil); err != nil {
		return nil, err
	}
	limit := max(self.settings.LookupRowLimit, 0)
	lookup := &AbuseJoinLookup{Cases: []*AbuseCase{}}
	server.Db(ctx, func(conn server.PgConn) {
		lookup.Cases = []*AbuseCase{}
		result, err := conn.Query(
			ctx,
			`
				SELECT `+abuseCaseColumns+`
				FROM abuse_case
				WHERE abuse_case.join_kind = $1 AND abuse_case.join_key = $2
				ORDER BY abuse_case.case_id
				LIMIT $3
			`,
			string(joinKind),
			key,
			limit+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				lookup.Cases = append(lookup.Cases, scanAbuseCase(result))
			}
		})
	})
	if limit < len(lookup.Cases) {
		lookup.Cases = lookup.Cases[:limit]
		lookup.More = true
	}
	return lookup, nil
}
