package controller

// An operator's suspension of a network's compute through the abuse role's staff route (DESIGN-COMPUTE §17.2,
// D-CABUSE-1, D-CABUSE-2; CARCH-M12-01): two kinds of POST /abuse/action, which `sidecarctl compute suspend` and
// `sidecarctl compute resume` post, as `sidecarctl abuse` posts §21's (§21.10):
//
//	kind             roles     what it does
//	compute_suspend  reviewer  {network_id, reason}: holds the network's compute for one reason class, so every broker
//	                           admission refuses it `suspended`, and stops its enclaves (sidecar/controller's
//	                           ComputeAbuseHoldSuspend)
//	compute_resume   reviewer  {network_id}: ends the hold; nothing restarts on its own (ComputeAbuseHoldResume)
//
// Both keep the reviewer's opaque id on the row (compute_abuse_hold) and touch no ledger row: a held network keeps
// its balance and its records. Both are idempotent: a network held for that reason already, or not held at all, is
// answered as it stands, so a retry after a route that did not answer changes nothing twice. A binary that does not
// link MG-C1, where the table lives, refuses both, unavailable, until M12's Gn-out registers the group.
//
// Nothing here logs a network id, a reason or a reviewer (I-13); the answer goes only to the caller.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	operator "github.com/urnetwork/operator-core"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	sidecarmodel "github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// The kinds' names, as the route and sidecarctl spell them.
const (
	AbuseStaffKindComputeSuspend = "compute_suspend"
	AbuseStaffKindComputeResume  = "compute_resume"
)

// A binary that does not link MG-C1, which holds compute_abuse_hold: the route answers it 503.
var ErrAbuseComputeHoldUnlinked = errors.New("this binary does not link MG-C1, which holds the compute abuse hold")

// What the kinds run on. A test replaces each.
var abuseComputeHoldDeps = struct {
	// whether the binary links MG-C1
	linked func() bool
	// the process's Env, through which sidecar's controller reaches Postgres and Redis
	env func() operator.Env
	// the hold's settings, the projection's life shared with every broker
	settings func() *sidecarmodel.ComputeAbuseHoldSettings
	suspend  func(env operator.Env, ctx context.Context, settings *sidecarmodel.ComputeAbuseHoldSettings, request *sidecarcontroller.ComputeAbuseHoldRequest) (*sidecarcontroller.ComputeAbuseHoldAnswer, error)
	resume   func(env operator.Env, ctx context.Context, settings *sidecarmodel.ComputeAbuseHoldSettings, request *sidecarcontroller.ComputeAbuseHoldRequest) (*sidecarcontroller.ComputeAbuseHoldAnswer, error)
}{
	linked:   server.SidecarMgC1Linked,
	env:      operator.GlobalEnv,
	settings: sidecarmodel.DefaultComputeAbuseHoldSettings,
	suspend:  sidecarcontroller.ComputeAbuseHoldSuspend,
	resume:   sidecarcontroller.ComputeAbuseHoldResume,
}

// compute_suspend's body.
type abuseStaffComputeSuspendArgs struct {
	Kind      string `json:"kind"`
	NetworkId string `json:"network_id"`
	Reason    string `json:"reason"`
}

// compute_resume's body.
type abuseStaffComputeResumeArgs struct {
	Kind      string `json:"kind"`
	NetworkId string `json:"network_id"`
}

// What both kinds answer: the hold as it stands after the act.
type AbuseStaffComputeHoldView struct {
	NetworkId server.Id `json:"network_id"`
	// Whether the network's compute is held after the act.
	Held bool `json:"held"`
	// The class the hold stands for, or last stood for; "" for a network never held.
	Reason string `json:"reason"`
	// The row's version, 0 for a network never held.
	Version int64 `json:"version"`
	// When the hold was placed and when it was resumed, 0 for none.
	HoldTimeMs   int64 `json:"hold_time_ms"`
	ResumeTimeMs int64 `json:"resume_time_ms"`
	// Whether this act changed the row: false for a retry.
	Changed bool `json:"changed"`
	// Whether every broker reads the row's state at once; false where the projection failed after the commit, so
	// brokers read it once the stale projection expires, within the hold's settings' max age.
	Projected bool `json:"projected"`
	// What the enclaves' stop came to: none, stopped, failed or untouched.
	Enclaves string `json:"enclaves"`
}

