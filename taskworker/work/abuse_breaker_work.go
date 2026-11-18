package work

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §21.5's breakers as a job (ABUSE-M9-02; D-ABUSE-2, R-ABUSE-4): each
// PassInterval, takes one sample of the shared measures, asks
// model.NextMailBreakerState what the breakers' state is after it, and writes
// the difference as abuse_action rows of kind mail_breaker through §21's one
// action path, so each row and each lift is mailed off site like any action
// and §19.10's restore re-applies it. smtpedge's send gate reads the standing
// rows on its own refresh (model.GetMailBreakerState).
//
//	the level rises        one row level:<n>; lower levels' rows stay standing
//	an address is listed   one row address:<address>
//	an address is delisted its rows are lifted
//	a clean count starts   one row clean:<time>
//	a dirty sample         the clean row is lifted
//	the level clears       every level row and the clean row are lifted
//
// A machine never lifts a level 3 row: NextMailBreakerState keeps level 3
// until a person lifts it through the staff route, after which the level is
// the highest row still standing and clears by that level's own rule.
//
// A pass that cannot read the standing rows changes nothing, and one that
// fails part way leaves rows the next pass reads and finishes from, since the
// state is the rows. A sample with nothing known changes nothing either,
// which is what a process with no measurer takes. The job is scheduled only
// in a binary that links MG5, for the rows, and MG9, the milestone the shared
// sending addresses arrive with.

// Gathers one sample of the breakers' measures at now. It answers unknown,
// never zero, for what it cannot measure.
type MailBreakerMeasurer func(ctx context.Context, now time.Time, settings *model.MailBreakerSettings) *model.MailBreakerMeasures

// The job's seams, which a test replaces.
var abuseBreakerDeps = struct {
	stateLock sync.Mutex
	measurer  MailBreakerMeasurer
}{
	measurer: noMailBreakerMeasures,
}

// What a process with no measurer samples: nothing known, so no level trips
// and none clears.
func noMailBreakerMeasures(ctx context.Context, now time.Time, settings *model.MailBreakerSettings) *model.MailBreakerMeasures {
	return &model.MailBreakerMeasures{}
}

// Installs the process's measurer, returning what puts the previous one back.
func SetMailBreakerMeasurer(measurer MailBreakerMeasurer) (restore func()) {
	abuseBreakerDeps.stateLock.Lock()
	defer abuseBreakerDeps.stateLock.Unlock()
	previous := abuseBreakerDeps.measurer
	abuseBreakerDeps.measurer = measurer
	return func() {
		abuseBreakerDeps.stateLock.Lock()
		defer abuseBreakerDeps.stateLock.Unlock()
		abuseBreakerDeps.measurer = previous
	}
}

func currentMailBreakerMeasurer() MailBreakerMeasurer {
	abuseBreakerDeps.stateLock.Lock()
	defer abuseBreakerDeps.stateLock.Unlock()
	return abuseBreakerDeps.measurer
}

// The reasons a machine's lift of a breaker row records: fixed words.
const (
	mailBreakerLiftReasonCleared  = "the breaker cleared"
	mailBreakerLiftReasonDelisted = "the address is no longer listed"
	mailBreakerLiftReasonDirty    = "a sample at or over the clear rate"
)

// What one pass did.
type MailBreakerPassResult struct {
	// the standing rows could not be read whole, so nothing changed
	Held          bool
	PreviousLevel model.MailBreakerLevel
	Level         model.MailBreakerLevel
	// rows recorded and rows lifted
	AppliedCount int
	LiftedCount  int
	// sending addresses out of the sending set after the pass
	ListedCount int
}

