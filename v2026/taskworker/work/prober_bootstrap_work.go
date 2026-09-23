package work

import (
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// ProberBootstrapTimeout is the cadence of the credential refresh.
//
// Six hours is far shorter than the jwt refresh age it is keeping ahead of (see
// model.ProberJwtRefreshAge), so a missed pass or two costs nothing. The first
// task is immediate; only the post-step uses this recurring cadence.
const ProberBootstrapTimeout = 6 * time.Hour

type ProberBootstrapArgs struct{}

type ProberBootstrapResult struct{}

// ScheduleProberBootstrap arms the first pass immediately. Probe shard tasks
// start at the same deployment boundary and must not wait six hours for their
// only usable identity.
func ScheduleProberBootstrap(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleProberBootstrapAt(clientSession, tx, server.NowUtc())
}

func scheduleProberBootstrapAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		ProberBootstrap,
		&ProberBootstrapArgs{},
		clientSession,
		task.RunOnce("prober_bootstrap"),
		task.RunAt(runAt),
	)
}

// ProberBootstrap creates and refreshes the egress prober's credential -- the
// network account, its transfer balance, and its client jwt -- with no human
// step. Before this, an operator had to create an account by hand, authorise a
// balance code for it, mint a client jwt through /network/auth-client and paste
// the result into the prober's environment; a deployment where that had not
// been done had no egress probing at all, and nothing said so.
//
// Everything conditional lives in model.BootstrapProberIdentity. This function
// deliberately holds no logic of its own, which is what makes the constraint
// below true by construction rather than by review.
//
// The session here is UNAUTHENTICATED. InitTasks builds it as
// session.NewLocalClientSession(ctx, "0.0.0.0:0", nil), so ByJwt is nil and
// reading it would panic on the first run. The session is passed along (for its
// context and its address) and never read for identity; the model builds its
// own authenticated session from the stored prober identity when it needs one.
func ProberBootstrap(
	_ *ProberBootstrapArgs,
	clientSession *session.ClientSession,
) (*ProberBootstrapResult, error) {
	status, err := model.BootstrapProberIdentity(clientSession)
	if err != nil {
		return nil, err
	}

	// only say something when something happened; the steady state is silent
	if status.NetworkCreated || status.BalanceGranted || status.ClientJwtMinted {
		glog.Infof(
			"[proberboot]pass complete: network_created=%t balance_granted=%t jwt_minted=%t\n",
			status.NetworkCreated,
			status.BalanceGranted,
			status.ClientJwtMinted,
		)
	}

	return &ProberBootstrapResult{}, nil
}

// ProberBootstrapPost re-arms the chain.
//
// This is not boilerplate. These tasks are single-shot: the next run exists
// only because the previous run's Post scheduled it. Omitting this would leave
// the prober's credential to expire with nothing to renew it, and the failure
// would appear weeks later as a prober that cannot connect -- with no failing
// task anywhere to point at.
func ProberBootstrapPost(
	_ *ProberBootstrapArgs,
	_ *ProberBootstrapResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleProberBootstrapAt(clientSession, tx, server.NowUtc().Add(ProberBootstrapTimeout))
	return nil
}