// The kinds' registrations.
func init() {
	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  AbuseStaffKindComputeSuspend,
		Roles: []AbuseStaffRole{AbuseStaffRoleReviewer},
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffComputeSuspendArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			networkId, err := parseAbuseStaffComputeNetworkId(args.NetworkId)
			if err != nil {
				return nil, err
			}
			reason, err := sidecarmodel.ParseComputeAbuseHoldReason(strings.TrimSpace(args.Reason))
			if err != nil {
				return nil, fmt.Errorf("%w: the reason is none of %s", ErrAbuseStaffRequestInvalid, strings.Join(sidecarmodel.ComputeAbuseHoldReasonWords(), ", "))
			}
			return runAbuseStaffComputeHold(ctx, abuseComputeHoldDeps.suspend, &sidecarcontroller.ComputeAbuseHoldRequest{
				NetworkId: operator.Id(networkId),
				Reason:    reason,
				StaffId:   operator.Id(staff.ReviewerId),
			})
		},
	})
	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  AbuseStaffKindComputeResume,
		Roles: []AbuseStaffRole{AbuseStaffRoleReviewer},
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffComputeResumeArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			networkId, err := parseAbuseStaffComputeNetworkId(args.NetworkId)
			if err != nil {
				return nil, err
			}
			return runAbuseStaffComputeHold(ctx, abuseComputeHoldDeps.resume, &sidecarcontroller.ComputeAbuseHoldRequest{
				NetworkId: operator.Id(networkId),
				StaffId:   operator.Id(staff.ReviewerId),
			})
		},
	})
}

// A network id in its 8-4-4-4-12 form, never the zero id; the refusal never repeats the text.
func parseAbuseStaffComputeNetworkId(text string) (server.Id, error) {
	networkId, err := server.ParseId(strings.TrimSpace(text))
	if err != nil || networkId == (server.Id{}) {
		return server.Id{}, fmt.Errorf("%w: network_id is no network's id", ErrAbuseStaffRequestInvalid)
	}
	return networkId, nil
}

// Runs one act on the process's Env and answers its view.
func runAbuseStaffComputeHold(
	ctx context.Context,
	act func(env operator.Env, ctx context.Context, settings *sidecarmodel.ComputeAbuseHoldSettings, request *sidecarcontroller.ComputeAbuseHoldRequest) (*sidecarcontroller.ComputeAbuseHoldAnswer, error),
	request *sidecarcontroller.ComputeAbuseHoldRequest,
) (*AbuseStaffComputeHoldView, error) {
	if !abuseComputeHoldDeps.linked() {
		return nil, ErrAbuseComputeHoldUnlinked
	}
	answer, err := act(abuseComputeHoldDeps.env(), ctx, abuseComputeHoldDeps.settings(), request)
	if errors.Is(err, sidecarmodel.ErrComputeAbuseHoldArgs) {
		return nil, fmt.Errorf("%w: the network, the reviewer or the reason", ErrAbuseStaffRequestInvalid)
	}
	if err != nil {
		return nil, err
	}
	view := &AbuseStaffComputeHoldView{
		NetworkId: server.Id(request.NetworkId),
		Changed:   answer.Changed,
		Projected: answer.Projected,
		Enclaves:  string(answer.Enclaves),
	}
	if hold := answer.Hold; hold != nil {
		view.Held = hold.Held()
		view.Reason = hold.Reason.String()
		view.Version = hold.Version
		view.HoldTimeMs = hold.HoldTime.UnixMilli()
		if hold.ResumeTime != nil {
			view.ResumeTimeMs = hold.ResumeTime.UnixMilli()
		}
	}
	return view, nil
}
