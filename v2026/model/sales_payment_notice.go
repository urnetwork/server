package model

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// Payment notices for the sales Slack channel (owner request 2026-10-28):
// each payment for Pro or for data -- a new subscription, a renewal, a
// one-time purchase -- is posted once, with the network name, what was
// bought, the amount and how it was paid. The channel is the sales webhook
// (sales_webhook.go), which the Services leads post to as well.
//
// A notice is registered in the transaction that newly creates the payment's
// durable record (AddSalesPaymentNoticeInTx), on the branch where the record's
// idempotency gate inserted a row, and it is queued only once that
// transaction commits (server.AddTxPostCommit). A rollback, a retried attempt,
// a duplicate delivery, a client resubmission or a reconciler run over an
// existing record queues nothing. A crash between the commit and the post
// loses the notice: at most once.
//
// Posting never touches the purchase. The commit hook only queues, without
// blocking, on a bounded queue; one worker posts each notice under its own
// deadlines, with the webhook's bounded retries. A full queue drops the notice,
// and every outcome is counted in urnetwork_sales_payment_notify_total.

// SalesPaymentNotice is one payment to post. Item, Kind, Amount and Method are
// plain text; they are Slack-escaped when the message is built.
type SalesPaymentNotice struct {
	// RecordKey names the durable record, e.g. "stripe_invoice/in_123". It keys
	// the commit hook and the duplicate check, and is never posted.
	RecordKey string
	// NetworkId is the network the payment is for. nil when it has none: a data
	// code bought without a network, redeemed later.
	NetworkId *server.Id
	// Item is what was bought: "Pro monthly", "Pro yearly", "1 TiB data".
	Item string
	// Kind is "new", "renewal", "one-time", ...; "" when the store does not say.
	Kind string
	// Amount is the amount with its currency, as the payment record or the store
	// reports it. A configured list price is marked as one; nothing is estimated.
	Amount string
	// Method is how it was paid: "Stripe (card)", "Apple App Store", ...
	Method string
	// Enrich, when set, fills in what the record does not hold (the Stripe
	// payment method). It runs on the worker after the commit, never in the
	// purchase transaction, under salesPaymentNoticeEnrichTimeout. What it
	// cannot find stays as it was.
	Enrich func(ctx context.Context, notice *SalesPaymentNotice)
}

const salesPaymentNoticeKeyPrefix = "sales_payment_notice/"

const salesPaymentNoticeQueueSize = 1024

// a record key queued again within this window is a duplicate
const salesPaymentNoticeDuplicateWindow = 24 * time.Hour

const salesPaymentNoticeMaxQueuedKeys = 16 * 1024

const (
	salesPaymentNoticeEnrichTimeout = 20 * time.Second
	salesPaymentNoticeNameTimeout   = 10 * time.Second
	salesPaymentNoticeNotifyTimeout = time.Minute
)

// the longest field posted; a longer value is cut
const salesPaymentNoticeMaxFieldLength = 200

// Sales payment notice outcomes (the result label).
const (
	SalesPaymentNoticeResultOk           = "ok"
	SalesPaymentNoticeResultError        = "error"
	SalesPaymentNoticeResultUnconfigured = "unconfigured"
	SalesPaymentNoticeResultDropped      = "dropped"
	SalesPaymentNoticeResultDuplicate    = "duplicate"
	SalesPaymentNoticeResultUnhooked     = "unhooked"
	SalesPaymentNoticeResultBuildError   = "build_error"
)

var salesPaymentNoticeResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_sales_payment_notify_total",
	Help: "Sales channel payment notices (one per newly recorded payment), by result.",
}, []string{"result"})

func init() {
	prometheus.MustRegister(salesPaymentNoticeResults)
}

// CountSalesPaymentNotice counts a notice outcome decided outside this file:
// a notice the payment path could not build.
func CountSalesPaymentNotice(result string) {
	salesPaymentNoticeResults.WithLabelValues(result).Inc()
}

// salesPaymentNoticeTxPostCommit is server.AddTxPostCommit. Replaced only by
// tests, which have no server-owned transaction.
var salesPaymentNoticeTxPostCommit = server.AddTxPostCommit

