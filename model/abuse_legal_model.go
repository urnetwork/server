package model

// §21.9's legal requests (ABUSE-M5-17; D-ABUSE-6, D-ABUSE-7, R-ABUSE-5, Q-ABUSE-5, I-13; owner decision 93; QD-126,
// QD-916): legal_request records one order or request as counsel files it, clocks it, and records who signed a
// production off. Counsel reviews each order, so nothing here judges one: it records and clocks.
//
//   - FileLegalRequest records the instrument, the issuing country, the accounts named (network ids), whether a sealing
//     or nondisclosure order bars notice and until when, and a due time: an EU production order's 10 days, or 8 h in
//     an emergency (Regulation 2023/1543), counsel's own time for any other instrument. It hangs off a case: the one
//     counsel names, or a new case of category other, reporter class law_enforcement, barred when the request is
//     sealed, so §21.8's notices stay silent.
//   - SignLegalProduction records one sign-off: a legal one and an engineering one, each by a staff member holding
//     that role, never both by one person (§21.10, QD-126). A production exports nothing until both stand
//     (abuse_production_model.go).
//   - CloseLegalRequest ends one: produced stays produced, and one never produced is declined. Its 3 years run from
//     then (§21.10, AbuseRetention).
//
// A sealed request stays sealed: nothing here writes a hint, a notice, an admin event or a log line naming a network
// (owner decisions 93, 184). Notice to Owners and Admins, before production unless barred and after a seal lapses, is
// counsel's call to send, never automatic. Nothing reads or writes the table unless the binary links MG5
// (server.SidecarMg5Linked).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

// legal_request.instrument: what kind of order or request it is, the transparency report's first key (§21.11). A
// fixed list, so the counts' rows are fixed too; counsel files anything else as other.
type LegalInstrument string

const (
	// 18 U.S.C. §2703(c)(2)
	LegalInstrumentSubpoena LegalInstrument = "subpoena"
	// §2703(d)
	LegalInstrumentCourtOrder LegalInstrument = "court_order"
	// §2703(a), (b)
	LegalInstrumentSearchWarrant LegalInstrument = "search_warrant"
	// §3123's pen register or trap and trace: no prospective collection exists, so it reaches only what is retained
	LegalInstrumentPenRegister LegalInstrument = "pen_register"
	// §2703(f): a preservation request, its hold placed through place_hold
	LegalInstrumentPreservation LegalInstrument = "preservation"
	// §2702(b)(8), (c)(4)
	LegalInstrumentEmergency LegalInstrument = "emergency"
	// Regulation 2023/1543's production order, with its 10 days or 8 h
	LegalInstrumentEuProductionOrder LegalInstrument = "eu_production_order"
	// Regulation 2023/1543's preservation order
	LegalInstrumentEuPreservationOrder LegalInstrument = "eu_preservation_order"
	// a mutual legal assistance request or another country's order
	LegalInstrumentForeign LegalInstrument = "foreign"
	LegalInstrumentOther   LegalInstrument = "other"
)

// Every instrument, in the report's order.
func LegalInstruments() []LegalInstrument {
	return []LegalInstrument{
		LegalInstrumentSubpoena,
		LegalInstrumentCourtOrder,
		LegalInstrumentSearchWarrant,
		LegalInstrumentPenRegister,
		LegalInstrumentPreservation,
		LegalInstrumentEmergency,
		LegalInstrumentEuProductionOrder,
		LegalInstrumentEuPreservationOrder,
		LegalInstrumentForeign,
		LegalInstrumentOther,
	}
}

// legal_request.production_state.
const (
	// filed; nothing exported yet
	LegalProductionStatePending = "pending"
	// exported at least once
	LegalProductionStateProduced = "produced"
	// closed with nothing exported
	LegalProductionStateDeclined = "declined"
)

// Who signs a production off (§21.10): one of each, never one person both.
type LegalSignoffRole string

const (
	LegalSignoffRoleLegal       LegalSignoffRole = "legal"
	LegalSignoffRoleEngineering LegalSignoffRole = "engineering"
)

// Arguments no row may hold. The message names no network and no value (I-13).
var ErrLegalRequestInvalid = errors.New("legal request invalid")

// No request has the id.
var ErrLegalRequestNotFound = errors.New("legal request not found")

// The id names a row that is not this request.
var ErrLegalRequestConflict = errors.New("legal request id conflict")

// The request is closed: nothing more is signed or produced under it.
var ErrLegalRequestClosed = errors.New("legal request closed")

// A sign-off the request cannot take: its role signed already by someone else, or the other role's signer signing
// this one too.
var ErrLegalSignoffRefused = errors.New("legal sign-off refused")