// One pass over one sample at now (see the file header). The error is a row
// that could not be written: what was written before it stands, and the next
// pass finishes from the rows.
func RunMailBreakerPass(
	ctx context.Context,
	measures *model.MailBreakerMeasures,
	now time.Time,
	settings *model.MailBreakerSettings,
) (result *MailBreakerPassResult, returnErr error) {
	result = &MailBreakerPassResult{}
	defer func() {
		// the store raises a failed read or write
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			returnErr = fmt.Errorf("abuse breakers: the store did not answer: %v", r)
		}
	}()
	rows, ok := model.GetMailBreakerRows(ctx, settings)
	if !ok {
		result.Held = true
		return result, nil
	}
	state := rows.State
	next := model.NextMailBreakerState(state, measures, now, settings)
	result.PreviousLevel = state.Level
	result.Level = state.Level
	result.ListedCount = len(state.ListedAddresses)

	abuseSettings := model.DefaultAbuseSettings()
	apply := func(target string) error {
		var caseId server.Id
		var err error
		server.Tx(ctx, func(tx server.PgTx) {
			caseId, err = model.EnsureMailBreakerCaseInTx(ctx, tx, now, abuseSettings)
		})
		if err != nil {
			return err
		}
		if _, err := controller.ApplyAbuseAction(ctx, &model.AbuseActionArgs{
			ActionId: server.NewId(),
			CaseId:   caseId,
			Kind:     model.AbuseActionKindMailBreaker,
			Target:   target,
		}); err != nil {
			return err
		}
		result.AppliedCount += 1
		return nil
	}
	lift := func(actions []*model.AbuseAction, reason string) error {
		for _, action := range actions {
			_, err := controller.LiftAbuseAction(ctx, &model.AbuseLiftArgs{
				ActionId:       server.NewId(),
				CaseId:         action.CaseId,
				LiftedActionId: action.ActionId,
				ReviewState:    model.AbuseReviewStateLifted,
				ReviewReason:   reason,
			})
			if errors.Is(err, model.ErrAbuseActionLifted) {
				// a person lifted it since the rows were read
				continue
			}
			if err != nil {
				return err
			}
			result.LiftedCount += 1
		}
		return nil
	}

	// what pauses goes first: the level, then the listed addresses
	if state.Level < next.Level {
		if err := apply(model.MailBreakerLevelTarget(next.Level)); err != nil {
			return result, err
		}
		result.Level = next.Level
	}
	for _, address := range next.ListedAddresses {
		if slices.Contains(state.ListedAddresses, address) {
			continue
		}
		if err := apply(model.MailBreakerAddressTarget(address)); err != nil {
			return result, err
		}
		result.ListedCount += 1
	}

	// then what the sample ends
	if !state.CleanSinceTime.IsZero() && (next.CleanSinceTime.IsZero() || next.Level == model.MailBreakerLevelNone) {
		reason := mailBreakerLiftReasonDirty
		if next.Level < state.Level {
			reason = mailBreakerLiftReasonCleared
		}
		if err := lift(rows.CleanActions, reason); err != nil {
			return result, err
		}
	}
	for _, address := range state.ListedAddresses {
		if slices.Contains(next.ListedAddresses, address) {
			continue
		}
		if err := lift(rows.AddressActionsOf(address), mailBreakerLiftReasonDelisted); err != nil {
			return result, err
		}
		result.ListedCount -= 1
	}
	if next.Level < state.Level {
		// the machine clears to none only, and never from level 3
		// (model.NextMailBreakerState)
		if next.Level != model.MailBreakerLevelNone || model.MailBreakerLevelAll <= state.Level {
			return result, fmt.Errorf("abuse breakers: a machine may not take level %d to %d", state.Level, next.Level)
		}
		if err := lift(rows.LevelActions, mailBreakerLiftReasonCleared); err != nil {
			return result, err
		}
		result.Level = next.Level
	}

	// and a clean count that starts
	if state.CleanSinceTime.IsZero() && !next.CleanSinceTime.IsZero() {
		if err := apply(model.MailBreakerCleanTarget(next.CleanSinceTime)); err != nil {
			return result, err
		}
	}
	return result, nil
}

type AbuseBreakersArgs struct {
}

type AbuseBreakersResult struct {
	Held          bool `json:"held"`
	PreviousLevel int  `json:"previous_level"`
	Level         int  `json:"level"`
	AppliedCount  int  `json:"applied_count"`
	LiftedCount   int  `json:"lifted_count"`
	ListedCount   int  `json:"listed_count"`
}

// Whether this binary runs the breakers: it links MG5, whose rows hold their
// state, and MG9, the milestone they guard.
func abuseBreakersLinked() bool {
	return server.SidecarMg5Linked() && server.SidecarMg9Linked()
}

// Schedules the next pass one PassInterval from now, once. A binary that does
// not link MG5 and MG9 schedules nothing.
func ScheduleAbuseBreakers(clientSession *session.ClientSession, tx server.PgTx) {
	if !abuseBreakersLinked() {
		return
	}
	settings := model.DefaultMailBreakerSettings()
	task.ScheduleTaskInTx(
		tx,
		AbuseBreakers,
		&AbuseBreakersArgs{},
		clientSession,
		task.RunOnce("abuse_breakers"),
		task.RunAt(server.NowUtc().Add(settings.PassInterval)),
		task.MaxTime(settings.PassTaskMaxTime),
	)
}

// One pass over the process's measurer's sample, with pro.yml's thresholds. A
// pass that failed is logged and answered, so the next is scheduled.
func AbuseBreakers(
	abuseBreakers *AbuseBreakersArgs,
	clientSession *session.ClientSession,
) (*AbuseBreakersResult, error) {
	if !abuseBreakersLinked() {
		return &AbuseBreakersResult{}, nil
	}
	settings := model.MailBreakerSettingsOfPro()
	now := server.NowUtc()
	measures := currentMailBreakerMeasurer()(clientSession.Ctx, now, settings)
	result, err := RunMailBreakerPass(clientSession.Ctx, measures, now, settings)
	if err != nil {
		glog.Errorf("[abuse]the breaker pass did not finish, and the next pass finishes from its rows: %s\n", err)
	}
	if result.Held {
		glog.Errorf("[abuse]the breaker rows did not read whole, so the breaker pass changed nothing\n")
	}
	if result.Level != result.PreviousLevel || 0 < result.AppliedCount || 0 < result.LiftedCount {
		glog.Infof(
			"[abuse]the mail breaker went from level %d to %d with %d sending addresses listed (%d rows recorded, %d lifted)\n",
			result.PreviousLevel, result.Level, result.ListedCount, result.AppliedCount, result.LiftedCount,
		)
	}
	return &AbuseBreakersResult{
		Held:          result.Held,
		PreviousLevel: int(result.PreviousLevel),
		Level:         int(result.Level),
		AppliedCount:  result.AppliedCount,
		LiftedCount:   result.LiftedCount,
		ListedCount:   result.ListedCount,
	}, nil
}

// Schedules the next pass.
func AbuseBreakersPost(
	abuseBreakers *AbuseBreakersArgs,
	abuseBreakersResult *AbuseBreakersResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleAbuseBreakers(clientSession, tx)
	return nil
}
