package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Pure tests: no database, redis or Slack (owner request 2026-10-28). The
// sales webhook is a local TLS server, configured through a vault override;
// the real vault is never read.

// salesTestSlack is a local webhook that records each post. While block is
// set, it answers nothing until the test ends.
type salesTestSlack struct {
	server *httptest.Server
	status atomic.Int32
	block  atomic.Bool

	stateLock   sync.Mutex
	bodies      []string
	posted      chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

// releaseHangs answers every post held while block was set.
func (self *salesTestSlack) releaseHangs() {
	self.releaseOnce.Do(func() {
		close(self.release)
	})
}

func newSalesTestSlack(t *testing.T) *salesTestSlack {
	slack := &salesTestSlack{
		posted:  make(chan struct{}, 64),
		release: make(chan struct{}),
	}
	slack.status.Store(http.StatusOK)
	slack.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		func() {
			slack.stateLock.Lock()
			defer slack.stateLock.Unlock()
			slack.bodies = append(slack.bodies, string(body))
		}()
		slack.posted <- struct{}{}
		if slack.block.Load() {
			select {
			case <-slack.release:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(int(slack.status.Load()))
	}))
	t.Cleanup(func() {
		slack.releaseHangs()
		slack.server.Close()
	})

	// the webhook, through the vault override
	salesWebhookConfigCache.Store(nil)
	pop := server.Vault.PushSimpleResource(salesWebhookVaultResource, []byte("slack_webhook_url: "+slack.server.URL+"/services/T0/B0/salestest\n"))
	t.Cleanup(func() {
		pop()
		salesWebhookConfigCache.Store(nil)
	})

	previousFactory := salesPaymentNotifierFactory
	salesPaymentNotifierFactory = func() *salesWebhookNotifier {
		client := slack.server.Client()
		client.Timeout = 2 * time.Second
		return &salesWebhookNotifier{
			httpClient:  client,
			retryDelays: []time.Duration{0},
			sleep:       func(context.Context, time.Duration) bool { return true },
		}
	}
	t.Cleanup(func() { salesPaymentNotifierFactory = previousFactory })
	return slack
}

func (self *salesTestSlack) posts() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.bodies...)
}

// salesTestNetworkName names every network name.
func salesTestNetworkName(t *testing.T, name string) {
	previous := salesPaymentNoticeNetworkName
	salesPaymentNoticeNetworkName = func(context.Context, server.Id) string { return name }
	t.Cleanup(func() { salesPaymentNoticeNetworkName = previous })
}

// salesTestQueue replaces the notice queue with one whose worker never runs:
// the test takes the notices from it.
func salesTestQueue(t *testing.T, size int) *salesPaymentNoticeQueue {
	queue := newSalesPaymentNoticeQueue(size)
	queue.started = true
	previous := salesPaymentNotices
	salesPaymentNotices = queue
	t.Cleanup(func() { salesPaymentNotices = previous })
	return queue
}

func salesTestNotice(recordKey string) *SalesPaymentNotice {
	networkId := server.NewId()
	return &SalesPaymentNotice{
		RecordKey: recordKey,
		NetworkId: &networkId,
		Item:      "Pro monthly",
		Kind:      SalesPaymentKindRenewal,
		Amount:    "4.99 USD",
		Method:    "Stripe (card)",
	}
}

// A network named to mention the channel and disguise a link renders inert,
// and a name cannot add lines of its own.
func TestSalesPaymentNoticeTextEscapesTheNetworkName(t *testing.T) {
	notice := salesTestNotice("stripe_invoice/in_1")
	text := SalesPaymentNoticeText(notice, "<!channel> & <https://x|y>")
	connect.AssertEqual(t, text, "*Sale:* Pro monthly (renewal)\n*Network:* &lt;!channel&gt; &amp; &lt;https://x|y&gt;\n*Amount:* 4.99 USD\n*Payment method:* Stripe (card)")
	for _, forbidden := range []string{"<!channel>", "<https://", "|y>"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("unescaped %q in %q", forbidden, text)
		}
	}

	text = SalesPaymentNoticeText(notice, "evil\n*Amount:* 0.00 USD\u2028<!here>")
	connect.AssertEqual(t, len(strings.Split(text, "\n")), 4)
	connect.AssertEqual(t, strings.Contains(text, "*Network:* evil *Amount:* 0.00 USD &lt;!here&gt;\n"), true)

	// what the stores report is escaped too
	notice.Method = "Stripe (<!everyone>)"
	connect.AssertEqual(t, strings.Contains(SalesPaymentNoticeText(notice, "acme"), "Stripe (&lt;!everyone&gt;)"), true)
}