// Tunables of the legal requests.
type LegalSettings struct {
	// Regulation 2023/1543's time to produce for an EU production order: 10 days.
	EuProductionDue time.Duration
	// The same in an emergency: 8 hours.
	EuProductionEmergencyDue time.Duration
	// The most accounts one request names. 256: an order naming more is filed as several, so a production stays a
	// file counsel can read.
	RequestNetworkMaxCount int
}

// The regulation's clocks and the account bound.
func DefaultLegalSettings() *LegalSettings {
	return &LegalSettings{
		EuProductionDue:          10 * 24 * time.Hour,
		EuProductionEmergencyDue: 8 * time.Hour,
		RequestNetworkMaxCount:   256,
	}
}

// A request to file.
type LegalRequestArgs struct {
	// the caller's, so a retry records one row
	RequestId server.Id
	// the case it belongs to; nil opens one
	CaseId     *server.Id
	Instrument LegalInstrument
	// the issuing authority's country, ISO 3166-1 alpha-2, uppercase
	Country string
	// the accounts the request names, each a network that exists
	NetworkIds []server.Id
	// a sealing or nondisclosure order bars every notice (18 U.S.C. §2705(b), §3123(d))
	Sealed bool
	// when the seal lapses; nil while it runs until lifted. Only with Sealed.
	SealUntil *time.Time
	// an EU production order's emergency, 8 h. Only for that instrument.
	Emergency bool
	// counsel's own due time, for an instrument this has no clock for
	DueTime *time.Time
}

// One request as legal_request holds it.
type LegalRequest struct {
	RequestId            server.Id
	CaseId               server.Id
	Instrument           LegalInstrument
	Country              string
	NetworkIds           []server.Id
	Sealed               bool
	SealUntil            *time.Time
	DueTime              *time.Time
	ProductionState      string
	LegalSignoffId       *server.Id
	EngineeringSignoffId *server.Id
	CreateTime           time.Time
	CloseTime            *time.Time
}

// Whether the seal stands at now: sealed, with no end or an end ahead.
func (self *LegalRequest) SealedAt(now time.Time) bool {
	return self.Sealed && (self.SealUntil == nil || now.Before(*self.SealUntil))
}

// Whether both sign-offs stand, by two people (§21.10).
func (self *LegalRequest) SignedOff() bool {
	return self.LegalSignoffId != nil && self.EngineeringSignoffId != nil && *self.LegalSignoffId != *self.EngineeringSignoffId
}

// Whether the instrument is one this build files.
func (self LegalInstrument) known() bool {
	return slices.Contains(LegalInstruments(), self)
}

// Whether country is two uppercase ASCII letters.
func isLegalCountry(country string) bool {
	if len(country) != 2 {
		return false
	}
	for index := range len(country) {
		if country[index] < 'A' || 'Z' < country[index] {
			return false
		}
	}
	return true
}

// Refuses what no row may hold.
func (self *LegalRequestArgs) validate(settings *LegalSettings) error {
	switch {
	case self.RequestId == (server.Id{}):
		return fmt.Errorf("%w: no request id", ErrLegalRequestInvalid)
	case !self.Instrument.known():
		return fmt.Errorf("%w: an unknown instrument", ErrLegalRequestInvalid)
	case !isLegalCountry(self.Country):
		return fmt.Errorf("%w: a country that is not two uppercase letters", ErrLegalRequestInvalid)
	case settings.RequestNetworkMaxCount < len(self.NetworkIds):
		return fmt.Errorf("%w: %d accounts", ErrLegalRequestInvalid, len(self.NetworkIds))
	case self.SealUntil != nil && !self.Sealed:
		return fmt.Errorf("%w: a seal's end with no seal", ErrLegalRequestInvalid)
	case self.Emergency && self.Instrument != LegalInstrumentEuProductionOrder:
		return fmt.Errorf("%w: an emergency clock on an instrument with none", ErrLegalRequestInvalid)
	}
	seen := map[server.Id]bool{}
	for _, networkId := range self.NetworkIds {
		if networkId == (server.Id{}) || seen[networkId] {
			return fmt.Errorf("%w: an account named twice or not at all", ErrLegalRequestInvalid)
		}
		seen[networkId] = true
	}
	return nil
}

// The due time of a request filed at now: the regulation's for an EU production order, else counsel's, else none.
func legalRequestDueTime(args *LegalRequestArgs, now time.Time, settings *LegalSettings) *time.Time {
	if args.Instrument == LegalInstrumentEuProductionOrder {
		due := now.Add(settings.EuProductionDue)
		if args.Emergency {
			due = now.Add(settings.EuProductionEmergencyDue)
		}
		due = due.UTC()
		if args.DueTime != nil && args.DueTime.Before(due) {
			due = args.DueTime.UTC()
		}
		return &due
	}
	if args.DueTime != nil {
		due := args.DueTime.UTC()
		return &due
	}
	return nil
}

