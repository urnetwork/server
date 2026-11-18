package model

// §21.5's breakers (ABUSE-M9-02; D-ABUSE-2, R-ABUSE-4, D-SMTP-14, I-12): what
// guards <root> and the shared sending addresses above §14.8's per-network
// halts. This file is the state machine, pure, and the rows its state lives
// in. taskworker's AbuseBreakers gathers the measures and applies the
// machine's answer; smtpedge's send gate reads the state on its refresh, off
// every preamble's path.
//
//	level  trigger (trailing 24 h)                                  acts on                    clears
//	1      <root>'s Postmaster spam rate at breaker_restrict_rate   restricted networks pause; breaker_clear_days below
//	       two days running, or feedback-loop complaints at that    Community caps halve       breaker_clear_rate
//	       rate of recipients
//	2      either measure at breaker_community_rate, or a sending   Community pauses; a listed delisting, then the same
//	       address on Spamhaus SBL or CSS or SNDS red               address leaves the sending clean days
//	                                                                set, never ingress
//	3      either measure at breaker_all_rate                       Community and Pro pause;   a person, through the
//	                                                                trusted Team continues     staff route's lift
//
// The levels ratchet: a measure dipping under a trigger clears nothing, and a
// level falls only by its own clearing rule. A clean sample is one where
// every measure is known, both rates are under breaker_clear_rate and no
// sending address is listed; the level clears once such samples have run
// breaker_clear_days with no dirty one between. A dirty sample, a rate at or
// over the clear rate or a listed address, restarts the count. An unknown
// measure neither trips nor clears: it keeps the level and the count where
// they are, so a source that is down, a day Postmaster Tools has not reported
// and a missing credential never read as clean. Thresholds that pro.yml does
// not give whole make every rate unknown.
//
// A breaker acts on tiers and addresses and on no network's case: it opens no
// per-network action and strikes nobody, and it can pause networks no report
// names (§21.15). It acts early, since <root> cannot be replaced. A listing
// removes an address from sending only; api and connect ingress on it stay
// (owner decision 124).
//
// The state is abuse_action rows of kind mail_breaker with no network
// (plan-ABUSE-metadata-rows #5), under a case of join kind breaker, each
// mailed like any action so that §19.10's restore re-applies it. A row stands
// until a lift names it:
//
//	level:<n>            the breaker is at level n; the level is the highest standing one
//	address:<address>    the sending address is listed and out of the sending set
//	clean:<time>         clean samples have run since that time
//
// Clearing lifts the level rows and the clean row; a delisting lifts its
// address row; a dirty sample lifts the clean row. Level 3 is lifted only by
// a person, and a person may lift any row sooner. This is §21.5's breaker,
// not §14.7's Community hourly one (smtpedgeservice.SmtpBreaker).
//
// Nothing here reads or writes the tables unless the binary links MG5
// (server.SidecarMg5Linked), and then GetMailBreakerState answers not loaded,
// which the send gate answers TRY_LATER.

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// A breaker's row, as abuse_action stores it.
const AbuseActionKindMailBreaker AbuseActionKind = "mail_breaker"

// Who a breaker's case came from: the abuse worker's own measures.
const AbuseReporterClassBreaker AbuseReporterClass = "breaker"

// The breaker case's join key and target: fixed words, since a breaker names
// no network and no host.
const (
	mailBreakerJoinKey    = "mail"
	mailBreakerCaseTarget = "mail-breaker"
)

// The words a breaker row's target starts with.
const (
	mailBreakerTargetLevel   = "level"
	mailBreakerTargetAddress = "address"
	mailBreakerTargetClean   = "clean"
)

// pro.yml's sidecar_abuse: keys for the thresholds (D-ABUSE-2), each rate in
// parts per million of recipients.
const (
	ProKeyBreakerRestrictRate  = "breaker_restrict_rate"
	ProKeyBreakerCommunityRate = "breaker_community_rate"
	ProKeyBreakerAllRate       = "breaker_all_rate"
	ProKeyBreakerClearRate     = "breaker_clear_rate"
	ProKeyBreakerClearDays     = "breaker_clear_days"
)

