package model

// §21.7's standing (ABUSE-M5-07; D-ABUSE-4, R-ABUSE-6, Q-ABUSE-2, I-4, I-12): what a network may do beyond its tier,
// which §8's hard deny and the §12-§15 gates read. It is derived (QD-194): a state holds exactly while its entry
// holds, and where several hold, the first of these applies, since probation's mail rule is stricter than
// restricted's:
//
//	suspended   a network_abuse_state row in state suspended, which ABUSE-M5-06's effects write at a §21
//	            suspension and end at its lift (SetNetworkAbuseSuspended, EndNetworkAbuseSuspended): until reviewed
//	probation   network.create_time within ProbationAge, 7 days
//	restricted  a network_abuse_state row in state restricted whose until_time is ahead: a host, label or network
//	            kill, or a network halt, within RestrictedWindow, 30 days, unless review lifted it as wrong
//	            (plan-ABUSE-small-readings #4)
//	trusted     §21.7's trusted (ABUSE-M6-01, plan-ABUSE-small-readings #18): tier team (NetworkTierOf, read through
//	            the cache's own ttls), a verified network_domain row, the Team grant itself, not its credit line, in
//	            window across the whole of TrustedPaidWindow, 30 days, with no unresolved invoice.payment_failed, and
//	            no action in TrustedCleanWindow, 90 days, unless review lifted it as wrong (a lift and a reviewer's
//	            screen_allow are no action). It needs MG6's tables, so a binary that does not link MG6 answers none
//	standard    anything else
//
// Rows exist only for restricted and suspended (plan-ABUSE-small-readings #3), and only while current (§21.10).
// ReevaluateNetworkAbuseStateInTx keeps the restricted row equal to what the action history says: in each action's
// and lift's own transaction (controller.ApplyAbuseAction, LiftAbuseAction), after a suspension's lift turns its row
// over (the effects' EndNetworkSuspended), and in AbuseStandingPass for every row whose until_time has passed, so an
// appeal's lift, which runs as a lift, re-derives it too. A suspended row, or one in a state this build does not know,
// is never the window's to change. A reader never trusts a restricted row past its until_time, so the pass is
// retention, not correctness.
//
// Not counted toward restricted: a burner label's halt (mail_label_halt), a §21.4 no-strike action (a protective kill
// for an exposed panel, a compromise's halt or kill), a revocation, a forwarded notice, and a quota suspension, which
// is no §21 action at all (plan-QUOTA-small-readings #17).
//
// A state this build does not know reads as itself, and every gate refuses it (AbuseStanding.Known). A §21 standing
// is server state and never a signed statement: a server originates only denials (I-4). Nothing here is read on an
// inline packet or frame path; a gate reads NewAbuseGate's memory (abuse_gate.go), which loads from here off the path
// (I-12). Nothing reads MG5's tables unless the binary links the group (server.SidecarMg5Linked): without it there is
// no row, and a network is on probation or standard by its age alone.

