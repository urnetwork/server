package controller

// The compute balance's notices on the server (BAL-M12-11; DESIGN-COMPUTE §9.8, §9.9, §9.12): every kind renders
// through the shared layout as every account message does (one subject line, no unresolved action, links on ur.io
// alone, a plain-text part without markup, the support footer), naming its amounts to the cent and its network; and a
// notice owed in a ledger change's transaction exists exactly when the change commits, once a key and address.

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/model"
)

// Each kind's notice renders, with the figures its kind names, a link to ur.io where one is set and words where none
// is.
func TestComputeNoticeRendersEachKind(t *testing.T) {
	retry := time.Date(2026, 11, 26, 15, 4, 0, 0, time.UTC)
	days := int64(3)
	linkHref := regexp.MustCompile(`href="([^"]+)"`)
	for _, kind := range sidecarcontroller.ComputeNoticeKinds() {
		for _, renewUrl := range []string{"", "https://ur.io/app/compute"} {
			template := computeNoticeTemplateOf(&sidecarcontroller.ComputeNotice{
				NetworkId: server.NewId(), Kind: kind, Key: "k", AmountNanoUsd: 20_000_000_000, FailureCount: 1, RetryTime: &retry,
				BalanceNanoUsd: 4_120_000_000, AtRiskEnclaveIds: []operator.Id{server.NewId(), server.NewId()}, IndexerDaysLeft: &days,
				RefundedNanoUsd: 37_500_000_000, ForfeitedNanoUsd: 2_010_000_000,
			}, renewUrl)
			template.nameNetwork("Synthetic & <compute>")
			name := string(kind) + "/" + renewUrl
			subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
			if err != nil {
				t.Fatalf("%s: render: %v", name, err)
			}
			if subject == "" || subject != strings.TrimSpace(subject) || strings.ContainsAny(subject, "\r\n") || !strings.Contains(subject, "your URnetwork network Synthetic & <compute>") {
				t.Errorf("%s: the subject %q", name, subject)
			}
			for _, part := range []string{subject, bodyHtml, bodyText} {
				if strings.Contains(part, "{{") {
					t.Errorf("%s: an unresolved action", name)
				}
			}
			if strings.Contains(bodyHtml, "<compute>") || !strings.Contains(bodyHtml, "Synthetic &amp; &lt;compute&gt;") {
				t.Errorf("%s: the network's name is not escaped in the html part", name)
			}
			if strings.Contains(bodyText, "<p") || strings.Contains(bodyText, "&amp;") {
				t.Errorf("%s: the text part holds markup", name)
			}
			if !strings.Contains(bodyText, "https://ur.io/support") || !strings.Contains(bodyHtml, "https://ur.io/support") {
				t.Errorf("%s: no support footer", name)
			}
			for _, match := range linkHref.FindAllStringSubmatch(bodyHtml, -1) {
				if href := match[1]; href != "https://ur.io" && !strings.HasPrefix(href, "https://ur.io/") && href != "mailto:support@ur.io" {
					t.Errorf("%s: a link %q leaves ur.io", name, href)
				}
			}
			if renewUrl != "" && (!strings.Contains(bodyText, renewUrl) || !strings.Contains(bodyHtml, renewUrl)) {
				t.Errorf("%s: no renew link", name)
			}
			if renewUrl == "" && !strings.Contains(bodyText, "compute screen") {
				t.Errorf("%s: no words for the compute screen", name)
			}
			want := map[sidecarcontroller.ComputeNoticeKind][]string{
				sidecarcontroller.ComputeNoticeTopUpFailed:            {"$20.00", "November 26, 2026, 15:04 UTC"},
				sidecarcontroller.ComputeNoticeTopUpOff:               {"$20.00", "third time"},
				sidecarcontroller.ComputeNoticeAuthenticationRequired: {"$20.00", "confirm"},
				sidecarcontroller.ComputeNoticeCardDetached:           {"removed"},
				sidecarcontroller.ComputeNoticeMonthlyFailed:          {"$20.00"},
				sidecarcontroller.ComputeNoticeLow:                    {"$4.12", "2 enclave(s)", "3 days"},
				sidecarcontroller.ComputeNoticeGrace:                  {"$4.12", "2 enclave(s)"},
				sidecarcontroller.ComputeNoticeSuspended:              {"$4.12", "Local inference keeps working"},
				sidecarcontroller.ComputeNoticeClosed:                 {"$37.50", "$2.01"},
			}[kind]
			normalText := strings.Join(strings.Fields(bodyText), " ")
			for _, words := range want {
				if !strings.Contains(normalText, words) || !strings.Contains(bodyHtml, words) {
					t.Errorf("%s: lacks %q", name, words)
				}
			}
		}
	}
	if computeNoticeUsd(4_000_000) != "< $0.01" || computeNoticeUsd(-1_250_000_000) != "-$1.25" || computeNoticeUsd(0) != "$0.00" {
		t.Error("an amount's words")
	}
}

// The outbox rows of a notice's key.
func testComputeNoticeRows(ctx context.Context, networkId server.Id) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `
			SELECT count(*) FROM account_message_outbox WHERE network_id = $1 AND template_name = $2
		`, networkId, (&ComputeNoticeTemplate{}).Name()).Scan(&count))
	})
	return count
}

// A notice owed in a change's transaction is in the outbox once the change commits, once however often the change
// runs, and not at all where the change rolls back; a network no email reaches owes none.
func TestComputeNoticeOwedOnceInTheChangesTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "compute-"+networkId.String()[:8], server.NewId())
		env, err := core.NewEnv(&sidecar.ServiceHost{}, &core.Grants{Db: true})
		if err != nil {
			tb.Fatal(err)
		}
		db, err := env.Db()
		if err != nil {
			tb.Fatal(err)
		}
		notifier := NewComputeServerNotifier(func() string { return "" })
		notice := &sidecarcontroller.ComputeNotice{NetworkId: networkId, Kind: sidecarcontroller.ComputeNoticeLow, Key: "state/" + networkId.String() + "/7", BalanceNanoUsd: 3_000_000_000}
		owe := func(rollback bool) {
			tb.Helper()
			err := db.Tx(ctx, func(tx operator.DbTx) error {
				if err := notifier.NoticeInTx(env, ctx, tx, notice); err != nil {
					return err
				}
				if rollback {
					return context.Canceled
				}
				return nil
			}, operator.DbRetryNone)
			if (err != nil) != rollback {
				tb.Fatalf("the change: %v", err)
			}
		}
		owe(true)
		if testComputeNoticeRows(ctx, networkId) != 0 {
			tb.Fatal("a change that rolled back owed its notice")
		}
		owe(false)
		owe(false)
		if count := testComputeNoticeRows(ctx, networkId); count != 1 {
			tb.Fatalf("%d notices owed for one change", count)
		}
		// a network with no email reachable owes none, and its change still commits
		elsewhere := server.NewId()
		other := &sidecarcontroller.ComputeNotice{NetworkId: elsewhere, Kind: sidecarcontroller.ComputeNoticeGrace, Key: "state/" + elsewhere.String() + "/1"}
		if err := db.Tx(ctx, func(tx operator.DbTx) error {
			return notifier.NoticeInTx(env, ctx, tx, other)
		}); err != nil || testComputeNoticeRows(ctx, elsewhere) != 0 {
			tb.Fatalf("a network with no email: %v", err)
		}
		// a transaction another Db made owes nothing and says so
		if err := notifier.NoticeInTx(env, ctx, nil, notice); err == nil {
			tb.Fatal("a transaction not of core's owed a notice")
		}
	})
}
