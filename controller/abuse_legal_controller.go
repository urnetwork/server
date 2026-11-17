package controller

// §21.9's legal process on the staff route (ABUSE-M5-17; D-ABUSE-6, D-ABUSE-7, R-ABUSE-5, QD-126, QD-916): the kinds
// POST /abuse/action takes for legal requests, productions and the transparency counts, beside the table in
// abuse_staff_controller.go:
//
//	kind                  roles                what it does
//	file_legal_request    legal                records an order or request and clocks it (model.FileLegalRequest)
//	sign_production       legal, engineering   one sign-off, in a role the member holds (model.SignLegalProduction)
//	produce               legal                the production for counsel, both sign-offs standing, its read logged
//	                                           (model.AbuseReader.ProduceLegalRequest)
//	close_legal_request   legal                ends a request, declined if never produced
//	transparency_counts   legal                §21.11's totals over a window, the read logged
//
// The answer of produce is the file counsel keeps, which sidecarctl abuse production writes out; nothing here logs a
// network, an instrument or a country (I-13), and a sealed request writes no hint, notice or admin event (owner
// decisions 93, 184).

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The legal kinds' seams, which a test replaces.
var abuseLegalDeps = struct {
	legalSettings      func() *model.LegalSettings
	productionSettings func() *model.AbuseProductionSettings
}{
	legalSettings:      model.DefaultLegalSettings,
	productionSettings: model.DefaultAbuseProductionSettings,
}

// file_legal_request's body.
type abuseStaffFileLegalRequestArgs struct {
	Kind string `json:"kind"`
	// the caller's id for the request, so a retry records it once; absent draws one
	RequestId *server.Id `json:"request_id"`
	// the case it belongs to; absent opens one
	CaseId     *server.Id  `json:"case_id"`
	Instrument string      `json:"instrument"`
	Country    string      `json:"country"`
	NetworkIds []server.Id `json:"network_ids"`
	Sealed     bool        `json:"sealed"`
	// when the seal lapses, 0 while it runs until lifted
	SealUntilMs int64 `json:"seal_until_ms"`
	// an EU production order's emergency
	Emergency bool `json:"emergency"`
	// counsel's own due time, 0 for none
	DueTimeMs int64 `json:"due_time_ms"`
}

// sign_production's body.
type abuseStaffSignProductionArgs struct {
	Kind      string    `json:"kind"`
	RequestId server.Id `json:"request_id"`
	// legal or engineering
	Role string `json:"role"`
}

// produce's and close_legal_request's body.
type abuseStaffLegalRequestArgs struct {
	Kind      string    `json:"kind"`
	RequestId server.Id `json:"request_id"`
}

// transparency_counts' body: the window [from, to).
type abuseStaffTransparencyCountsArgs struct {
	Kind       string `json:"kind"`
	FromTimeMs int64  `json:"from_time_ms"`
	ToTimeMs   int64  `json:"to_time_ms"`
}

// A legal request as the staff route answers it.
type AbuseStaffLegalRequestView struct {
	RequestId            server.Id   `json:"request_id"`
	CaseId               server.Id   `json:"case_id"`
	Instrument           string      `json:"instrument"`
	Country              string      `json:"country"`
	NetworkIds           []server.Id `json:"network_ids"`
	Sealed               bool        `json:"sealed"`
	SealUntilMs          int64       `json:"seal_until_ms"`
	DueTimeMs            int64       `json:"due_time_ms"`
	ProductionState      string      `json:"production_state"`
	LegalSignoffId       *server.Id  `json:"legal_signoff_id,omitempty"`
	EngineeringSignoffId *server.Id  `json:"engineering_signoff_id,omitempty"`
	CreateTimeMs         int64       `json:"create_time_ms"`
	CloseTimeMs          int64       `json:"close_time_ms"`
}

// What a legal kind did to a request.
type AbuseStaffLegalRequestResult struct {
	Request  *AbuseStaffLegalRequestView `json:"request"`
	Inserted bool                        `json:"inserted,omitempty"`
}