import (
	"context"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A network's §21.7 standing at a time.
type NetworkAbuseStanding struct {
	NetworkId server.Id
	Standing  AbuseStanding
	// The case behind a suspended or restricted standing, nil for any other.
	CaseId *server.Id
	// That case bars every notice (§21.8): GET /abuse/status then shows the standing and never why
	// (plan-ABUSE-small-readings #6).
	Barred bool
	// When the standing changes by time alone: probation's end, a restriction's end. nil when only an action or a
	// review changes it.
	ChangeTime *time.Time
}

// network_abuse_state's row, as a standing reads it.
type networkAbuseRow struct {
	state     string
	caseId    server.Id
	untilTime *time.Time
	barred    bool
}

// What one read of a network gives its standing and, when asked, its network_verified.
type networkAbuseFacts struct {
	createTime time.Time
	// nil when the network has no row
	row      *networkAbuseRow
	verified bool
	// every entry condition of trusted holds (loadNetworkAbuseTrustedWithConn)
	trusted bool
	// microseconds since the unix epoch, Postgres clock: when the read began (pro_model.go's ordering note)
	version int64
}

// The standing at now from a network's creation and its row, QD-194's order. Pure.
func deriveNetworkAbuseStanding(
	networkId server.Id,
	createTime time.Time,
	row *networkAbuseRow,
	trusted bool,
	now time.Time,
	settings *AbuseSettings,
) *NetworkAbuseStanding {
	standing := &NetworkAbuseStanding{NetworkId: networkId}
	withCase := func() {
		caseId := row.caseId
		standing.CaseId = &caseId
		standing.Barred = row.barred
	}
	if row != nil && row.state != networkAbuseStateRestricted {
		// suspended until its review, or a state this build does not know, which every gate refuses
		standing.Standing = AbuseStanding(row.state)
		withCase()
		return standing
	}
	if 0 < settings.ProbationAge {
		probationEnd := createTime.Add(settings.ProbationAge)
		if now.Before(probationEnd) {
			standing.Standing = AbuseStandingProbation
			standing.ChangeTime = &probationEnd
			return standing
		}
	}
	if row != nil && (row.untilTime == nil || now.Before(*row.untilTime)) {
		// a restricted row with no end holds until a pass or an action re-derives it: the closed direction
		standing.Standing = AbuseStandingRestricted
		withCase()
		if row.untilTime != nil {
			untilTime := *row.untilTime
			standing.ChangeTime = &untilTime
		}
		return standing
	}
	if trusted {
		// a grant's lapse, a failed payment or an action ends it, none by time alone
		standing.Standing = AbuseStandingTrusted
		return standing
	}
	standing.Standing = AbuseStandingStandard
	return standing
}

// Each network's facts at now in one statement, its network_verified as well when withVerified, keyed by network. A
// network that does not exist has no entry.
func loadNetworkAbuseFactsWithConn(
	ctx context.Context,
	query server.PgCanQuery,
	networkIds []server.Id,
	now time.Time,
	withVerified bool,
) map[server.Id]*networkAbuseFacts {
	networkIdFacts := map[server.Id]*networkAbuseFacts{}
	if len(networkIds) == 0 {
		return networkIdFacts
	}
	stateColumns := `NULL::varchar, NULL::uuid, NULL::timestamp, false`
	stateJoins := ``
	if server.SidecarMg5Linked() {
		stateColumns = `
			network_abuse_state.state,
			network_abuse_state.case_id,
			network_abuse_state.until_time,
			COALESCE(abuse_case.barred, false)
		`
		stateJoins = `
			LEFT JOIN network_abuse_state ON network_abuse_state.network_id = network.network_id
			LEFT JOIN abuse_case ON abuse_case.case_id = network_abuse_state.case_id
		`
	}
	verifiedColumn := `false`
	args := []any{networkIds}
	if withVerified {
		verifiedColumn = networkVerifiedSql
		args = append(args, now.UTC())
	}
	result, err := query.Query(
		ctx,
		`
			SELECT
				network.network_id,
				network.create_time,
				`+stateColumns+`,
				`+verifiedColumn+`,
				transaction_timestamp()
			FROM network
			`+stateJoins+`
			WHERE network.network_id = ANY($1::uuid[])
		`,
		args...,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			var state *string
			var caseId *server.Id
			var untilTime *time.Time
			var barred bool
			var readTime time.Time
			facts := &networkAbuseFacts{}
			server.Raise(result.Scan(
				&networkId,
				&facts.createTime,
				&state,
				&caseId,
				&untilTime,
				&barred,
				&facts.verified,
				&readTime,
			))
			if state != nil && caseId != nil {
				facts.row = &networkAbuseRow{
					state:     *state,
					caseId:    *caseId,
					untilTime: untilTime,
					barred:    barred,
				}
			}
			facts.version = readTime.UnixMicro()
			networkIdFacts[networkId] = facts
		}
	})
	if 0 < len(networkIdFacts) {
		for networkId := range loadNetworkAbuseTrustedWithConn(ctx, query, networkIds, now, DefaultAbuseSettings()) {
			if facts, ok := networkIdFacts[networkId]; ok {
				facts.trusted = true
			}
		}
	}
	return networkIdFacts
}

