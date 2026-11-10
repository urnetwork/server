package work

// §7.11's webhook delivery (TEAM-M6-09): a pass every PassInterval through
// model.DeliverNetworkAuditWebhooks, which posts each webhook the admin
// events after its cursor and retries a failing one for 24 hours. A pass that
// stops at its webhook bound hands its cursor to the next, which runs at once.
// A binary that does not link MG1 and MG6, which hold the events and the
// webhooks, schedules nothing.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The chain's tunables, beside delivery's (model.NetworkAuditWebhookSettings).
type NetworkAuditWebhookDeliveryWorkSettings struct {
	// How often a pass starts: 60 s, so an event reaches a healthy receiver
	// within about a minute.
	PassInterval time.Duration
	// Each run's task max time: a full pass of deliveries that each time out.
	MaxTime time.Duration
}

func DefaultNetworkAuditWebhookDeliveryWorkSettings() *NetworkAuditWebhookDeliveryWorkSettings {
	return &NetworkAuditWebhookDeliveryWorkSettings{
		PassInterval: 60 * time.Second,
		MaxTime:      time.Hour,
	}
}

type NetworkAuditWebhookDeliveryArgs struct {
	// Where the run starts; nil for the first webhook.
	AfterWebhookId *server.Id `json:"after_webhook_id,omitempty"`
}

type NetworkAuditWebhookDeliveryResult struct {
	model.NetworkAuditWebhookDeliveryResult
}

// Schedules the chain's first run a pass interval from now. A binary that
// does not link MG1 and MG6 schedules nothing.
func ScheduleNetworkAuditWebhookDelivery(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleNetworkAuditWebhookDelivery(clientSession, tx, &NetworkAuditWebhookDeliveryArgs{}, server.NowUtc().Add(DefaultNetworkAuditWebhookDeliveryWorkSettings().PassInterval))
}

func scheduleNetworkAuditWebhookDelivery(clientSession *session.ClientSession, tx server.PgTx, args *NetworkAuditWebhookDeliveryArgs, runAt time.Time) {
	if !server.SidecarMg1Linked() || !server.SidecarMg6Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		NetworkAuditWebhookDelivery,
		args,
		clientSession,
		task.RunOnce("network_audit_webhook_delivery"),
		task.RunAt(runAt),
		task.MaxTime(DefaultNetworkAuditWebhookDeliveryWorkSettings().MaxTime),
	)
}

// One delivery pass at real time, through the process's sender.
func NetworkAuditWebhookDelivery(
	networkAuditWebhookDelivery *NetworkAuditWebhookDeliveryArgs,
	clientSession *session.ClientSession,
) (*NetworkAuditWebhookDeliveryResult, error) {
	result := model.DeliverNetworkAuditWebhooks(clientSession.Ctx, server.NowUtc(), networkAuditWebhookDelivery.AfterWebhookId, model.DefaultNetworkAuditWebhookSettings())
	if 0 < result.FailedCount || 0 < result.SkippedCount {
		glog.Infof("[audit webhook]delivered %d, failed %d, skipped %d\n", result.DeliveredCount, result.FailedCount, result.SkippedCount)
	}
	return &NetworkAuditWebhookDeliveryResult{NetworkAuditWebhookDeliveryResult: *result}, nil
}

// Schedules the next run: at once from this run's cursor when it stopped at
// its bound, else a pass interval from now.
func NetworkAuditWebhookDeliveryPost(
	networkAuditWebhookDelivery *NetworkAuditWebhookDeliveryArgs,
	networkAuditWebhookDeliveryResult *NetworkAuditWebhookDeliveryResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if networkAuditWebhookDeliveryResult.AfterWebhookId != nil {
		scheduleNetworkAuditWebhookDelivery(clientSession, tx, &NetworkAuditWebhookDeliveryArgs{AfterWebhookId: networkAuditWebhookDeliveryResult.AfterWebhookId}, server.NowUtc())
		return nil
	}
	ScheduleNetworkAuditWebhookDelivery(clientSession, tx)
	return nil
}
