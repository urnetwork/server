package controller

// ComputeServerNotifier (DESIGN-COMPUTE §9.8, §9.9, §9.12, DESIGN §7.4; BAL-M12-11): sidecar/controller's
// ComputeNotifier on the server.
//
//	NoticeInTx   owes a compute balance's notice to the network's billing contacts on the account message outbox, in
//	             the ledger change's own transaction (core.ServerTxOf), so the notice exists exactly when the change
//	             commits: a Team's Owners and Billing admins, a personal network's account (QD-1219's billing
//	             recipients, stripePaymentFailedNoticeEmailsInTx), each keyed by the notice's key and the address, so a
//	             change run again owes each once. It runs under a savepoint, so a notice that cannot be owed never
//	             fails the change it reports (a state change is what refuses spending): it counts as failed, and the
//	             change commits without it; a failure that reruns the whole transaction is the change's.
//	HintDevices  hints every active sidecar of the network on the compute topic (QD-1238), version the clock's
//	             milliseconds, which the hint script raises past the stored value; each device reads the balance route
//	             on it. Lossy: a device that misses one reads the state at its next poll.
//
// One template family, compute_notice, whose body follows the notice's kind: a top-up's failure and its retry, threshold
// turned off by the third, the bank's ask for authentication, a card detached, a monthly plan's unpaid invoice, a move
// into low, grace or suspended (from low on naming how many enclaves a suspension would stop and the indexer enclave's
// days left), and a deleted network's closure with what went back to the card and what was forfeited. It names amounts
// to the cent and counts, never a charge, a model or a person's content, and names the network as it stands at
// delivery (nameAccountMessageNetwork). Email only.
//
// InstallComputeNotices registers the process's notifier with sidecar/controller once, for the api's compute routes and
// taskworker's compute jobs.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/model"
)

// The compute notice family's outbox entry.
func init() {
	accountMessageTemplates[(&ComputeNoticeTemplate{}).Name()] = func() Template {
		return &ComputeNoticeTemplate{}
	}
	prometheus.MustRegister(computeNoticeCounter)
}

// Counts the compute notices owed, by kind and result (owed, no_email, failed).
var computeNoticeCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "compute",
		Name:      "notices_total",
		Help:      "Compute balance notices owed to billing contacts, by kind and result",
	},
	[]string{"kind", "result"},
)

// A notice owed in a transaction this package's core did not make: never in production, where every ledger change runs
// on core's Db.
var errComputeNoticeTx = errors.New("compute notice: the change's transaction is not a server transaction")

// Compute's notices and hints on the server (see the file header). Safe for concurrent use: it holds nothing.
type ComputeServerNotifier struct {
	// The page a notice links the person to, to add credit or save their card again: stripe.yml's compute.renew_url,
	// read at each notice; "" links none, and the notice names the app's compute screen in words.
	renewUrl func() string
}

var _ sidecarcontroller.ComputeNotifier = (*ComputeServerNotifier)(nil)

// The notifier over the config's renew url, nil taking stripe.yml's.
func NewComputeServerNotifier(renewUrl func() string) *ComputeServerNotifier {
	if renewUrl == nil {
		renewUrl = computeNoticeRenewUrlFromResources
	}
	return &ComputeServerNotifier{renewUrl: renewUrl}
}

// stripe.yml's compute.renew_url, "" where it names none or does not read.
func computeNoticeRenewUrlFromResources() string {
	var c struct {
		Compute *struct {
			RenewUrl string `yaml:"renew_url"`
		} `yaml:"compute"`
	}
	resource, err := server.Config.SimpleResource("stripe.yml")
	if err != nil || resource.UnmarshalYamlE(&c) != nil || c.Compute == nil {
		return ""
	}
	return strings.TrimSpace(c.Compute.RenewUrl)
}