// Every column of a request, in scanLegalRequest's order.
const legalRequestColumns = `
	legal_request.request_id,
	legal_request.case_id,
	legal_request.instrument,
	legal_request.country,
	legal_request.network_ids,
	legal_request.sealed,
	legal_request.seal_until,
	legal_request.due_time,
	legal_request.production_state,
	legal_request.legal_signoff_id,
	legal_request.engineering_signoff_id,
	legal_request.create_time,
	legal_request.close_time
`

// One row of legalRequestColumns.
func scanLegalRequest(result server.PgResult) *LegalRequest {
	request := &LegalRequest{}
	var instrument string
	server.Raise(result.Scan(
		&request.RequestId,
		&request.CaseId,
		&instrument,
		&request.Country,
		&request.NetworkIds,
		&request.Sealed,
		&request.SealUntil,
		&request.DueTime,
		&request.ProductionState,
		&request.LegalSignoffId,
		&request.EngineeringSignoffId,
		&request.CreateTime,
		&request.CloseTime,
	))
	request.Instrument = LegalInstrument(instrument)
	if request.NetworkIds == nil {
		request.NetworkIds = []server.Id{}
	}
	return request
}

// The request, read in the caller's transaction and locked to its end when forUpdate, or nil.
func getLegalRequestInTx(ctx context.Context, tx server.PgTx, requestId server.Id, forUpdate bool) *LegalRequest {
	sql := `SELECT ` + legalRequestColumns + ` FROM legal_request WHERE legal_request.request_id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var request *LegalRequest
	result, err := tx.Query(ctx, sql, requestId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			request = scanLegalRequest(result)
		}
	})
	return request
}

// The request, or nil, including nil in a binary that does not link MG5. A machine read, which the production and
// the staff route reach through model.AbuseReader.
func getLegalRequest(ctx context.Context, requestId server.Id) (request *LegalRequest) {
	if !server.SidecarMg5Linked() {
		return nil
	}
	server.Tx(ctx, func(tx server.PgTx) {
		request = getLegalRequestInTx(ctx, tx, requestId, false)
	})
	return
}

// Files a request at now, the path in the file header. A second call with the same id adds nothing and answers the
// row there, inserted false, or ErrLegalRequestConflict when that row is another request.
func FileLegalRequest(ctx context.Context, args *LegalRequestArgs, now time.Time, settings *LegalSettings) (request *LegalRequest, inserted bool, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, false, ErrAbuseUnlinked
	}
	if err := args.validate(settings); err != nil {
		return nil, false, err
	}
	now = now.UTC()
	networkIds := slices.Clone(args.NetworkIds)
	slices.SortFunc(networkIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	dueTime := legalRequestDueTime(args, now, settings)
	server.Tx(ctx, func(tx server.PgTx) {
		if existing := getLegalRequestInTx(ctx, tx, args.RequestId, false); existing != nil {
			if existing.Instrument != args.Instrument || existing.Country != args.Country || !slices.Equal(existing.NetworkIds, networkIds) ||
				existing.Sealed != args.Sealed || (args.CaseId != nil && existing.CaseId != *args.CaseId) {
				returnErr = ErrLegalRequestConflict
				return
			}
			request = existing
			return
		}
		if 0 < len(networkIds) {
			var foundCount int
			server.Raise(tx.QueryRow(
				ctx,
				`SELECT COUNT(*) FROM network WHERE network_id = ANY($1::uuid[])`,
				networkIds,
			).Scan(&foundCount))
			if foundCount != len(networkIds) {
				returnErr = fmt.Errorf("%w: an account that does not exist", ErrLegalRequestInvalid)
				return
			}
		}
		var caseId server.Id
		if args.CaseId != nil {
			abuseCase := getAbuseCaseInTx(ctx, tx, *args.CaseId, true)
			if abuseCase == nil {
				returnErr = ErrAbuseCaseNotFound
				return
			}
			caseId = abuseCase.CaseId
		} else {
			// a case of its own, never coalesced with another, barred while sealed
			caseArgs := &AbuseCaseArgs{
				Category:      AbuseCategoryOther,
				Target:        AbuseTarget{Kind: AbuseTargetKindNetwork, Name: args.RequestId.String()},
				ReporterClass: AbuseReporterClassLawEnforcement,
				JoinKind:      AbuseJoinKindNetwork,
				JoinKey:       args.RequestId.String(),
				Barred:        args.Sealed,
			}
			if len(networkIds) == 1 {
				networkId := networkIds[0]
				caseArgs.NetworkId = &networkId
				caseArgs.Target.Name = networkId.String()
				caseArgs.JoinKey = networkId.String()
			}
			caseSettings := *DefaultAbuseSettings()
			caseSettings.CaseCoalesceWindow = 0
			var err error
			caseId, _, err = OpenAbuseCaseInTx(ctx, tx, caseArgs, now, &caseSettings)
			if err != nil {
				returnErr = err
				return
			}
		}
		var sealUntil *time.Time
		if args.SealUntil != nil {
			until := args.SealUntil.UTC()
			sealUntil = &until
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO legal_request (
					request_id,
					case_id,
					instrument,
					country,
					network_ids,
					sealed,
					seal_until,
					due_time,
					production_state,
					create_time
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			`,
			args.RequestId,
			caseId,
			string(args.Instrument),
			args.Country,
			networkIds,
			args.Sealed,
			sealUntil,
			dueTime,
			LegalProductionStatePending,
			now,
		))
		request = getLegalRequestInTx(ctx, tx, args.RequestId, false)
		inserted = true
	})
	if returnErr != nil {
		return nil, false, returnErr
	}
	return request, inserted, nil
}