// The networks of networkIds every entry condition of trusted holds for at now (the file header's row), through
// query: a verified domain, no unresolved payment failure and no counted action in TrustedCleanWindow in one
// statement, then the Team grant's cover of TrustedPaidWindow, then tier team through the tier's cache. None in a
// binary that does not link MG6, whose network_domain and grant tier it reads, or MG5, whose actions it counts.
func loadNetworkAbuseTrustedWithConn(
	ctx context.Context,
	query server.PgCanQuery,
	networkIds []server.Id,
	now time.Time,
	settings *AbuseSettings,
) map[server.Id]bool {
	trustedNetworkIds := map[server.Id]bool{}
	if !server.SidecarMg5Linked() || !server.SidecarMg6Linked() || len(networkIds) == 0 ||
		settings.TrustedPaidWindow <= 0 || settings.TrustedCleanWindow <= 0 {
		return trustedNetworkIds
	}
	now = now.UTC()
	candidateNetworkIds := []server.Id{}
	result, err := query.Query(
		ctx,
		`
			SELECT network.network_id
			FROM network
			WHERE
				network.network_id = ANY($1::uuid[]) AND
				EXISTS (
					SELECT 1
					FROM network_domain
					WHERE network_domain.network_id = network.network_id AND network_domain.verify_time IS NOT NULL
				) AND
				NOT EXISTS (
					SELECT 1
					FROM stripe_invoice_payment_failure
					WHERE stripe_invoice_payment_failure.network_id = network.network_id AND stripe_invoice_payment_failure.resolve_time IS NULL
				) AND
				NOT EXISTS (
					SELECT 1
					FROM abuse_action
					WHERE
						abuse_action.network_id = network.network_id AND
						$2 <= abuse_action.create_time AND
						abuse_action.kind NOT IN ($3, $4) AND
						abuse_action.review_state <> $5
				)
		`,
		networkIds,
		now.Add(-settings.TrustedCleanWindow),
		string(AbuseActionKindLift),
		string(AbuseActionKindScreenAllow),
		string(AbuseReviewStateLiftedWrong),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			server.Raise(result.Scan(&networkId))
			candidateNetworkIds = append(candidateNetworkIds, networkId)
		}
	})
	if len(candidateNetworkIds) == 0 {
		return trustedNetworkIds
	}
	// the Team grant's windows over the paid window, oldest first, to find a gap
	paidFrom := now.Add(-settings.TrustedPaidWindow)
	networkIdCoverEnds := map[server.Id]time.Time{}
	networkIdGaps := map[server.Id]bool{}
	result, err = query.Query(
		ctx,
		`
			SELECT network_id, start_time, end_time
			FROM transfer_balance
			WHERE
				network_id = ANY($1::uuid[]) AND
				tier = $2 AND
				grant_kind IS DISTINCT FROM $3 AND
				start_time <= $4 AND
				$5 < end_time
			ORDER BY network_id, start_time, end_time
		`,
		candidateNetworkIds,
		string(NetworkTierTeam),
		string(GrantKindTeamCredit),
		now,
		paidFrom,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var networkId server.Id
			var startTime, endTime time.Time
			server.Raise(result.Scan(&networkId, &startTime, &endTime))
			coverEnd, ok := networkIdCoverEnds[networkId]
			if !ok {
				coverEnd = paidFrom
			}
			if coverEnd.Before(startTime) {
				networkIdGaps[networkId] = true
			}
			if coverEnd.Before(endTime) {
				coverEnd = endTime
			}
			networkIdCoverEnds[networkId] = coverEnd
		}
	})
	for _, networkId := range candidateNetworkIds {
		coverEnd, ok := networkIdCoverEnds[networkId]
		if !ok || networkIdGaps[networkId] || !now.Before(coverEnd) {
			continue
		}
		if networkTierOfWithQuery(ctx, query, networkId) != NetworkTierTeam {
			continue
		}
		trustedNetworkIds[networkId] = true
	}
	return trustedNetworkIds
}

// The network's §21.7 standing at now, read from Postgres, found false for a network that does not exist. Not for an
// inline path: a gate reads NewAbuseGate's memory (I-12).
func GetNetworkAbuseStanding(ctx context.Context, networkId server.Id, now time.Time) (standing *NetworkAbuseStanding, found bool) {
	settings := DefaultAbuseSettings()
	server.Db(ctx, func(conn server.PgConn) {
		facts, ok := loadNetworkAbuseFactsWithConn(ctx, conn, []server.Id{networkId}, now, false)[networkId]
		if !ok {
			return
		}
		standing = deriveNetworkAbuseStanding(networkId, facts.createTime, facts.row, facts.trusted, now, settings)
		found = true
	})
	return
}

// The action kinds that restrict a network for RestrictedWindow (plan-ABUSE-small-readings #4): a host, label or
// network kill, in its TLS or its DNS form, and a network halt. Enum values, so a list and no tunable.
func abuseRestrictingActionKinds() []string {
	return []string{
		string(AbuseActionKindHostKill),
		string(AbuseActionKindHostSuspend),
		string(AbuseActionKindLabelKill),
		string(AbuseActionKindZoneSuspend),
		string(AbuseActionKindNetworkSuspend),
		string(AbuseActionKindMailHalt),
	}
}