// §21.5's levels, with the values smtpedgeservice.SmtpBreakerLevel gives them.
// What each level pauses and halves is the send gate's to apply
// (smtpedgeservice's breaker row): this file only decides the level.
type MailBreakerLevel int

const (
	MailBreakerLevelNone MailBreakerLevel = 0
	// restricted networks pause and Community caps halve
	MailBreakerLevelRestrict MailBreakerLevel = 1
	// Community pauses too
	MailBreakerLevelCommunity MailBreakerLevel = 2
	// Community and Pro pause, and only a trusted Team sends
	MailBreakerLevelAll MailBreakerLevel = 3
)

func (self MailBreakerLevel) known() bool {
	return MailBreakerLevelNone <= self && self <= MailBreakerLevelAll
}

// One measured rate, or the fact that it could not be measured. The zero
// value is unknown, never zero percent.
type MailRate struct {
	Known bool
	// of recipients, in parts per million: 0.10% is 1000
	PartsPerMillion int64
}

// A known rate.
func KnownMailRate(partsPerMillion int64) MailRate {
	return MailRate{Known: true, PartsPerMillion: partsPerMillion}
}

// Tunables of §21.5's breakers: the machine's, the job's and the measures'.
type MailBreakerSettings struct {
	// pro.yml's breaker_restrict_rate, breaker_community_rate,
	// breaker_all_rate and breaker_clear_rate, in parts per million of
	// recipients: §21.5's 0.10%, 0.20%, 0.30% and 0.08%. They come only from
	// pro.yml (ApplyProMailBreaker), so they are zero here until read, and
	// while they are not positive and in that order every rate reads unknown:
	// no rate trips a level and none clears one.
	RestrictRate  int64
	CommunityRate int64
	AllRate       int64
	ClearRate     int64
	// pro.yml's breaker_clear_days: §21.5's 3 days of clean samples.
	// Non-positive never clears by machine.
	ClearDays int
	// How many of the latest reported days <root>'s Postmaster spam rate must
	// be at the restrict rate to trip level 1: §21.5's "two days running".
	// Non-positive trips level 1 on no Postmaster rate.
	PostmasterRunDays int
	// After a dirty sample ends a clean count, how long before a clean sample
	// may start another: an hour, so a rate hovering at the clear rate writes
	// at most one clean row and one lift an hour. Non-positive starts one at
	// once.
	CleanRestartHoldoff time.Duration

	// How often AbuseBreakers runs: 5 minutes, the listing feed's refresh
	// (§15.4), so a listed address leaves the sending set within it.
	PassInterval time.Duration
	// The task runner's bound on one pass.
	PassTaskMaxTime time.Duration
	// The most standing breaker rows one read takes. Past it the state reads
	// not loaded, since a level among the unread rows could be the highest.
	RowLimit int

	// The window of the complaint rate: §21.5's trailing 24 h.
	ComplaintWindow time.Duration
	// How many days back the Postmaster rate may lie and still be the latest:
	// 3, since Postmaster Tools reports a day late and sometimes two. Past it
	// the rate is unknown.
	PostmasterMaxLagDays int
}

// The breakers' own numbers. The thresholds stay zero: they are pro.yml's.
func DefaultMailBreakerSettings() *MailBreakerSettings {
	return &MailBreakerSettings{
		PostmasterRunDays:    2,
		CleanRestartHoldoff:  time.Hour,
		PassInterval:         5 * time.Minute,
		PassTaskMaxTime:      10 * time.Minute,
		RowLimit:             1024,
		ComplaintWindow:      24 * time.Hour,
		PostmasterMaxLagDays: 3,
	}
}