func TestSalesPaymentNoticeTextWithoutNetworkOrName(t *testing.T) {
	notice := salesTestNotice("balance_code/cs_1")
	notice.NetworkId = nil
	notice.Kind = SalesPaymentKindOneTime
	notice.Item = "1 TiB data"
	connect.AssertEqual(t, SalesPaymentNoticeText(notice, ""), "*Sale:* 1 TiB data (one-time)\n*Network:* none yet (a data code, redeemed later)\n*Amount:* 4.99 USD\n*Payment method:* Stripe (card)")

	// a network whose name cannot be read, and a store that reports no amount
	notice = salesTestNotice("apple_transaction/1")
	notice.Kind = ""
	notice.Amount = ""
	connect.AssertEqual(t, SalesPaymentNoticeText(notice, ""), "*Sale:* Pro monthly\n*Network:* (name unavailable)\n*Amount:* not reported\n*Payment method:* Stripe (card)")

	// a very long name is cut
	text := SalesPaymentNoticeText(salesTestNotice("x"), strings.Repeat("n", 500))
	connect.AssertEqual(t, strings.Contains(text, strings.Repeat("n", salesPaymentNoticeMaxFieldLength)+"...\n"), true)
}

// Amounts are exact: never rounded, zeros kept down to the currency's minor
// unit.
func TestSalesAmountIsExact(t *testing.T) {
	cases := []struct {
		units    int64
		exponent int
		currency string
		want     string
	}{
		{499, 2, "usd", "4.99 USD"},
		{500, 2, "USD", "5.00 USD"},
		{4990, 3, "USD", "4.99 USD"},
		{4995, 3, "USD", "4.995 USD"},
		{600_000, 3, "JPY", "600 JPY"},
		{980, 0, "jpy", "980 JPY"},
		{1250, 3, "KWD", "1.250 KWD"},
		{4_990_000_000, 9, "EUR", "4.99 EUR"},
		{4_990_000, 6, "USDC", "4.99 USDC"},
		{4_000_000_001, 9, "USD", "4.000000001 USD"},
		{0, 2, "USD", "0.00 USD"},
		{4990, 3, "", "4.99 (currency not reported)"},
	}
	for _, c := range cases {
		connect.AssertEqual(t, SalesAmount64(c.units, c.exponent, c.currency), c.want)
	}
	connect.AssertEqual(t, SalesAmount(nil, 2, "USD"), "")
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	connect.AssertEqual(t, SalesAmount(huge, 18, "ETH"), "123456789012.34567890123456789 ETH")
}

func TestSalesDataItem(t *testing.T) {
	cases := map[ByteCount]string{
		Tib:            "1 TiB data",
		10 * Tib:       "10 TiB data",
		3 * Tib / 2:    "1.5 TiB data",
		100 * Gib:      "100 GiB data",
		600 * Gib:      "600 GiB data",
		512 * Mib:      "512 MiB data",
		ByteCount(999): "999 B data",
	}
	for byteCount, want := range cases {
		connect.AssertEqual(t, SalesDataItem(byteCount), want)
	}
	connect.AssertEqual(t, SalesProItem(PlanMonthly), "Pro monthly")
	connect.AssertEqual(t, SalesProItem(PlanYearly), "Pro yearly")
	connect.AssertEqual(t, SalesProItem(""), "Pro")
	connect.AssertEqual(t, SalesCurrencyCode(" usd "), "USD")
	connect.AssertEqual(t, SalesCurrencyCode("US$"), "")
	connect.AssertEqual(t, SalesCurrencyCode("USDC"), "")
}