// The §21.4 rows whose actions strike nothing: a protective kill for an exposed panel, a compromise's halt or kill.
func abuseNoStrikeCategories() []string {
	return []string{
		string(AbuseCategoryExposedPanel),
		string(AbuseCategoryCompromise),
	}
}

// Whether an action restricts its network for RestrictedWindow from its creation, as
// ReevaluateNetworkAbuseStateInTx's query selects it. Pure.
func AbuseActionRestricts(kind AbuseActionKind, category AbuseCategory, reviewState AbuseReviewState) bool {
	if reviewState == AbuseReviewStateLiftedWrong {
		return false
	}
	restricts := false
	for _, restrictingKind := range abuseRestrictingActionKinds() {
		restricts = restricts || string(kind) == restrictingKind
	}
	for _, noStrikeCategory := range abuseNoStrikeCategories() {
		restricts = restricts && string(category) != noStrikeCategory
	}
	return restricts
}

// Re-derives the network's restricted row at now in the caller's transaction, locking the row to its end: restricted
// until the newest restricting action's creation plus RestrictedWindow, under that action's case, while that is ahead,
// and no row once it is not. A suspended row, or one in a state this build does not know, is left as it is: the
// effects own a suspension's row. restricted is whether a restricted row stands afterwards, and changed whether this
// wrote. Nothing in a binary that does not link MG5.
func ReevaluateNetworkAbuseStateInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	now time.Time,
	settings *AbuseSettings,
) (restricted bool, changed bool) {
	if !server.SidecarMg5Linked() {
		return false, false
	}
	now = now.UTC()
	var row *networkAbuseRow
	result, err := tx.Query(
		ctx,
		`
			SELECT
				network_abuse_state.state,
				network_abuse_state.case_id,
				network_abuse_state.until_time
			FROM network_abuse_state
			WHERE network_abuse_state.network_id = $1
			FOR UPDATE
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			row = &networkAbuseRow{}
			server.Raise(result.Scan(&row.state, &row.caseId, &row.untilTime))
		}
	})
	if row != nil && row.state != networkAbuseStateRestricted {
		return false, false
	}

	var restrictCaseId server.Id
	var restrictTime time.Time
	restricts := false
	if 0 < settings.RestrictedWindow {
		result, err := tx.Query(
			ctx,
			`
				SELECT
					abuse_action.case_id,
					abuse_action.create_time
				FROM abuse_action
				WHERE
					abuse_action.network_id = $1 AND
					$2 < abuse_action.create_time AND
					abuse_action.kind = ANY($3::varchar[]) AND
					abuse_action.category <> ALL($4::varchar[]) AND
					abuse_action.review_state <> $5
				ORDER BY abuse_action.create_time DESC, abuse_action.action_id DESC
				LIMIT 1
			`,
			networkId,
			now.Add(-settings.RestrictedWindow),
			abuseRestrictingActionKinds(),
			abuseNoStrikeCategories(),
			string(AbuseReviewStateLiftedWrong),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&restrictCaseId, &restrictTime))
				restricts = true
			}
		})
	}

	if restricts {
		untilTime := restrictTime.Add(settings.RestrictedWindow).UTC()
		if row != nil && row.caseId == restrictCaseId && row.untilTime != nil && row.untilTime.Equal(untilTime) {
			return true, false
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_abuse_state (
					network_id,
					state,
					case_id,
					until_time,
					update_time
				) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (network_id) DO UPDATE
				SET
					case_id = EXCLUDED.case_id,
					until_time = EXCLUDED.until_time,
					update_time = EXCLUDED.update_time
				WHERE network_abuse_state.state = $2
			`,
			networkId,
			networkAbuseStateRestricted,
			restrictCaseId,
			untilTime,
			now,
		))
		return true, true
	}
	if row == nil {
		return false, false
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM network_abuse_state
			WHERE
				network_abuse_state.network_id = $1 AND
				network_abuse_state.state = $2
		`,
		networkId,
		networkAbuseStateRestricted,
	))
	return false, true
}

// ReevaluateNetworkAbuseStateInTx in a transaction of its own.
func ReevaluateNetworkAbuseState(ctx context.Context, networkId server.Id, now time.Time) (restricted bool, changed bool) {
	settings := DefaultAbuseSettings()
	server.Tx(ctx, func(tx server.PgTx) {
		restricted, changed = ReevaluateNetworkAbuseStateInTx(ctx, tx, networkId, now, settings)
	})
	return
}

// What a §21.7 standing allows beyond the network's tier (§21.7's Effect column), for the routes and gates that
// enforce it: the tls_endpoints count (QUOTA-M5-01), a publish's expiry (TLS), DNS record writes (DNS-M5-12), names
// held for review (ABUSE-M5-08), aliases, outbound shares (BRIDGE-M6-02) and mail (SMTP-M9-10). Each caller enforces
// its own field; nothing here refuses anything.
type AbuseStandingLimitSet struct {
	Standing AbuseStanding
	// Whether the network has reach, publishing and sending at all: false for suspended and for a standing this build
	// does not know.
	Reach bool
	// The most TLS endpoints, -1 for no cap beyond its tier's: ProbationTlsEndpoints on probation and restricted.
	TlsEndpoints int64
	// The longest a publish runs, zero for no cap beyond §13.9's: ProbationPublishMaxDuration on probation and
	// restricted.
	PublishMaxDuration time.Duration
	// The most user RRsets, -1 for no cap beyond dns_entries: probation_dns_entries on probation and restricted.
	DnsEntries int64
	// Whether user RRsets may be A, AAAA or CNAME.
	DnsAddressRecords bool
	// Whether a name may wait for review (§21.6), alias another, or be shared outbound (§17.9).
	HeldNames      bool
	Aliases        bool
	OutboundShares bool
	// Whether mail may be deposited only from the same network (D-SMTP-14): probation's.
	SameNetworkMailOnly bool
	// Whether internet mail, once its halt ends, sends at Community caps whatever the tier: restricted's.
	CommunityMailCaps bool
	// Team thresholds and brand use: trusted's.
	BrandUse bool
}

// §21.7's table for one standing. A standing this build does not know takes suspended's row, the closed direction.
// The DNS count reads pro.yml's sidecar_abuse: probation_dns_entries, which reads zero, none, while pro.yml has none.
func AbuseStandingLimits(standing AbuseStanding, settings *AbuseSettings) *AbuseStandingLimitSet {
	limits := &AbuseStandingLimitSet{Standing: standing}
	probationLimits := func() {
		limits.Reach = true
		limits.TlsEndpoints = settings.ProbationTlsEndpoints
		limits.PublishMaxDuration = settings.ProbationPublishMaxDuration
		limits.DnsEntries = Pro().SidecarAbuse.Limit(DnsProbationEntriesLimitKey)
	}
	tierLimits := func() {
		limits.Reach = true
		limits.TlsEndpoints = proSidecarLimitNone
		limits.DnsEntries = proSidecarLimitNone
		limits.DnsAddressRecords = true
		limits.HeldNames = true
		limits.Aliases = true
		limits.OutboundShares = true
	}
	switch standing {
	case AbuseStandingProbation:
		probationLimits()
		limits.SameNetworkMailOnly = true
	case AbuseStandingRestricted:
		probationLimits()
		limits.CommunityMailCaps = true
	case AbuseStandingStandard:
		tierLimits()
	case AbuseStandingTrusted:
		tierLimits()
		limits.BrandUse = true
	}
	// suspended and an unknown standing keep every zero: no reach, no endpoint, no record
	return limits
}

// What one AbuseStandingPass did.
type AbuseStandingPassResult struct {
	// rows past their until_time that the pass re-derived
	ReevaluatedCount int
	// of those, rows removed because no restricting action is left in the window
	RemovedCount int
	// of those, rows a newer restricting action kept restricted past now
	ExtendedCount int
}

// Where a pass over network_abuse_state stands: the last row it took, in until_time then network order.
type networkAbuseStateCursor struct {
	untilTime time.Time
	networkId server.Id
}

// Up to limit restricted rows whose until_time is at or before now, past cursor, and the cursor after them.
func listExpiredNetworkAbuseStates(
	ctx context.Context,
	now time.Time,
	cursor networkAbuseStateCursor,
	limit int,
) (networkIds []server.Id, next networkAbuseStateCursor) {
	networkIds = []server.Id{}
	next = cursor
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_abuse_state.network_id,
					network_abuse_state.until_time
				FROM network_abuse_state
				WHERE
					network_abuse_state.state = $1 AND
					network_abuse_state.until_time <= $2 AND
					(network_abuse_state.until_time, network_abuse_state.network_id) > ($3, $4)
				ORDER BY network_abuse_state.until_time, network_abuse_state.network_id
				LIMIT $5
			`,
			networkAbuseStateRestricted,
			now.UTC(),
			cursor.untilTime.UTC(),
			cursor.networkId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var untilTime time.Time
				server.Raise(result.Scan(&networkId, &untilTime))
				networkIds = append(networkIds, networkId)
				next = networkAbuseStateCursor{untilTime: untilTime, networkId: networkId}
			}
		})
	})
	return
}