// Lays pro.yml's sidecar_abuse: thresholds over settings (D-ABUSE-2). A key
// the file lacks reads zero, which leaves the rates unusable and the breakers
// holding.
func ApplyProMailBreaker(settings *MailBreakerSettings, abuse *ProSidecarAbuse) {
	settings.RestrictRate = abuse.Limit(ProKeyBreakerRestrictRate)
	settings.CommunityRate = abuse.Limit(ProKeyBreakerCommunityRate)
	settings.AllRate = abuse.Limit(ProKeyBreakerAllRate)
	settings.ClearRate = abuse.Limit(ProKeyBreakerClearRate)
	settings.ClearDays = int(abuse.Limit(ProKeyBreakerClearDays))
}

// The breakers' settings with pro.yml's thresholds.
func MailBreakerSettingsOfPro() *MailBreakerSettings {
	settings := DefaultMailBreakerSettings()
	ApplyProMailBreaker(settings, &Pro().SidecarAbuse)
	return settings
}

// Whether the thresholds are whole: positive, the clear rate no higher than
// the restrict rate, and the levels' rates in order.
func (self *MailBreakerSettings) ratesUsable() bool {
	return 0 < self.ClearRate &&
		self.ClearRate <= self.RestrictRate &&
		self.RestrictRate <= self.CommunityRate &&
		self.CommunityRate <= self.AllRate
}

// One sample of what the breakers read (ABUSE-M9-03 gathers it). Aggregate
// figures only: nothing here names a network.
type MailBreakerMeasures struct {
	// <root>'s Postmaster spam rate for its latest reported day, then each
	// day before it, newest first. Empty when no day inside the lag is
	// reported.
	PostmasterRates []MailRate
	// feedback-loop complaints over recipients, trailing 24 h
	ComplaintRate MailRate
	// whether every listing source answered; false keeps the addresses that
	// stand listed
	ListingKnown bool
	// URnetwork's own sending addresses on Spamhaus SBL or CSS or SNDS red.
	// An address named here counts even when ListingKnown is false: a source
	// that answered listed it.
	ListedAddresses []netip.Addr
}

// The breakers' state, as the standing rows hold it.
type MailBreakerState struct {
	Level MailBreakerLevel
	// sending addresses out of the sending set, sorted, each once
	ListedAddresses []netip.Addr
	// since when clean samples have run; zero while none do
	CleanSinceTime time.Time
	// when a clean count last ended on a dirty sample; zero when none has
	CleanEndTime time.Time
}

// Whether two states are the same.
func (self MailBreakerState) Equal(other MailBreakerState) bool {
	return self.Level == other.Level &&
		slices.Equal(self.ListedAddresses, other.ListedAddresses) &&
		self.CleanSinceTime.Equal(other.CleanSinceTime) &&
		self.CleanEndTime.Equal(other.CleanEndTime)
}

// Addresses unmapped, sorted, each once, with invalid ones dropped.
func mailBreakerAddresses(addresses []netip.Addr) []netip.Addr {
	kept := []netip.Addr{}
	for _, address := range addresses {
		if address = address.Unmap(); address.IsValid() && !slices.Contains(kept, address) {
			kept = append(kept, address)
		}
	}
	slices.SortFunc(kept, func(a netip.Addr, b netip.Addr) int {
		return a.Compare(b)
	})
	return kept
}