// The template a notice renders.
func computeNoticeTemplateOf(notice *sidecarcontroller.ComputeNotice, renewUrl string) *ComputeNoticeTemplate {
	template := &ComputeNoticeTemplate{
		Kind:               string(notice.Kind),
		AmountNanoUsd:      notice.AmountNanoUsd,
		FailureCount:       notice.FailureCount,
		BalanceNanoUsd:     notice.BalanceNanoUsd,
		AtRiskEnclaveCount: len(notice.AtRiskEnclaveIds),
		IndexerDaysLeft:    notice.IndexerDaysLeft,
		RefundedNanoUsd:    notice.RefundedNanoUsd,
		ForfeitedNanoUsd:   notice.ForfeitedNanoUsd,
		RenewUrl:           renewUrl,
	}
	if notice.RetryTime != nil {
		retry := notice.RetryTime.UTC()
		template.RetryTime = &retry
	}
	return template
}

// See sidecarcontroller.ComputeNotifier (and the file header).
func (self *ComputeServerNotifier) NoticeInTx(env operator.Env, ctx context.Context, tx operator.DbTx, notice *sidecarcontroller.ComputeNotice) error {
	pgTx, ok := core.ServerTxOf(tx)
	if !ok {
		return errComputeNoticeTx
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT compute_notice`); err != nil {
		return err
	}
	result, failure := func() (result string, failure any) {
		defer func() {
			if recovered := recover(); recovered != nil {
				failure = recovered
			}
		}()
		return self.oweInTx(ctx, pgTx, notice), nil
	}()
	if failure == nil {
		if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT compute_notice`); err != nil {
			return err
		}
		computeNoticeCounter.WithLabelValues(string(notice.Kind), result).Inc()
		return nil
	}
	if err, rerun := computeNoticeFailureReruns(ctx, failure); rerun {
		return err
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT compute_notice`); err != nil {
		return err
	}
	computeNoticeCounter.WithLabelValues(string(notice.Kind), "failed").Inc()
	glog.Infof("[compute]a %s notice could not be owed (%T), and its change commits without it\n", notice.Kind, failure)
	return nil
}

// Owes notice to each billing contact in tx: the result to count.
func (self *ComputeServerNotifier) oweInTx(ctx context.Context, tx server.PgTx, notice *sidecarcontroller.ComputeNotice) string {
	emails, _ := stripePaymentFailedNoticeEmailsInTx(ctx, tx, notice.NetworkId)
	if len(emails) == 0 {
		return "no_email"
	}
	template := computeNoticeTemplateOf(notice, self.renewUrl())
	networkId := notice.NetworkId
	messages := []*accountMessage{}
	for _, email := range emails {
		messages = append(messages, &accountMessage{
			key:       "compute/" + notice.Key + "/" + email,
			networkId: &networkId,
			userAuth:  email,
			template:  template,
		})
	}
	addAccountMessagesInTx(ctx, tx, messages)
	return "owed"
}

// Whether a failure while owing a notice ends the whole transaction anyway, the change's own handling to take: the
// caller's stop, or a serialization failure or deadlock, which reruns it.
func computeNoticeFailureReruns(ctx context.Context, failure any) (error, bool) {
	if ctx.Err() != nil {
		return ctx.Err(), true
	}
	err, ok := failure.(error)
	if !ok {
		return nil, false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err, true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "40") {
		return err, true
	}
	return nil, false
}

// See sidecarcontroller.ComputeNotifier (and the file header).
func (self *ComputeServerNotifier) HintDevices(env operator.Env, ctx context.Context, networkId operator.Id) error {
	clientIds := computeNoticeNetworkClientIds(ctx, networkId)
	if len(clientIds) == 0 {
		return nil
	}
	writer, err := computeHintWriter()
	if err != nil {
		return err
	}
	connectClientIds := make([]connect.Id, len(clientIds))
	for index, clientId := range clientIds {
		connectClientIds[index] = connect.Id(clientId)
	}
	return writer.Hint(ctx, connectClientIds, &sidecar.ClientHint{
		Topic:   sidecar.PushTopicCompute,
		Version: uint64(server.NowUtc().UnixMilli()),
	})
}

// The compute topic's hint writer, made at first use: a clock topic, which wakes no device (§18.7).
var computeHintWriter = sync.OnceValues(func() (*pushservice.HintWriter, error) {
	return pushservice.NewHintWriter(model.NewSidecarPushRedis(nil), pushservice.DefaultHintWriterSettings(), nil, nil)
})

// Every current client of the network's active sidecars: each identity's newest binding (§5.15). None in a binary that
// does not link MG1, or where the read fails, since a hint is lossy.
func computeNoticeNetworkClientIds(ctx context.Context, networkId server.Id) []server.Id {
	clientIds := []server.Id{}
	if !server.SidecarMg1Linked() {
		return clientIds
	}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT DISTINCT ON (sidecar_identity_binding.sidecar_id) sidecar_identity_binding.client_id
					FROM sidecar_identity
					INNER JOIN sidecar_identity_binding ON sidecar_identity_binding.sidecar_id = sidecar_identity.sidecar_id
					WHERE sidecar_identity.network_id = $1 AND sidecar_identity.status = $2
					ORDER BY sidecar_identity_binding.sidecar_id, sidecar_identity_binding.generation DESC
				`,
				networkId,
				model.SidecarIdentityStatusActive,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					server.Raise(result.Scan(&clientId))
					clientIds = append(clientIds, clientId)
				}
			})
		})
	})
	return clientIds
}