// One pass over the restricted rows past their until_time (§21.10's "only while current", QD-194's daily
// re-evaluation): each is re-derived in its own transaction, so it goes, or a newer action keeps it, in batches of
// StandingPassBatchSize by a cursor, until none is left, StandingPassMaxTime has passed or ctx ends. A binary that
// does not link MG5 does nothing.
func RunAbuseStandingPass(ctx context.Context, now time.Time, settings *AbuseSettings) *AbuseStandingPassResult {
	result := &AbuseStandingPassResult{}
	if !server.SidecarMg5Linked() || settings.StandingPassBatchSize <= 0 {
		return result
	}
	startTime := time.Now()
	cursor := networkAbuseStateCursor{}
	for {
		if ctx.Err() != nil || (0 < settings.StandingPassMaxTime && settings.StandingPassMaxTime <= time.Since(startTime)) {
			return result
		}
		networkIds, next := listExpiredNetworkAbuseStates(ctx, now, cursor, settings.StandingPassBatchSize)
		if len(networkIds) == 0 {
			return result
		}
		cursor = next
		for _, networkId := range networkIds {
			if ctx.Err() != nil {
				return result
			}
			var restricted, changed bool
			server.Tx(ctx, func(tx server.PgTx) {
				restricted, changed = ReevaluateNetworkAbuseStateInTx(ctx, tx, networkId, now, settings)
			})
			result.ReevaluatedCount += 1
			switch {
			case changed && restricted:
				result.ExtendedCount += 1
			case changed:
				result.RemovedCount += 1
			}
		}
	}
}