// A notice is queued only when its transaction commits: a rolled-back or
// retried attempt queues nothing, and the commit queues it without posting.
func TestSalesPaymentNoticeQueuedOnlyWhenTheTransactionCommits(t *testing.T) {
	queue := salesTestQueue(t, 8)
	type registration struct {
		key  string
		post server.PostFunction
	}
	registrations := []registration{}
	previous := salesPaymentNoticeTxPostCommit
	salesPaymentNoticeTxPostCommit = func(tx server.PgTx, key string, post server.PostFunction) bool {
		registrations = append(registrations, registration{key: key, post: post})
		return true
	}
	t.Cleanup(func() { salesPaymentNoticeTxPostCommit = previous })

	// the attempt that rolls back
	AddSalesPaymentNoticeInTx(nil, salesTestNotice("stripe_invoice/in_1"))
	// the attempt that commits
	notice := salesTestNotice("stripe_invoice/in_1")
	AddSalesPaymentNoticeInTx(nil, notice)
	connect.AssertEqual(t, len(registrations), 2)
	connect.AssertEqual(t, registrations[1].key, "sales_payment_notice/stripe_invoice/in_1")
	connect.AssertEqual(t, len(queue.notices), 0)

	// the commit: only the committed attempt's post runs
	registrations[1].post()
	connect.AssertEqual(t, len(queue.notices), 1)
	connect.AssertEqual(t, <-queue.notices, notice)
}

// A raw or nested pgx transaction has no commit to wait for: nothing is
// queued, so a payment that rolls back is never announced.
func TestAddSalesPaymentNoticeInTxWithoutACommitHookPostsNothing(t *testing.T) {
	queue := salesTestQueue(t, 8)
	AddSalesPaymentNoticeInTx(nil, salesTestNotice("stripe_invoice/in_raw"))
	connect.AssertEqual(t, len(queue.notices), 0)
}

// The same record queued twice (a duplicate delivery that reached the commit
// hook again) posts once.
func TestSalesPaymentNoticeDuplicateRecordPostsOnce(t *testing.T) {
	slack := newSalesTestSlack(t)
	salesTestNetworkName(t, "acme")
	queue := salesTestQueue(t, 8)

	connect.AssertEqual(t, queue.enqueue(salesTestNotice("apple_transaction/1000")), true)
	connect.AssertEqual(t, queue.enqueue(salesTestNotice("apple_transaction/1000")), false)
	connect.AssertEqual(t, queue.enqueue(salesTestNotice("apple_transaction/1001")), true)
	for 0 < len(queue.notices) {
		connect.AssertEqual(t, deliverSalesPaymentNotice(<-queue.notices), SalesPaymentNoticeResultOk)
	}
	connect.AssertEqual(t, len(slack.posts()), 2)
}

// The post: one message with the network's name, Slack-escaped, and no link
// or media unfurling.
func TestDeliverSalesPaymentNoticePostsTheMessage(t *testing.T) {
	slack := newSalesTestSlack(t)
	salesTestNetworkName(t, "<!channel> & <https://x|y>")

	enriched := false
	notice := salesTestNotice("stripe_invoice/in_2")
	notice.Method = "Stripe"
	notice.Enrich = func(ctx context.Context, notice *SalesPaymentNotice) {
		enriched = true
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("enrichment runs without a deadline")
		}
		notice.Method = "Stripe (Link)"
	}
	connect.AssertEqual(t, deliverSalesPaymentNotice(notice), SalesPaymentNoticeResultOk)
	connect.AssertEqual(t, enriched, true)

	posts := slack.posts()
	connect.AssertEqual(t, len(posts), 1)
	var message struct {
		Text        string `json:"text"`
		UnfurlLinks *bool  `json:"unfurl_links"`
		UnfurlMedia *bool  `json:"unfurl_media"`
	}
	connect.AssertEqual(t, json.Unmarshal([]byte(posts[0]), &message), nil)
	connect.AssertEqual(t, message.Text, "*Sale:* Pro monthly (renewal)\n*Network:* &lt;!channel&gt; &amp; &lt;https://x|y&gt;\n*Amount:* 4.99 USD\n*Payment method:* Stripe (Link)")
	connect.AssertEqual(t, *message.UnfurlLinks, false)
	connect.AssertEqual(t, *message.UnfurlMedia, false)
}