var installComputeNoticesOnce sync.Once

// Registers the process's notifier with sidecar/controller, once a process: a second call does nothing.
func InstallComputeNotices() {
	installComputeNoticesOnce.Do(func() {
		sidecarcontroller.RegisterComputeNotifier(NewComputeServerNotifier(nil))
	})
}

// The account message a compute balance's notice owes each billing contact (see the file header). Email only.
type ComputeNoticeTemplate struct {
	// sidecar/controller's ComputeNoticeKind.
	Kind string
	// A top-up's or a monthly plan's amount, the failures in a row, and when it retries.
	AmountNanoUsd int64
	FailureCount  int
	RetryTime     *time.Time
	// A state's: the balance it read, holds left out, how many enclaves a suspension would stop, and the network's
	// indexer enclave's days left, nil where it has none.
	BalanceNanoUsd     int64
	AtRiskEnclaveCount int
	IndexerDaysLeft    *int64
	// A closure's: what went back to the card, and what was forfeited.
	RefundedNanoUsd  int64
	ForfeitedNanoUsd int64
	// Where the person adds credit or saves a card again; "" for none.
	RenewUrl string
	BaseTemplate
	securityNoticeNetwork
}

// The template's files under email_templates.
func (self *ComputeNoticeTemplate) Name() string {
	return "compute_notice"
}