func abuseStaffLegalRequestView(request *model.LegalRequest) *AbuseStaffLegalRequestView {
	return &AbuseStaffLegalRequestView{
		RequestId:            request.RequestId,
		CaseId:               request.CaseId,
		Instrument:           string(request.Instrument),
		Country:              request.Country,
		NetworkIds:           slices.Clone(request.NetworkIds),
		Sealed:               request.Sealed,
		SealUntilMs:          abuseStaffTimeMs(request.SealUntil),
		DueTimeMs:            abuseStaffTimeMs(request.DueTime),
		ProductionState:      request.ProductionState,
		LegalSignoffId:       request.LegalSignoffId,
		EngineeringSignoffId: request.EngineeringSignoffId,
		CreateTimeMs:         request.CreateTime.UnixMilli(),
		CloseTimeMs:          abuseStaffTimeMs(request.CloseTime),
	}
}

// A time in Unix milliseconds, nil for 0 or less.
func abuseStaffOptionalTime(timeMs int64) *time.Time {
	if timeMs <= 0 {
		return nil
	}
	t := time.UnixMilli(timeMs).UTC()
	return &t
}

// The verbs this task builds.
func init() {
	legalRoles := []AbuseStaffRole{AbuseStaffRoleLegal}

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "file_legal_request",
		Roles: legalRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffFileLegalRequestArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			requestId := server.NewId()
			if args.RequestId != nil {
				requestId = *args.RequestId
			}
			request, inserted, err := model.FileLegalRequest(ctx, &model.LegalRequestArgs{
				RequestId:  requestId,
				CaseId:     args.CaseId,
				Instrument: model.LegalInstrument(strings.TrimSpace(args.Instrument)),
				Country:    strings.ToUpper(strings.TrimSpace(args.Country)),
				NetworkIds: args.NetworkIds,
				Sealed:     args.Sealed,
				SealUntil:  abuseStaffOptionalTime(args.SealUntilMs),
				Emergency:  args.Emergency,
				DueTime:    abuseStaffOptionalTime(args.DueTimeMs),
			}, abuseStaffDeps.now(), abuseLegalDeps.legalSettings())
			if err != nil {
				return nil, err
			}
			return &AbuseStaffLegalRequestResult{Request: abuseStaffLegalRequestView(request), Inserted: inserted}, nil
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "sign_production",
		Roles: []AbuseStaffRole{AbuseStaffRoleLegal, AbuseStaffRoleEngineering},
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffSignProductionArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			role := model.LegalSignoffRole(args.Role)
			// a member signs only in a role it holds
			switch role {
			case model.LegalSignoffRoleLegal:
				if !staff.HoldsAny([]AbuseStaffRole{AbuseStaffRoleLegal}) {
					return nil, ErrAbuseStaffRoleRefused
				}
			case model.LegalSignoffRoleEngineering:
				if !staff.HoldsAny([]AbuseStaffRole{AbuseStaffRoleEngineering}) {
					return nil, ErrAbuseStaffRoleRefused
				}
			default:
				return nil, fmt.Errorf("%w: a role that is neither legal nor engineering", ErrAbuseStaffRequestInvalid)
			}
			request, err := model.SignLegalProduction(ctx, args.RequestId, staff.ReviewerId, role)
			if err != nil {
				return nil, err
			}
			return &AbuseStaffLegalRequestResult{Request: abuseStaffLegalRequestView(request)}, nil
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "produce",
		Roles: legalRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffLegalRequestArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			reader, err := model.NewAbuseReader(staff.ReviewerId, abuseStaffDeps.settings())
			if err != nil {
				return nil, err
			}
			return reader.ProduceLegalRequest(ctx, args.RequestId, abuseStaffDeps.now(), abuseLegalDeps.productionSettings())
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "close_legal_request",
		Roles: legalRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffLegalRequestArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			request, err := model.CloseLegalRequest(ctx, args.RequestId, abuseStaffDeps.now())
			if err != nil {
				return nil, err
			}
			return &AbuseStaffLegalRequestResult{Request: abuseStaffLegalRequestView(request)}, nil
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "transparency_counts",
		Roles: legalRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffTransparencyCountsArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			if args.ToTimeMs <= args.FromTimeMs {
				return nil, fmt.Errorf("%w: a window that ends before it starts", ErrAbuseStaffRequestInvalid)
			}
			reader, err := model.NewAbuseReader(staff.ReviewerId, abuseStaffDeps.settings())
			if err != nil {
				return nil, err
			}
			return reader.TransparencyCounts(ctx, time.UnixMilli(args.FromTimeMs).UTC(), time.UnixMilli(args.ToTimeMs).UTC())
		},
	})
}