// salesPaymentNoticeRegister queues the notice when tx commits and reports
// whether tx has a commit hook. Replaced only by tests
// (Testing_CaptureSalesPaymentNotices).
var salesPaymentNoticeRegister = registerSalesPaymentNotice

func registerSalesPaymentNotice(tx server.PgTx, notice *SalesPaymentNotice) bool {
	return salesPaymentNoticeTxPostCommit(tx, salesPaymentNoticeKeyPrefix+notice.RecordKey, func() any {
		salesPaymentNotices.enqueue(notice)
		return nil
	})
}

// AddSalesPaymentNoticeInTx queues the notice when tx commits. Call it on the
// branch where the payment's durable record was newly inserted. It never fails
// the caller: a transaction without a commit hook (a raw or nested pgx
// transaction) posts nothing, since posting before the commit could announce a
// payment that rolls back.
func AddSalesPaymentNoticeInTx(tx server.PgTx, notice *SalesPaymentNotice) {
	if notice == nil {
		return
	}
	if !salesPaymentNoticeRegister(tx, notice) {
		salesPaymentNoticeResults.WithLabelValues(SalesPaymentNoticeResultUnhooked).Inc()
		glog.Errorf("[sales]payment notice %s: the transaction has no commit hook; not posted\n", notice.RecordKey)
	}
}

// Testing_CaptureSalesPaymentNotices replaces the commit hook with one that
// records each registered notice, for tests without a database: a notice is
// captured where a server-owned transaction would queue it at commit. notices
// lists them in order; restore puts the hook back.
func Testing_CaptureSalesPaymentNotices() (notices func() []*SalesPaymentNotice, restore func()) {
	var stateLock sync.Mutex
	captured := []*SalesPaymentNotice{}
	previous := salesPaymentNoticeRegister
	salesPaymentNoticeRegister = func(tx server.PgTx, notice *SalesPaymentNotice) bool {
		stateLock.Lock()
		defer stateLock.Unlock()
		captured = append(captured, notice)
		return true
	}
	notices = func() []*SalesPaymentNotice {
		stateLock.Lock()
		defer stateLock.Unlock()
		return append([]*SalesPaymentNotice{}, captured...)
	}
	restore = func() {
		salesPaymentNoticeRegister = previous
	}
	return
}

// salesPaymentNoticeQueue is the bounded queue between the commit hooks and
// the one worker that posts.
type salesPaymentNoticeQueue struct {
	notices chan *SalesPaymentNotice

	stateLock  sync.Mutex
	started    bool
	queuedKeys map[string]time.Time
	// notices queued and not yet delivered; idle is closed while there are none
	pending int
	idle    chan struct{}
}

func newSalesPaymentNoticeQueue(size int) *salesPaymentNoticeQueue {
	idle := make(chan struct{})
	close(idle)
	return &salesPaymentNoticeQueue{
		notices:    make(chan *SalesPaymentNotice, size),
		queuedKeys: map[string]time.Time{},
		idle:       idle,
	}
}

var salesPaymentNotices = newSalesPaymentNoticeQueue(salesPaymentNoticeQueueSize)

// WaitSalesPaymentNotices waits up to timeout for the notices queued so far to
// be delivered, and reports whether they were. A short-lived process that
// credits payments (bringyourctl) calls it before it exits; a service never
// needs to.
func WaitSalesPaymentNotices(timeout time.Duration) bool {
	return salesPaymentNotices.wait(timeout)
}

func (self *salesPaymentNoticeQueue) addPending(delta int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.addPendingWithLock(delta)
}

func (self *salesPaymentNoticeQueue) addPendingWithLock(delta int) {
	if self.pending == 0 && 0 < delta {
		self.idle = make(chan struct{})
	}
	self.pending += delta
	if self.pending == 0 && delta < 0 {
		close(self.idle)
	}
}