// nano-USD as dollars to the cent, "< $0.01" below one and a sign below zero (§9.13's figures).
func computeNoticeUsd(nanoUsd int64) string {
	sign := ""
	if nanoUsd < 0 {
		sign = "-"
		nanoUsd = -nanoUsd
	}
	cents := nanoUsd / 10_000_000
	if cents == 0 && 0 < nanoUsd {
		return sign + "< $0.01"
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}

// The amount, the balance, the refund and the forfeit in words.
func (self *ComputeNoticeTemplate) AmountText() string {
	return computeNoticeUsd(self.AmountNanoUsd)
}

func (self *ComputeNoticeTemplate) BalanceText() string {
	return computeNoticeUsd(self.BalanceNanoUsd)
}

func (self *ComputeNoticeTemplate) RefundedText() string {
	return computeNoticeUsd(self.RefundedNanoUsd)
}

func (self *ComputeNoticeTemplate) ForfeitedText() string {
	return computeNoticeUsd(self.ForfeitedNanoUsd)
}

// When a failed top-up retries, in UTC; "" for none.
func (self *ComputeNoticeTemplate) RetryText() string {
	if self.RetryTime == nil {
		return ""
	}
	return self.RetryTime.UTC().Format("January 2, 2006, 15:04 UTC")
}

// The indexer enclave's days left in words; "" where the network has none.
func (self *ComputeNoticeTemplate) IndexerText() string {
	if self.IndexerDaysLeft == nil {
		return ""
	}
	if *self.IndexerDaysLeft == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", *self.IndexerDaysLeft)
}

// Whether the kind is a state's, low, grace or suspended, which names the enclaves at risk.
func (self *ComputeNoticeTemplate) StateNotice() bool {
	switch sidecarcontroller.ComputeNoticeKind(self.Kind) {
	case sidecarcontroller.ComputeNoticeLow, sidecarcontroller.ComputeNoticeGrace, sidecarcontroller.ComputeNoticeSuspended:
		return true
	}
	return false
}

// Each kind, for the templates' conditions.
func (self *ComputeNoticeTemplate) IsTopUpFailed() bool {
	return self.Kind == string(sidecarcontroller.ComputeNoticeTopUpFailed)
}

func (self *ComputeNoticeTemplate) IsTopUpOff() bool {
	return self.Kind == string(sidecarcontroller.ComputeNoticeTopUpOff)
}

func (self *ComputeNoticeTemplate) IsAuthenticationRequired() bool {
	return self.Kind == string(sidecarcontroller.ComputeNoticeAuthenticationRequired)
}

func (self *ComputeNoticeTemplate) IsCardDetached() bool {
	return self.Kind == string(sidecarcontroller.ComputeNoticeCardDetached)
}

func (self *ComputeNoticeTemplate) IsMonthlyFailed() bool {
	return self.Kind == string(sidecarcontroller.ComputeNoticeMonthlyFailed)
}

func (self *ComputeNoticeTemplate) IsLow() bool {
	return self.Kind == string(sidecarcontroller.ComputeNoticeLow)
}

func (self *ComputeNoticeTemplate) IsGrace() bool {
	return self.Kind == string(sidecarcontroller.ComputeNoticeGrace)
}

func (self *ComputeNoticeTemplate) IsSuspended() bool {
	return self.Kind == string(sidecarcontroller.ComputeNoticeSuspended)
}

func (self *ComputeNoticeTemplate) IsClosed() bool {
	return self.Kind == string(sidecarcontroller.ComputeNoticeClosed)
}

// The subject: what happened, and in which network.
func (self *ComputeNoticeTemplate) Subject() string {
	network := self.Network()
	switch sidecarcontroller.ComputeNoticeKind(self.Kind) {
	case sidecarcontroller.ComputeNoticeTopUpFailed:
		return "A compute credit top-up for " + network + " failed"
	case sidecarcontroller.ComputeNoticeTopUpOff:
		return "Compute credit top-ups for " + network + " are off"
	case sidecarcontroller.ComputeNoticeAuthenticationRequired:
		return "Your bank asks you to confirm a compute credit top-up for " + network
	case sidecarcontroller.ComputeNoticeCardDetached:
		return "Compute credit top-ups for " + network + " stopped"
	case sidecarcontroller.ComputeNoticeMonthlyFailed:
		return "The monthly compute credit plan for " + network + " was not paid"
	case sidecarcontroller.ComputeNoticeLow:
		return "Compute credit is running low on " + network
	case sidecarcontroller.ComputeNoticeGrace:
		return "Compute credit ran out on " + network
	case sidecarcontroller.ComputeNoticeSuspended:
		return "Compute is suspended on " + network
	case sidecarcontroller.ComputeNoticeClosed:
		return "The compute balance of " + network + " is closed"
	}
	return "About the compute balance of " + network
}

// The headline: what happened, in a few words, naming no network (the layout's two to four words, EMAIL1.md).
func (self *ComputeNoticeTemplate) Headline() string {
	switch sidecarcontroller.ComputeNoticeKind(self.Kind) {
	case sidecarcontroller.ComputeNoticeTopUpFailed:
		return "Top-up failed"
	case sidecarcontroller.ComputeNoticeTopUpOff:
		return "Top-ups turned off"
	case sidecarcontroller.ComputeNoticeAuthenticationRequired:
		return "Confirm your top-up"
	case sidecarcontroller.ComputeNoticeCardDetached:
		return "Top-ups stopped"
	case sidecarcontroller.ComputeNoticeMonthlyFailed:
		return "Monthly plan unpaid"
	case sidecarcontroller.ComputeNoticeLow:
		return "Compute credit low"
	case sidecarcontroller.ComputeNoticeGrace:
		return "Compute credit ran out"
	case sidecarcontroller.ComputeNoticeSuspended:
		return "Compute suspended"
	case sidecarcontroller.ComputeNoticeClosed:
		return "Compute balance closed"
	}
	return "Compute balance"
}

// The headline in the text part's upper case.
func (self *ComputeNoticeTemplate) TextHeadline() string {
	return strings.ToUpper(self.Headline())
}