// Without a webhook nothing is posted, and nothing is read for the notice.
func TestDeliverSalesPaymentNoticeUnconfiguredPostsNothing(t *testing.T) {
	salesWebhookConfigCache.Store(nil)
	pop := server.Vault.PushSimpleResource(salesWebhookVaultResource, []byte("slack_webhook_url: \"\"\n"))
	t.Cleanup(func() {
		pop()
		salesWebhookConfigCache.Store(nil)
	})
	var built atomic.Int32
	previousFactory := salesPaymentNotifierFactory
	salesPaymentNotifierFactory = func() *salesWebhookNotifier {
		built.Add(1)
		return previousFactory()
	}
	t.Cleanup(func() { salesPaymentNotifierFactory = previousFactory })

	notice := salesTestNotice("stripe_invoice/in_3")
	notice.Enrich = func(context.Context, *SalesPaymentNotice) {
		t.Fatal("an unconfigured notice was enriched")
	}
	connect.AssertEqual(t, deliverSalesPaymentNotice(notice), SalesPaymentNoticeResultUnconfigured)
	connect.AssertEqual(t, built.Load(), int32(0))
}

// The commit hook never waits on Slack: it only queues, and a full queue
// drops the notice rather than block the purchase.
func TestSalesPaymentNoticeCommitNeverWaitsOnSlack(t *testing.T) {
	slack := newSalesTestSlack(t)
	slack.block.Store(true)
	salesTestNetworkName(t, "acme")
	queue := salesTestQueue(t, 1)
	var post server.PostFunction
	previous := salesPaymentNoticeTxPostCommit
	salesPaymentNoticeTxPostCommit = func(tx server.PgTx, key string, p server.PostFunction) bool {
		post = p
		return true
	}
	t.Cleanup(func() { salesPaymentNoticeTxPostCommit = previous })

	AddSalesPaymentNoticeInTx(nil, salesTestNotice("x402/0x1"))
	connect.AssertEqual(t, post(), nil)
	connect.AssertEqual(t, len(queue.notices), 1)
	// the commit posted nothing itself
	connect.AssertEqual(t, len(slack.posts()), 0)

	// the queue is full: the next commit drops its notice, without waiting
	AddSalesPaymentNoticeInTx(nil, salesTestNotice("x402/0x2"))
	connect.AssertEqual(t, post(), nil)
	connect.AssertEqual(t, len(queue.notices), 1)
}

// A Slack that fails, or that never answers, ends in an error the worker
// counts; the payment it announces was committed before any of it.
func TestDeliverSalesPaymentNoticeSlackFailureOrTimeout(t *testing.T) {
	slack := newSalesTestSlack(t)
	salesTestNetworkName(t, "acme")

	slack.status.Store(http.StatusInternalServerError)
	connect.AssertEqual(t, deliverSalesPaymentNotice(salesTestNotice("play_purchase/t/1")), SalesPaymentNoticeResultError)
	connect.AssertEqual(t, len(slack.posts()), salesWebhookAttemptCount)

	// a 4xx other than 429 is not retried
	slack.status.Store(http.StatusNotFound)
	connect.AssertEqual(t, deliverSalesPaymentNotice(salesTestNotice("play_purchase/t/2")), SalesPaymentNoticeResultError)
	connect.AssertEqual(t, len(slack.posts()), salesWebhookAttemptCount+1)

	// a Slack that never answers: each attempt ends at the client's timeout
	slack.block.Store(true)
	connect.AssertEqual(t, deliverSalesPaymentNotice(salesTestNotice("play_purchase/t/3")), SalesPaymentNoticeResultError)
	connect.AssertEqual(t, len(slack.posts()), 2*salesWebhookAttemptCount+1)
}

