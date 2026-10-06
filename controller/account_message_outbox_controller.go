// Delivery of the account message outbox (model/account_message_outbox_model.go).
//
// A state change that owes its account an email or SMS adds the message in its
// own transaction (`addAccountMessageInTx`, or the model for the messages it
// writes itself), instead of sending after the commit, where a crash between the
// commit and the send lost the message and a failed send was dropped. The
// delivery task (`DeliverAccountMessages`, every minute and right after a
// deploy) claims due messages one at a time, sends each through the message
// sender with its send timeout, and records the outcome: a failed send is due
// again after a backoff, and a message that fails `maxAttemptCount` times is
// abandoned. A claim leases the message to one attempt, so concurrent or
// repeated deliveries send a message once; only a crash between a send and its
// record sends it again, once the lease ends.
//
// Only the templates in `accountMessageTemplates` go through the outbox. Codes
// a person is waiting for (verification, password reset) are sent in their
// request, which reports a failed send.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// How often the delivery task runs when no run left due messages behind.
const accountMessageDeliverInterval = 1 * time.Minute

// The longest one delivery run claims messages before it leaves the rest to the
// next run.
const accountMessageDeliverRunBudget = 5 * time.Minute

// A claim's lease. It must outlast one send and its record, so it is longer than
// two of the longest sends `email.yml` allows (maxAccountMessageSendTimeout).
const accountMessageClaimLease = 10 * time.Minute

// The backoff after a failed send doubles from the base to the cap.
const accountMessageRetryBaseDelay = 1 * time.Minute
const accountMessageRetryMaxDelay = 1 * time.Hour

// A message is abandoned after this many failed attempts, about 15 hours after
// the first with the default backoff.
const accountMessageMaxAttemptCount = 20

// A run stops after this many failed sends in a row (an outage) and leaves the
// rest to the next run.
const accountMessageMaxConsecutiveFailureCount = 3

// Delivered and abandoned messages, with their recipients, are removed this long
// after they were added. A writer's key is remembered as long.
const accountMessageRetention = 7 * 24 * time.Hour

// The most finished messages one run removes.
const accountMessageRemoveLimit = 10000

// The minimum time between two default-level reports of each delivery problem.
const accountMessageDeliveryReportInterval = 1 * time.Minute

// The templates that go through the outbox, by name, each with a constructor the
// stored fields decode into.
var accountMessageTemplates = map[string]func() Template{
	(&SubscriptionEndedTemplate{}).Name(): func() Template {
		return &SubscriptionEndedTemplate{}
	},
	(&SubscriptionDataAppliedTemplate{}).Name(): func() Template {
		return &SubscriptionDataAppliedTemplate{}
	},
	(&X402ReceiptTemplate{}).Name(): func() Template {
		return &X402ReceiptTemplate{}
	},
	(&MissingWalletTemplate{}).Name(): func() Template {
		return &MissingWalletTemplate{}
	},
	// written by the model (model.AuthPasswordSetTemplateName)
	(&AuthPasswordSetTemplate{}).Name(): func() Template {
		return &AuthPasswordSetTemplate{}
	},
	// written by the model (model.NetworkWelcomeTemplateName)
	(&NetworkWelcomeTemplate{}).Name(): func() Template {
		return &NetworkWelcomeTemplate{}
	},
}

// Counts delivery outcomes by template and result (sent, retried, abandoned,
// lease_lost).
var accountMessageDeliveryCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "account_message",
		Name:      "outbox_deliveries_total",
		Help:      "Account message outbox delivery outcomes, by template and result",
	},
	[]string{"template", "result"},
)

// Registers the delivery metric with the default registry.
func init() {
	prometheus.MustRegister(accountMessageDeliveryCounter)
}

// The default-level reports of delivery problems: a failed send due again, a
// message given up, and a claim that outlived its lease (the message may go out
// twice).
var (
	accountMessageRetryReports     = newBoundedReport(accountMessageDeliveryReportInterval)
	accountMessageAbandonReports   = newBoundedReport(accountMessageDeliveryReportInterval)
	accountMessageLeaseLostReports = newBoundedReport(accountMessageDeliveryReportInterval)
)

// The template's metric label: its name when it goes through the outbox, so the
// label set stays bounded.
func accountMessageTemplateLabel(templateName string) string {
	if _, ok := accountMessageTemplates[templateName]; ok {
		return templateName
	}
	return "unregistered"
}

// A message for the outbox. The key identifies the state change that owes it.
type accountMessage struct {
	key       string
	networkId *server.Id
	userAuth  string
	template  Template
	// a held message waits for its writer to release it
	held bool
}

// The model's arguments for the message. The template must go through the
// outbox.
func (self *accountMessage) args() *model.AccountMessageArgs {
	templateName := self.template.Name()
	if _, ok := accountMessageTemplates[templateName]; !ok {
		panic(fmt.Errorf("account message template %s does not go through the outbox", templateName))
	}
	templateJson, err := json.Marshal(self.template)
	if err != nil {
		panic(err)
	}
	return &model.AccountMessageArgs{
		Key:          self.key,
		NetworkId:    self.networkId,
		UserAuth:     self.userAuth,
		TemplateName: templateName,
		TemplateJson: string(templateJson),
		Held:         self.held,
	}
}