// The state after one sample at now (see the file header). Pure: the rows
// that hold the state are the caller's to write.
func NextMailBreakerState(
	state MailBreakerState,
	measures *MailBreakerMeasures,
	now time.Time,
	settings *MailBreakerSettings,
) MailBreakerState {
	next := MailBreakerState{
		Level:           state.Level,
		ListedAddresses: mailBreakerAddresses(state.ListedAddresses),
		CleanSinceTime:  state.CleanSinceTime,
		CleanEndTime:    state.CleanEndTime,
	}
	if measures == nil {
		measures = &MailBreakerMeasures{}
	}

	ratesUsable := settings.ratesUsable()
	rate := func(measured MailRate) MailRate {
		if !ratesUsable {
			return MailRate{}
		}
		return measured
	}
	atOrOver := func(measured MailRate, threshold int64) bool {
		return measured.Known && threshold <= measured.PartsPerMillion
	}
	complaintRate := rate(measures.ComplaintRate)
	// the latest reported day's rate
	postmasterRate := MailRate{}
	if 0 < len(measures.PostmasterRates) {
		postmasterRate = rate(measures.PostmasterRates[0])
	}
	// the run of latest days at the restrict rate
	postmasterRun := 0 < settings.PostmasterRunDays && settings.PostmasterRunDays <= len(measures.PostmasterRates)
	if postmasterRun {
		for _, dayRate := range measures.PostmasterRates[:settings.PostmasterRunDays] {
			if !atOrOver(rate(dayRate), settings.RestrictRate) {
				postmasterRun = false
			}
		}
	}

	// a listing a source answered counts at once; an unknown listing keeps
	// what stands listed
	listed := mailBreakerAddresses(measures.ListedAddresses)
	if measures.ListingKnown {
		next.ListedAddresses = listed
	} else {
		next.ListedAddresses = mailBreakerAddresses(append(next.ListedAddresses, listed...))
	}

	triggered := MailBreakerLevelNone
	switch {
	case atOrOver(postmasterRate, settings.AllRate) || atOrOver(complaintRate, settings.AllRate):
		triggered = MailBreakerLevelAll
	case atOrOver(postmasterRate, settings.CommunityRate) || atOrOver(complaintRate, settings.CommunityRate) ||
		0 < len(next.ListedAddresses):
		triggered = MailBreakerLevelCommunity
	case atOrOver(complaintRate, settings.RestrictRate) || postmasterRun:
		triggered = MailBreakerLevelRestrict
	}
	next.Level = max(next.Level, triggered)

	switch next.Level {
	case MailBreakerLevelNone, MailBreakerLevelAll:
		// nothing to clear, or only a person clears it
		next.CleanSinceTime = time.Time{}
		return next
	}

	dirty := atOrOver(postmasterRate, settings.ClearRate) || atOrOver(complaintRate, settings.ClearRate) ||
		0 < len(next.ListedAddresses)
	clean := !dirty && postmasterRate.Known && complaintRate.Known && measures.ListingKnown
	switch {
	case dirty:
		if !next.CleanSinceTime.IsZero() {
			next.CleanSinceTime = time.Time{}
			next.CleanEndTime = now.UTC()
		}
	case !clean:
		// an unknown measure keeps the level and the count where they are
	case next.CleanSinceTime.IsZero():
		if next.CleanEndTime.IsZero() || settings.CleanRestartHoldoff <= 0 || settings.CleanRestartHoldoff <= now.Sub(next.CleanEndTime) {
			next.CleanSinceTime = now.UTC()
		}
	case 0 < settings.ClearDays && time.Duration(settings.ClearDays)*24*time.Hour <= now.Sub(next.CleanSinceTime):
		next.Level = MailBreakerLevelNone
		next.CleanSinceTime = time.Time{}
	}
	return next
}

// The target of a level's row.
func MailBreakerLevelTarget(level MailBreakerLevel) string {
	return mailBreakerTargetLevel + ":" + strconv.Itoa(int(level))
}

// The target of a listed sending address's row.
func MailBreakerAddressTarget(address netip.Addr) string {
	return mailBreakerTargetAddress + ":" + address.Unmap().String()
}

// The target of a clean count's row, to the second.
func MailBreakerCleanTarget(cleanSinceTime time.Time) string {
	return mailBreakerTargetClean + ":" + cleanSinceTime.UTC().Format(time.RFC3339)
}

// One breaker row's target, read. ok is false for a target no breaker row
// may hold: a level that is none or unknown, an address that does not parse
// or is not in its canonical form, a time that is not RFC 3339 in UTC.
type mailBreakerTarget struct {
	word           string
	level          MailBreakerLevel
	address        netip.Addr
	cleanSinceTime time.Time
}