// The worker posts what the commit queued, after the commit, on its own.
func TestSalesPaymentNoticeWorkerPostsAfterTheCommit(t *testing.T) {
	slack := newSalesTestSlack(t)
	salesTestNetworkName(t, "acme")
	queue := newSalesPaymentNoticeQueue(8)
	previousQueue := salesPaymentNotices
	salesPaymentNotices = queue
	t.Cleanup(func() {
		salesPaymentNotices = previousQueue
		// ends the worker
		close(queue.notices)
	})
	var post server.PostFunction
	previous := salesPaymentNoticeTxPostCommit
	salesPaymentNoticeTxPostCommit = func(tx server.PgTx, key string, p server.PostFunction) bool {
		post = p
		return true
	}
	t.Cleanup(func() { salesPaymentNoticeTxPostCommit = previous })

	AddSalesPaymentNoticeInTx(nil, salesTestNotice("solana_intent/ref-1"))
	select {
	case <-slack.posted:
		t.Fatal("posted before the commit")
	default:
	}
	post()
	select {
	case <-slack.posted:
	case <-time.After(30 * time.Second):
		t.Fatal("the worker did not post")
	}
	connect.AssertEqual(t, strings.Contains(slack.posts()[0], `"text":"*Sale:* Pro monthly (renewal)\n*Network:* acme\n`), true)
	// the worker is done before the test's replacements are put back
	connect.AssertEqual(t, queue.wait(time.Minute), true)
}

// The webhook url is a credential: a transport error carries none of it.
func TestSalesPaymentNoticeErrorOmitsTheWebhookUrl(t *testing.T) {
	notifier := &salesWebhookNotifier{
		httpClient:  &http.Client{Timeout: time.Second},
		retryDelays: []time.Duration{0},
		sleep:       func(context.Context, time.Duration) bool { return true },
	}
	err := notifier.Notify(context.Background(), "https://127.0.0.1:1/services/T0SECRET/B0SECRET/salessecret", "text")
	if err == nil {
		t.Fatal("expected a transport failure")
	}
	for _, secret := range []string{"T0SECRET", "B0SECRET", "salessecret", "/services/"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks the webhook url: %s", err)
		}
	}
	var statusErr *errSalesWebhookStatus
	connect.AssertEqual(t, errors.As(err, &statusErr), false)
}

// A short-lived process (bringyourctl) waits for its notices before it exits:
// the wait ends once the worker has delivered them, and gives up at its
// bound when Slack does not answer.
func TestWaitSalesPaymentNotices(t *testing.T) {
	slack := newSalesTestSlack(t)
	salesTestNetworkName(t, "acme")
	queue := newSalesPaymentNoticeQueue(8)
	previousQueue := salesPaymentNotices
	salesPaymentNotices = queue
	t.Cleanup(func() {
		salesPaymentNotices = previousQueue
		close(queue.notices)
	})

	// nothing queued: nothing to wait for
	connect.AssertEqual(t, WaitSalesPaymentNotices(0), true)

	connect.AssertEqual(t, queue.enqueue(salesTestNotice("crypto_intent/1")), true)
	connect.AssertEqual(t, queue.enqueue(salesTestNotice("crypto_intent/2")), true)
	connect.AssertEqual(t, WaitSalesPaymentNotices(time.Minute), true)
	connect.AssertEqual(t, len(slack.posts()), 2)

	// a duplicate or a dropped notice leaves nothing pending
	connect.AssertEqual(t, queue.enqueue(salesTestNotice("crypto_intent/1")), false)
	connect.AssertEqual(t, WaitSalesPaymentNotices(0), true)

	slack.block.Store(true)
	connect.AssertEqual(t, queue.enqueue(salesTestNotice("crypto_intent/3")), true)
	connect.AssertEqual(t, WaitSalesPaymentNotices(50*time.Millisecond), false)

	// once Slack answers, the worker finishes, before the test's replacements
	// are put back
	slack.releaseHangs()
	connect.AssertEqual(t, WaitSalesPaymentNotices(time.Minute), true)
}