// Adds the message in the caller's transaction (`model.AddAccountMessageInTx`).
// Returns false when a message with the key exists.
func addAccountMessageInTx(ctx context.Context, tx server.PgTx, message *accountMessage) bool {
	return model.AddAccountMessageInTx(ctx, tx, message.args())
}

// Adds the messages in the caller's transaction in one round trip.
func addAccountMessagesInTx(ctx context.Context, tx server.PgTx, messages []*accountMessage) {
	argsList := []*model.AccountMessageArgs{}
	for _, message := range messages {
		argsList = append(argsList, message.args())
	}
	model.AddAccountMessagesInTx(ctx, tx, argsList)
}

// The template a stored message renders, decoded from its fields.
func decodeAccountMessageTemplate(templateName string, templateJson string) (Template, error) {
	newTemplate, ok := accountMessageTemplates[templateName]
	if !ok {
		// a newer build may know it; keep it due for that build
		return nil, fmt.Errorf("account message template %s is not registered", templateName)
	}
	template := newTemplate()
	if err := json.Unmarshal([]byte(templateJson), template); err != nil {
		return nil, fmt.Errorf("account message template %s does not decode: %w", templateName, err)
	}
	return template, nil
}

// Settings of a delivery run.
type accountMessageDeliverySettings struct {
	runBudget                  time.Duration
	claimLease                 time.Duration
	retryBaseDelay             time.Duration
	retryMaxDelay              time.Duration
	maxAttemptCount            int
	maxConsecutiveFailureCount int
}

// The production settings.
func defaultAccountMessageDeliverySettings() *accountMessageDeliverySettings {
	return &accountMessageDeliverySettings{
		runBudget:                  accountMessageDeliverRunBudget,
		claimLease:                 accountMessageClaimLease,
		retryBaseDelay:             accountMessageRetryBaseDelay,
		retryMaxDelay:              accountMessageRetryMaxDelay,
		maxAttemptCount:            accountMessageMaxAttemptCount,
		maxConsecutiveFailureCount: accountMessageMaxConsecutiveFailureCount,
	}
}

// One delivery run: claims due messages one at a time, sends each and records
// the outcome, until none is due, the budget is spent, the context ends, or
// sends keep failing. Not safe for concurrent use; runs in other goroutines or
// processes coordinate through the claims.
type accountMessageDelivery struct {
	ctx      context.Context
	sender   MessageSender
	now      func() time.Time
	settings *accountMessageDeliverySettings

	sentCount               int
	retryCount              int
	abandonCount            int
	consecutiveFailureCount int
}

// A run that sends with `sender` and reads time from `now`.
func newAccountMessageDelivery(
	ctx context.Context,
	sender MessageSender,
	now func() time.Time,
	settings *accountMessageDeliverySettings,
) *accountMessageDelivery {
	return &accountMessageDelivery{
		ctx:      ctx,
		sender:   sender,
		now:      now,
		settings: settings,
	}
}

// Delivers due messages. Returns whether due messages may be left because the
// budget or the context ended the run; a run that stops after failed sends
// leaves the rest to the next interval.
func (self *accountMessageDelivery) Run() (more bool) {
	endTime := self.now().Add(self.settings.runBudget)
	for {
		select {
		case <-self.ctx.Done():
			return true
		default:
		}
		if self.settings.maxConsecutiveFailureCount <= self.consecutiveFailureCount {
			return false
		}
		now := self.now()
		if !now.Before(endTime) {
			return true
		}
		message := model.ClaimAccountMessage(self.ctx, now, self.settings.claimLease)
		if message == nil {
			return false
		}
		self.deliver(message)
	}
}

// The wait before a message's next attempt, after `attemptCount` failed ones.
func (self *accountMessageDelivery) retryDelay(attemptCount int) time.Duration {
	delay := self.settings.retryBaseDelay
	for i := 1; i < attemptCount && delay < self.settings.retryMaxDelay; i += 1 {
		delay *= 2
	}
	return min(delay, self.settings.retryMaxDelay)
}

