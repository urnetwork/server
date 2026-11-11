package controller

// POST /approval/cancel (§16.6's cancel row; D-MCP-11, AU3): an approver's
// ApprovalCancel, with the server-gated decision it cancels, makes each use no
// row holds cancelled, so no gate spends it afterwards; a use a gate committed
// first stays done (model.CancelApprovalUses). A client token only, classified
// sidecar on §7.4's allowlist (api/route_authz.go), its network the bearer's.
// Dark: answers unavailable until the binary links MG5 and the api process
// installs its gate states (model.SetApprovalGateStates).

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The cancel and the decision, each a Signed as received.
type ApprovalCancelArgs struct {
	// The approver's Signed ApprovalCancel (§6.9, purpose approval).
	Cancel []byte `json:"cancel"`
	// The Signed ApprovalDecision it cancels.
	Decision []byte `json:"decision"`
}

type ApprovalCancelResult struct {
	// The uses this cancel made cancelled; 0 when every use was spent or
	// cancelled already.
	CancelledUseCount int `json:"cancelled_use_count"`
}

func ApprovalCancel(args *ApprovalCancelArgs, clientSession *session.ClientSession) (*ApprovalCancelResult, error) {
	if clientSession.ByJwt.ClientId == nil {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, "A cancel is posted with a client token.")
	}
	if args == nil || len(args.Cancel) == 0 || len(args.Decision) == 0 {
		return nil, fmt.Errorf("%d %s", http.StatusBadRequest, "A cancel names the cancel and the decision.")
	}
	count, err := model.CancelApprovalUses(clientSession.Ctx, clientSession.ByJwt.NetworkId, args.Cancel, args.Decision,
		server.NowUtc(), model.DefaultApprovalUseSettings())
	if err != nil {
		approvalErr := &model.ApprovalUseError{}
		if errors.As(err, &approvalErr) {
			return nil, fmt.Errorf("%d %s", approvalErr.Status, approvalCancelMessage(approvalErr.Code))
		}
		return nil, err
	}
	return &ApprovalCancelResult{CancelledUseCount: count}, nil
}

// A refusal's constant text, which names no input.
func approvalCancelMessage(code string) string {
	switch code {
	case "unavailable":
		return "Server-gated approvals are not available yet."
	case "invalid":
		return "The cancel or the decision does not parse."
	case "refused":
		return "The cancel is not the decision's approver's."
	}
	return "The cancel was refused."
}