func parseMailBreakerTarget(target string) (parsed mailBreakerTarget, ok bool) {
	word, value, found := strings.Cut(target, ":")
	if !found {
		return mailBreakerTarget{}, false
	}
	parsed.word = word
	switch word {
	case mailBreakerTargetLevel:
		level, err := strconv.Atoi(value)
		parsed.level = MailBreakerLevel(level)
		if err != nil || !parsed.level.known() || parsed.level == MailBreakerLevelNone || MailBreakerLevelTarget(parsed.level) != target {
			return mailBreakerTarget{}, false
		}
		return parsed, true
	case mailBreakerTargetAddress:
		address, err := netip.ParseAddr(value)
		if err != nil || MailBreakerAddressTarget(address) != target {
			return mailBreakerTarget{}, false
		}
		parsed.address = address
		return parsed, true
	case mailBreakerTargetClean:
		cleanSinceTime, err := time.Parse(time.RFC3339, value)
		if err != nil || MailBreakerCleanTarget(cleanSinceTime) != target {
			return mailBreakerTarget{}, false
		}
		parsed.cleanSinceTime = cleanSinceTime.UTC()
		return parsed, true
	}
	return mailBreakerTarget{}, false
}

// Refuses a breaker row no state may hold: one under a case that is not a
// breaker's, or with a target that does not read as a breaker row's.
func validateMailBreakerAction(abuseCase *AbuseCase, target string) error {
	if abuseCase.NetworkId != nil || abuseCase.JoinKind != AbuseJoinKindBreaker {
		return fmt.Errorf("%w: a breaker row on a case that is no breaker's", ErrAbuseActionInvalid)
	}
	if _, ok := parseMailBreakerTarget(target); !ok {
		return fmt.Errorf("%w: a breaker row whose target is no level, address or clean time", ErrAbuseActionInvalid)
	}
	return nil
}

// The standing breaker rows and the state they hold.
type MailBreakerRows struct {
	State MailBreakerState
	// the standing rows by what they hold, oldest first
	LevelActions   []*AbuseAction
	AddressActions []*AbuseAction
	CleanActions   []*AbuseAction
}

// The standing address rows of one address.
func (self *MailBreakerRows) AddressActionsOf(address netip.Addr) []*AbuseAction {
	target := MailBreakerAddressTarget(address)
	actions := []*AbuseAction{}
	for _, action := range self.AddressActions {
		if action.Target == target {
			actions = append(actions, action)
		}
	}
	return actions
}