// Installs §21.7's reads in a process that holds Postgres, at api's and taskworker's startup: one hard deny snapshot
// and a NewAbuseGate over it, which DNS's record writes read (SetDnsRecordsAbuseGate) and an outbound share's check
// reads, from memory, since its caller holds a transaction (SetNetworkBridgeShareOutboundCheck); the review clock's
// tier and standing for a report (SetAbuseReportStandingSource); and the tls_endpoints count's cap
// (SetTlsEndpointStandingLimit), read from Postgres at a grant's activation. Answers the snapshot, which this
// process's CreateContract shares (controller.SetAbuseHardDeny), and what puts the previous reads back and stops the
// gate and the snapshot.
func InstallAbuseStanding(ctx context.Context) (hardDeny *AbuseHardDenySnapshot, restore func()) {
	settings := DefaultAbuseSettings()
	hardDeny = NewAbuseHardDenySnapshot(ctx, nil)
	gate := NewAbuseGate(ctx, hardDeny, nil)

	restoreRecords := SetDnsRecordsAbuseGate(gate)
	restoreReports := SetAbuseReportStandingSource(func(ctx context.Context, networkId server.Id) (AbuseTier, AbuseStanding) {
		standing, found := GetNetworkAbuseStanding(ctx, networkId, server.NowUtc())
		if !found {
			return "", ""
		}
		return AbuseTier(NetworkTierOf(ctx, networkId)), standing.Standing
	})
	SetTlsEndpointStandingLimit(func(ctx context.Context, networkId server.Id) (int64, error) {
		standing, found := GetNetworkAbuseStanding(ctx, networkId, server.NowUtc())
		if !found {
			return 0, ErrAbuseStandingUnknown
		}
		return AbuseStandingLimits(standing.Standing, settings).TlsEndpoints, nil
	})
	SetNetworkBridgeShareOutboundCheck(func(ctx context.Context, networkId server.Id) (bool, error) {
		standing, ok := gate.Standing(connect.Id(networkId))
		if !ok {
			return false, ErrAbuseStandingUnknown
		}
		return AbuseStandingLimits(standing, settings).OutboundShares, nil
	})
	restore = func() {
		SetNetworkBridgeShareOutboundCheck(nil)
		SetTlsEndpointStandingLimit(nil)
		restoreReports()
		restoreRecords()
		gate.Close()
		hardDeny.Close()
	}
	return
}