func (self *salesPaymentNoticeQueue) wait(timeout time.Duration) bool {
	self.stateLock.Lock()
	idle := self.idle
	self.stateLock.Unlock()
	// idle first: with both ready, a select would pick either
	select {
	case <-idle:
		return true
	default:
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

// enqueue never blocks. It returns whether the notice was queued.
func (self *salesPaymentNoticeQueue) enqueue(notice *SalesPaymentNotice) bool {
	now := time.Now()
	self.stateLock.Lock()
	if queueTime, ok := self.queuedKeys[notice.RecordKey]; ok && now.Sub(queueTime) < salesPaymentNoticeDuplicateWindow {
		self.stateLock.Unlock()
		salesPaymentNoticeResults.WithLabelValues(SalesPaymentNoticeResultDuplicate).Inc()
		return false
	}
	if salesPaymentNoticeMaxQueuedKeys <= len(self.queuedKeys) {
		for key, queueTime := range self.queuedKeys {
			if salesPaymentNoticeDuplicateWindow <= now.Sub(queueTime) {
				delete(self.queuedKeys, key)
			}
		}
		if salesPaymentNoticeMaxQueuedKeys <= len(self.queuedKeys) {
			clear(self.queuedKeys)
		}
	}
	self.queuedKeys[notice.RecordKey] = now
	if !self.started {
		self.started = true
		go self.run()
	}
	// counted before the worker can take it
	self.addPendingWithLock(1)
	self.stateLock.Unlock()

	select {
	case self.notices <- notice:
		return true
	default:
		self.addPending(-1)
		salesPaymentNoticeResults.WithLabelValues(SalesPaymentNoticeResultDropped).Inc()
		glog.Errorf("[sales]payment notice %s dropped: the queue is full\n", notice.RecordKey)
		return false
	}
}

func (self *salesPaymentNoticeQueue) run() {
	for notice := range self.notices {
		result := SalesPaymentNoticeResultError
		server.HandleError(func() {
			result = deliverSalesPaymentNotice(notice)
		})
		salesPaymentNoticeResults.WithLabelValues(result).Inc()
		self.addPending(-1)
	}
}

// salesPaymentNotifierFactory builds the notifier the worker posts with. Tests
// replace it to reach a local webhook.
var salesPaymentNotifierFactory = newSalesWebhookNotifier

// salesPaymentNoticeNetworkName reads the network's name, "" when it cannot.
// Tests replace it.
var salesPaymentNoticeNetworkName = getSalesPaymentNoticeNetworkName

func getSalesPaymentNoticeNetworkName(ctx context.Context, networkId server.Id) (networkName string) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT network_name FROM network WHERE network_id = $1`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&networkName))
				}
			})
		})
	})
	return
}

// deliverSalesPaymentNotice posts one notice and returns its result, which
// the worker counts.
func deliverSalesPaymentNotice(notice *SalesPaymentNotice) string {
	webhookUrl := salesWebhookUrl(server.NowUtc())
	if webhookUrl == "" {
		return SalesPaymentNoticeResultUnconfigured
	}

	if notice.Enrich != nil {
		enrichCtx, enrichCancel := context.WithTimeout(context.Background(), salesPaymentNoticeEnrichTimeout)
		server.HandleError(func() {
			notice.Enrich(enrichCtx, notice)
		})
		enrichCancel()
	}

	networkName := ""
	if notice.NetworkId != nil {
		nameCtx, nameCancel := context.WithTimeout(context.Background(), salesPaymentNoticeNameTimeout)
		networkName = salesPaymentNoticeNetworkName(nameCtx, *notice.NetworkId)
		nameCancel()
	}

	notifyCtx, notifyCancel := context.WithTimeout(context.Background(), salesPaymentNoticeNotifyTimeout)
	defer notifyCancel()
	if err := salesPaymentNotifierFactory().Notify(notifyCtx, webhookUrl, SalesPaymentNoticeText(notice, networkName)); err != nil {
		// logged without the url
		glog.Infof("[sales]payment notice %s slack notify failed: %s\n", notice.RecordKey, err)
		return SalesPaymentNoticeResultError
	}
	return SalesPaymentNoticeResultOk
}

// SalesPaymentNoticeText is the message: a short block naming the network,
// what was bought, the amount and how it was paid. Every field is
// Slack-escaped and kept to one line, so a network name cannot mention a
// channel, disguise a link or add lines of its own.
func SalesPaymentNoticeText(notice *SalesPaymentNotice, networkName string) string {
	item := salesPaymentNoticeField(notice.Item, "payment")
	if kind := salesPaymentNoticeField(notice.Kind, ""); kind != "" {
		item = fmt.Sprintf("%s (%s)", item, kind)
	}
	var network string
	switch {
	case notice.NetworkId == nil:
		network = "none yet (a data code, redeemed later)"
	default:
		network = salesPaymentNoticeField(networkName, "(name unavailable)")
	}
	return fmt.Sprintf(
		`*Sale:* %s
*Network:* %s
*Amount:* %s
*Payment method:* %s`,
		item,
		network,
		salesPaymentNoticeField(notice.Amount, "not reported"),
		salesPaymentNoticeField(notice.Method, "not reported"),
	)
}

// salesPaymentNoticeField is value on one line, cut to
// salesPaymentNoticeMaxFieldLength and Slack-escaped; empty when it is blank.
func salesPaymentNoticeField(value string, blank string) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "?")
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return blank
	}
	if runes := []rune(value); salesPaymentNoticeMaxFieldLength < len(runes) {
		value = string(runes[:salesPaymentNoticeMaxFieldLength]) + "..."
	}
	return slackEscape(value)
}

// ----- the parts of a notice, shared by the payment paths -----

// Sales notice kinds.
const (
	SalesPaymentKindNew        = "new"
	SalesPaymentKindRenewal    = "renewal"
	SalesPaymentKindPlanChange = "plan change"
	SalesPaymentKindOneTime    = "one-time"
	SalesPaymentKindFreeTrial  = "new, free trial"
)

// SalesProItem names a Pro purchase by its plan (PlanMonthly, PlanYearly).
func SalesProItem(plan string) string {
	switch plan {
	case PlanMonthly:
		return "Pro monthly"
	case PlanYearly:
		return "Pro yearly"
	default:
		return "Pro"
	}
}

// SalesDataItem names a data purchase by its size, in binary units.
func SalesDataItem(byteCount ByteCount) string {
	units := []struct {
		size ByteCount
		name string
	}{
		{Tib, "TiB"},
		{Gib, "GiB"},
		{Mib, "MiB"},
		{Kib, "KiB"},
	}
	for _, unit := range units {
		if unit.size <= byteCount {
			value := new(big.Rat).SetFrac64(byteCount, unit.size)
			return fmt.Sprintf("%s %s data", salesTrimDecimal(value.FloatString(3), 0), unit.name)
		}
	}
	return fmt.Sprintf("%d B data", byteCount)
}

// SalesAmount is an exact amount of `units` in 10^-exponent of the currency,
// e.g. (499, 2, "usd") is "4.99 USD" and (4990, 3, "USD") is "4.99 USD".
// Trailing zeros are kept down to the currency's minor unit and no further, so
// the value is never rounded. An empty currency says it is not reported.
func SalesAmount(units *big.Int, exponent int, currency string) string {
	if units == nil {
		return ""
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	value := new(big.Rat).SetFrac(units, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(max(exponent, 0))), nil))
	digits := 2
	if currency != "" {
		digits = salesCurrencyMinorDigits(currency)
	}
	text := salesTrimDecimal(value.FloatString(max(exponent, 0)), digits)
	if currency == "" {
		return text + " (currency not reported)"
	}
	return text + " " + currency
}

// SalesAmount64 is SalesAmount of an int64.
func SalesAmount64(units int64, exponent int, currency string) string {
	return SalesAmount(big.NewInt(units), exponent, currency)
}

// salesTrimDecimal drops trailing fraction zeros beyond minDigits.
func salesTrimDecimal(text string, minDigits int) string {
	point := strings.IndexByte(text, '.')
	if point < 0 {
		if 0 < minDigits {
			return text + "." + strings.Repeat("0", minDigits)
		}
		return text
	}
	fraction := strings.TrimRight(text[point+1:], "0")
	if len(fraction) < minDigits {
		fraction += strings.Repeat("0", minDigits-len(fraction))
	}
	if fraction == "" {
		return text[:point]
	}
	return text[:point] + "." + fraction
}

// salesCurrencyMinorDigits is the ISO 4217 minor unit of a currency, 2 unless
// listed. It only decides how many zeros a displayed amount keeps.
func salesCurrencyMinorDigits(currency string) int {
	switch currency {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF",
		"UGX", "UYI", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return 3
	case "CLF", "UYW":
		return 4
	default:
		return 2
	}
}

// SalesCurrencyCode is a three-letter currency code, upper case, or "" when
// value is not one.
func SalesCurrencyCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 3 || strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return ""
	}
	return value
}