// The standing breaker rows, read whole, and whether they were: ok is false in
// a binary that does not link MG5, when more rows stand than RowLimit, when a
// standing row's target does not read, and when the store did not answer.
// Then the state is unknown, and a reader holds what it has or refuses.
func GetMailBreakerRows(ctx context.Context, settings *MailBreakerSettings) (rows *MailBreakerRows, ok bool) {
	if !server.SidecarMg5Linked() {
		return nil, false
	}
	defer func() {
		// the store raises a failed read
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			rows = nil
			ok = false
			if glog.V(1) {
				glog.Infof("[abuse]the breaker rows did not read: %v\n", r)
			}
		}
	}()
	actions := []*AbuseAction{}
	var cleanEndTime *time.Time
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+abuseActionColumns+`
				FROM abuse_action
				INNER JOIN abuse_case ON abuse_case.case_id = abuse_action.case_id
				WHERE
					abuse_case.join_kind = $1 AND
					abuse_case.join_key = $2 AND
					abuse_action.kind = $3 AND
					abuse_action.network_id IS NULL AND
					abuse_action.review_state NOT IN ('lifted', 'lifted_wrong')
				ORDER BY abuse_action.create_time, abuse_action.action_id
				LIMIT $4
			`,
			string(AbuseJoinKindBreaker),
			mailBreakerJoinKey,
			string(AbuseActionKindMailBreaker),
			settings.RowLimit+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				actions = append(actions, scanAbuseAction(result))
			}
		})
		// when the last clean count ended: the latest lift of a clean row
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT MAX(abuse_action.review_time)
				FROM abuse_action
				INNER JOIN abuse_case ON abuse_case.case_id = abuse_action.case_id
				WHERE
					abuse_case.join_kind = $1 AND
					abuse_case.join_key = $2 AND
					abuse_action.kind = $3 AND
					abuse_action.network_id IS NULL AND
					abuse_action.review_state IN ('lifted', 'lifted_wrong') AND
					abuse_action.target LIKE $4
			`,
			string(AbuseJoinKindBreaker),
			mailBreakerJoinKey,
			string(AbuseActionKindMailBreaker),
			mailBreakerTargetClean+":%",
		).Scan(&cleanEndTime))
	})
	if settings.RowLimit < len(actions) {
		return nil, false
	}

	rows = &MailBreakerRows{
		LevelActions:   []*AbuseAction{},
		AddressActions: []*AbuseAction{},
		CleanActions:   []*AbuseAction{},
	}
	if cleanEndTime != nil {
		rows.State.CleanEndTime = cleanEndTime.UTC()
	}
	addresses := []netip.Addr{}
	for _, action := range actions {
		parsed, ok := parseMailBreakerTarget(action.Target)
		if !ok {
			return nil, false
		}
		switch parsed.word {
		case mailBreakerTargetLevel:
			rows.LevelActions = append(rows.LevelActions, action)
			rows.State.Level = max(rows.State.Level, parsed.level)
		case mailBreakerTargetAddress:
			rows.AddressActions = append(rows.AddressActions, action)
			addresses = append(addresses, parsed.address)
		case mailBreakerTargetClean:
			rows.CleanActions = append(rows.CleanActions, action)
			// the earliest standing count
			if rows.State.CleanSinceTime.IsZero() || parsed.cleanSinceTime.Before(rows.State.CleanSinceTime) {
				rows.State.CleanSinceTime = parsed.cleanSinceTime
			}
		}
	}
	rows.State.ListedAddresses = mailBreakerAddresses(addresses)
	return rows, true
}

// §21.5's breaker state as the send gate reads it on its refresh, and whether
// it is loaded (GetMailBreakerRows' ok): not loaded answers every preamble
// TRY_LATER, so mail waits at its sender. Never called on a preamble's path
// (I-12).
func GetMailBreakerState(ctx context.Context) (state *MailBreakerState, ok bool) {
	rows, ok := GetMailBreakerRows(ctx, DefaultMailBreakerSettings())
	if !ok {
		return nil, false
	}
	return &rows.State, true
}

// The open breaker case, or a new one, in the caller's transaction: the case
// every breaker row hangs off. It names no network.
func EnsureMailBreakerCaseInTx(ctx context.Context, tx server.PgTx, now time.Time, settings *AbuseSettings) (caseId server.Id, err error) {
	if !server.SidecarMg5Linked() {
		return server.Id{}, ErrAbuseUnlinked
	}
	found := false
	result, queryErr := tx.Query(
		ctx,
		`
			SELECT abuse_case.case_id
			FROM abuse_case
			WHERE
				abuse_case.join_kind = $1 AND
				abuse_case.join_key = $2 AND
				abuse_case.state = 'open' AND
				abuse_case.network_id IS NULL
			ORDER BY abuse_case.case_id DESC
			LIMIT 1
			FOR UPDATE
		`,
		string(AbuseJoinKindBreaker),
		mailBreakerJoinKey,
	)
	server.WithPgResult(result, queryErr, func() {
		if result.Next() {
			server.Raise(result.Scan(&caseId))
			found = true
		}
	})
	if found {
		return caseId, nil
	}
	caseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
		Category:      AbuseCategorySpam,
		Target:        AbuseTarget{Kind: AbuseTargetKindSendingDomain, Name: mailBreakerCaseTarget},
		ReporterClass: AbuseReporterClassBreaker,
		JoinKind:      AbuseJoinKindBreaker,
		JoinKey:       mailBreakerJoinKey,
	}, now, settings)
	return caseId, err
}