// Records reviewerId's sign-off in role on the request (§21.10): refused on a closed request, for a role someone else
// signed already, and for a reviewer who signed the other role. The same reviewer's same sign-off again changes
// nothing.
func SignLegalProduction(ctx context.Context, requestId server.Id, reviewerId server.Id, role LegalSignoffRole) (request *LegalRequest, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if reviewerId == (server.Id{}) {
		return nil, fmt.Errorf("%w: no reviewer", ErrLegalSignoffRefused)
	}
	column := ""
	switch role {
	case LegalSignoffRoleLegal:
		column = "legal_signoff_id"
	case LegalSignoffRoleEngineering:
		column = "engineering_signoff_id"
	default:
		return nil, fmt.Errorf("%w: an unknown role", ErrLegalSignoffRefused)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		request = getLegalRequestInTx(ctx, tx, requestId, true)
		if request == nil {
			returnErr = ErrLegalRequestNotFound
			return
		}
		if request.CloseTime != nil {
			returnErr = ErrLegalRequestClosed
			return
		}
		mine, other := request.LegalSignoffId, request.EngineeringSignoffId
		if role == LegalSignoffRoleEngineering {
			mine, other = other, mine
		}
		if other != nil && *other == reviewerId {
			returnErr = fmt.Errorf("%w: one person signs one role", ErrLegalSignoffRefused)
			return
		}
		if mine != nil {
			if *mine != reviewerId {
				returnErr = fmt.Errorf("%w: the role is signed", ErrLegalSignoffRefused)
			}
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE legal_request SET `+column+` = $2 WHERE request_id = $1`,
			requestId,
			reviewerId,
		))
		request = getLegalRequestInTx(ctx, tx, requestId, false)
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return request, nil
}

// Closes the request at now: a produced one stays produced, a pending one is declined. A closed one stays as it is.
func CloseLegalRequest(ctx context.Context, requestId server.Id, now time.Time) (request *LegalRequest, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	server.Tx(ctx, func(tx server.PgTx) {
		request = getLegalRequestInTx(ctx, tx, requestId, true)
		if request == nil {
			returnErr = ErrLegalRequestNotFound
			return
		}
		if request.CloseTime != nil {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE legal_request
				SET
					close_time = $2,
					production_state = CASE WHEN production_state = $3 THEN $4 ELSE production_state END
				WHERE request_id = $1
			`,
			requestId,
			now.UTC(),
			LegalProductionStatePending,
			LegalProductionStateDeclined,
		))
		request = getLegalRequestInTx(ctx, tx, requestId, false)
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return request, nil
}

// The requests past their due time at now and not closed, oldest due first, at most limit: counsel's clock, which
// warns and does nothing else.
func ListOverdueLegalRequests(ctx context.Context, now time.Time, limit int) (requests []*LegalRequest) {
	requests = []*LegalRequest{}
	if !server.SidecarMg5Linked() || limit <= 0 {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+legalRequestColumns+`
				FROM legal_request
				WHERE legal_request.close_time IS NULL AND legal_request.due_time <= $1
				ORDER BY legal_request.due_time, legal_request.request_id
				LIMIT $2
			`,
			now.UTC(),
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				requests = append(requests, scanLegalRequest(result))
			}
		})
	})
	return
}

// Marks the request produced in the caller's transaction.
func setLegalRequestProducedInTx(ctx context.Context, tx server.PgTx, requestId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`UPDATE legal_request SET production_state = $2 WHERE request_id = $1`,
		requestId,
		LegalProductionStateProduced,
	))
}