// Sends one claimed message and records the outcome.
func (self *accountMessageDelivery) deliver(message *model.AccountMessage) {
	templateLabel := accountMessageTemplateLabel(message.TemplateName)
	template, err := decodeAccountMessageTemplate(message.TemplateName, message.TemplateJson)
	if err == nil {
		err = self.sender.SendAccountMessageTemplate(message.UserAuth, template)
		if err == nil {
			self.consecutiveFailureCount = 0
			if model.CompleteAccountMessage(self.ctx, message.MessageId, *message.ClaimId, self.now()) {
				self.sentCount += 1
				accountMessageDeliveryCounter.WithLabelValues(templateLabel, "sent").Inc()
			} else {
				self.reportLeaseLost(message)
			}
			return
		}
		// failed sends in a row end the run; a message that does not decode
		// says nothing about the sender
		self.consecutiveFailureCount += 1
	}

	failure := "failed"
	if errors.Is(err, context.DeadlineExceeded) {
		failure = "timeout"
	}
	now := self.now()
	if self.settings.maxAttemptCount <= message.AttemptCount {
		if !model.AbandonAccountMessage(self.ctx, message.MessageId, *message.ClaimId, now, err.Error()) {
			self.reportLeaseLost(message)
			return
		}
		self.abandonCount += 1
		accountMessageDeliveryCounter.WithLabelValues(templateLabel, "abandoned").Inc()
		if suppressedCount, ok := accountMessageAbandonReports.Allow(now); ok {
			glog.Infof(
				"[outbox]abandoned account message %s (%s) after %d attempts: the last %s; %d more abandoned since the last report\n",
				message.MessageId,
				message.TemplateName,
				message.AttemptCount,
				failure,
				suppressedCount,
			)
		}
		return
	}
	deliverTime := now.Add(self.retryDelay(message.AttemptCount))
	if !model.RetryAccountMessage(self.ctx, message.MessageId, *message.ClaimId, deliverTime, err.Error()) {
		self.reportLeaseLost(message)
		return
	}
	self.retryCount += 1
	accountMessageDeliveryCounter.WithLabelValues(templateLabel, "retried").Inc()
	if suppressedCount, ok := accountMessageRetryReports.Allow(now); ok {
		glog.Infof(
			"[outbox]account message %s (%s) attempt %d %s, due again at %s; %d more retried since the last report\n",
			message.MessageId,
			message.TemplateName,
			message.AttemptCount,
			failure,
			deliverTime.Format(time.RFC3339),
			suppressedCount,
		)
	}
}

// Counts and reports a claim whose lease ended before its outcome was recorded.
// Another attempt claimed the message, so it may be sent twice.
func (self *accountMessageDelivery) reportLeaseLost(message *model.AccountMessage) {
	accountMessageDeliveryCounter.WithLabelValues(accountMessageTemplateLabel(message.TemplateName), "lease_lost").Inc()
	if suppressedCount, ok := accountMessageLeaseLostReports.Allow(self.now()); ok {
		glog.Infof(
			"[outbox]account message %s (%s) attempt %d outlived its claim; %d more since the last report\n",
			message.MessageId,
			message.TemplateName,
			message.AttemptCount,
			suppressedCount,
		)
	}
}

// The delivery task takes no arguments.
type DeliverAccountMessagesArgs struct {
}

// What one delivery run did.
type DeliverAccountMessagesResult struct {
	SentCount    int `json:"sent_count"`
	RetryCount   int `json:"retry_count"`
	AbandonCount int `json:"abandon_count"`
	// due messages may be left; the next run starts at once
	More bool `json:"more"`
}

// Schedules a delivery run now. Run at startup, so a deploy delivers what the
// previous build left due.
func ScheduleDeliverAccountMessages(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleDeliverAccountMessagesAt(clientSession, tx, server.NowUtc())
}

// Schedules the next delivery run, one run at a time.
func scheduleDeliverAccountMessagesAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		DeliverAccountMessages,
		&DeliverAccountMessagesArgs{},
		clientSession,
		task.RunOnce("deliver_account_messages"),
		task.RunAt(runAt),
		task.MaxTime(2*accountMessageDeliverRunBudget),
	)
}

// Releases the missing-wallet notices a stopped payout run left held, delivers
// the due account messages, then removes finished ones past their retention.
func DeliverAccountMessages(
	_ *DeliverAccountMessagesArgs,
	clientSession *session.ClientSession,
) (*DeliverAccountMessagesResult, error) {
	// a payout run that stopped before its release leaves its notices held
	releaseMissingWalletNotices(
		clientSession.Ctx,
		server.NowUtc().Add(-missingWalletNoticeHoldTimeout),
		configuredMissingWalletNoticeMinPayout,
	)
	delivery := newAccountMessageDelivery(
		clientSession.Ctx,
		GetAWSMessageSender(),
		server.NowUtc,
		defaultAccountMessageDeliverySettings(),
	)
	more := delivery.Run()
	model.RemoveFinishedAccountMessages(
		clientSession.Ctx,
		server.NowUtc().Add(-accountMessageRetention),
		accountMessageRemoveLimit,
	)
	return &DeliverAccountMessagesResult{
		SentCount:    delivery.sentCount,
		RetryCount:   delivery.retryCount,
		AbandonCount: delivery.abandonCount,
		More:         more,
	}, nil
}

// Schedules the next run: at once when this one left due messages, else after
// the interval.
func DeliverAccountMessagesPost(
	_ *DeliverAccountMessagesArgs,
	result *DeliverAccountMessagesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	runAt := server.NowUtc().Add(accountMessageDeliverInterval)
	if result.More {
		runAt = server.NowUtc()
	}
	scheduleDeliverAccountMessagesAt(clientSession, tx, runAt)
	return nil
}
