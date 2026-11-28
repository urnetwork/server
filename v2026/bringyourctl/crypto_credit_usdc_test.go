package main

// `bringyourctl crypto credit-usdc` (controller/crypto_pay_operator_usdc.go;
// support inbox 8228, 8229 and 8241, owner queue OQ-26).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

func parseCryptoCreditUsdcArgs(args []string) (docopt.Opts, error) {
	return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
}

// Pure: a dry run unless --commit, --commit needs the dry run's digest, and
// `crypto credit` keeps its meaning.
func TestCryptoCreditUsdcCommandLine(t *testing.T) {
	intentId := server.NewId().String()
	signature := "5SyntheticSignature"
	for _, args := range [][]string{
		{"crypto", "credit-usdc", "--intent=" + intentId, "--tx=" + signature},
		{"crypto", "credit-usdc", "--intent=" + intentId, "--tx=" + signature, "--commit", "--expect=digest"},
	} {
		opts, err := parseCryptoCreditUsdcArgs(args)
		if err != nil {
			t.Fatal(args, err)
		}
		crypto, _ := opts.Bool("crypto")
		creditUsdc, _ := opts.Bool("credit-usdc")
		credit, _ := opts.Bool("credit")
		intentValue, _ := opts.String("--intent")
		signatureValue, _ := opts.String("--tx")
		commit, _ := opts.Bool("--commit")
		expect, _ := opts.String("--expect")
		if !crypto || !creditUsdc || credit || intentValue != intentId || signatureValue != signature ||
			commit != slices.Contains(args, "--commit") || (expect == "digest") != commit {
			t.Fatal("command line", args, opts)
		}
	}
	for _, args := range [][]string{
		{"crypto", "credit-usdc"},
		{"crypto", "credit-usdc", "--intent=" + intentId},
		{"crypto", "credit-usdc", "--tx=" + signature},
		{"crypto", "credit-usdc", "--intent=" + intentId, "--tx=" + signature, "--commit"},
		{"crypto", "credit-usdc", "--intent=" + intentId, "--tx=" + signature, "--expect=digest"},
		{"crypto", "credit-usdc", "--intent=" + intentId, "--tx=" + signature, "--dry-run"},
		{"crypto", "credit", "--intent=" + intentId, "--tx=" + signature},
	} {
		if _, err := parseCryptoCreditUsdcArgs(args); err == nil {
			t.Fatal("an invalid command line was accepted", args)
		}
	}
	opts, err := parseCryptoCreditUsdcArgs([]string{"crypto", "credit", "--intent=" + intentId})
	if err != nil {
		t.Fatal(err)
	}
	if credit, _ := opts.Bool("credit"); !credit {
		t.Fatal("crypto credit no longer parses as credit", opts)
	}
	if creditUsdc, _ := opts.Bool("credit-usdc"); creditUsdc {
		t.Fatal("crypto credit parses as credit-usdc", opts)
	}
}

// Pure: the dry run's process is read only before it opens a database
// session, and main runs the command.
func TestCryptoCreditUsdcDryRunProcessIsReadOnly(t *testing.T) {
	source, err := os.ReadFile("crypto_credit_usdc.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	readOnly := strings.Index(text, "if !commit {\n\t\tserver.RequirePgReadOnly()\n\t}")
	session := strings.Index(text, "session.NewLocalClientSession(")
	run := strings.Index(text, "return runCryptoCreditUsdc(")
	if readOnly < 0 || session < readOnly || run < readOnly {
		t.Fatal("the dry run opens a database session before its process is read only")
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "os.Exit(cryptoCreditUsdcCommand(opts, os.Stdout, os.Stderr))") {
		t.Fatal("main does not run the credit-usdc command")
	}
}

// The plan of support inbox 8228's case, with synthetic ids.
func testCryptoCreditUsdcPlan(state string) *controller.CryptoUsdcCreditPlan {
	networkId := server.NewId()
	quotedAt := time.Date(2026, 11, 7, 5, 27, 20, 0, time.UTC)
	sentAt := time.Date(2026, 11, 7, 5, 59, 29, 0, time.UTC)
	plan := &controller.CryptoUsdcCreditPlan{
		Time:      time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC),
		IntentId:  server.NewId(),
		Signature: "5SyntheticSignature",
		State:     state,
		Invoice: &controller.CryptoUsdcCreditInvoice{
			Asset:             model.CryptoAssetUsdcSolana,
			Delivery:          model.CryptoDeliveryAccount,
			NetworkId:         &networkId,
			NetworkExists:     true,
			Item:              model.SolanaPlanYearly,
			Tier:              "regional",
			PriceUsdNanoCents: model.UsdToNanoCents(4),
			AmountAtomic:      4_005_505,
			QuotedAt:          quotedAt,
			ExpiresAt:         quotedAt.Add(time.Hour),
			CreateTime:        quotedAt,
			SolanaIntentFound: true,
		},
		Payment: &controller.CryptoUsdcCreditPayment{
			Signature:       "5SyntheticSignature",
			Reason:          model.SolanaUnfulfilledReasonNoIntent,
			AmountAtomic:    4_093_109,
			TransactionTime: &sentAt,
			RecordTime:      sentAt.Add(2 * time.Second),
			TiedNetworkIds:  []server.Id{},
		},
		SignatureCredits: &model.SolanaSignatureCredits{},
	}
	plan.Invoice.IntentId = plan.IntentId
	if state != controller.CryptoUsdcCreditStateRefused {
		plan.UsdNanoCents = model.UsdToNanoCents(4)
		plan.OverQuoteAtomic = 87_604
		plan.Digest = strings.Repeat("ab", 32)
	}
	return plan
}

// Pure: the report prints before any error. A dry run that can credit ends
// with the exact commit command; a commit waits for the sales notice and ends
// with the audit line; a refusal or a failure exits 1 without either; an
// argument that does not parse exits 2 and runs nothing.
func TestRunCryptoCreditUsdcOutput(t *testing.T) {
	run := func(commit bool, plan *controller.CryptoUsdcCreditPlan, err error, intentValue string, signatureValue string, noticesPosted bool) (int, string, string, bool, int) {
		var stdout, stderr bytes.Buffer
		ran := false
		waits := 0
		exit := runCryptoCreditUsdc(
			&stdout,
			&stderr,
			intentValue,
			signatureValue,
			commit,
			func(intentId server.Id, signature string) (*controller.CryptoUsdcCreditPlan, error) {
				ran = true
				if intentId.String() != strings.TrimSpace(intentValue) || signature != strings.TrimSpace(signatureValue) {
					t.Fatal("arguments", intentId, signature)
				}
				return plan, err
			},
			func() bool {
				waits += 1
				return noticesPosted
			},
		)
		return exit, stdout.String(), stderr.String(), ran, waits
	}

	// arguments that do not parse
	plan := testCryptoCreditUsdcPlan(controller.CryptoUsdcCreditStateCredit)
	if exit, _, stderr, ran, _ := run(false, plan, nil, "not-an-id", plan.Signature, true); exit != 2 || ran || !strings.Contains(stderr, "--intent") {
		t.Fatal("a bad intent id", exit, ran, stderr)
	}
	if exit, _, stderr, ran, _ := run(false, plan, nil, plan.IntentId.String(), "  ", true); exit != 2 || ran || !strings.Contains(stderr, "--tx is empty") {
		t.Fatal("an empty signature", exit, ran, stderr)
	}

	// a dry run that can credit
	exit, stdout, stderr, _, waits := run(false, plan, nil, " "+plan.IntentId.String()+" ", plan.Signature+"\n", true)
	commitLine := "to credit exactly this: bringyourctl crypto credit-usdc --intent=" + plan.IntentId.String() +
		" --tx=" + plan.Signature + " --commit --expect=" + plan.Digest + "\n"
	for _, line := range []string{
		"crypto credit-usdc: dry run (read only) at 2026-11-09T20:00:00Z\n",
		"invoice " + plan.IntentId.String() + ": usdc_solana yearly, tier \"regional\", delivery account\n",
		"  linked to network " + plan.Invoice.NetworkId.String() + "\n",
		"  price $4.00; quote 4.005505 usdc, quoted 2026-11-07T05:27:20Z, expires 2026-11-07T06:27:20Z\n",
		"payment 5SyntheticSignature: 4.093109 usdc, recorded no_intent\n",
		"  sent 2026-11-07T05:59:29Z (on-chain time)\n",
		"  0.087604 usdc over the quote\n",
		"credit $4.00: on time: the price\n",
		"  the unfulfilled record is resolved (removed) in the credit's transaction\n",
		"state credit\n",
		"digest " + plan.Digest + "\n",
	} {
		if !strings.Contains(stdout, line) {
			t.Fatalf("the dry run does not print %q:\n%s", line, stdout)
		}
	}
	if exit != 0 || stderr != "" || waits != 0 || !strings.HasSuffix(stdout, commitLine) {
		t.Fatal("dry run", exit, stdout, stderr, waits)
	}

	// late, without an on-chain time
	late := testCryptoCreditUsdcPlan(controller.CryptoUsdcCreditStateCredit)
	late.Late = true
	late.Payment.TransactionTime = nil
	_, stdout, _, _, _ = run(false, late, nil, late.IntentId.String(), late.Signature, true)
	if !strings.Contains(stdout, "credit $4.00: late, sent after the quote expired: what arrived, capped at the price\n") ||
		!strings.Contains(stdout, "  sent by 2026-11-07T05:59:31Z (no on-chain time; recorded then)\n") {
		t.Fatal("late", stdout)
	}

	// a code invoice
	code := testCryptoCreditUsdcPlan(controller.CryptoUsdcCreditStateCredit)
	code.Invoice.Delivery = model.CryptoDeliveryCode
	code.Invoice.NetworkId = nil
	code.Invoice.NetworkExists = false
	_, stdout, _, _, _ = run(false, code, nil, code.IntentId.String(), code.Signature, true)
	if !strings.Contains(stdout, "  linked to no network: it delivers a balance code\n") {
		t.Fatal("code", stdout)
	}

	// a refusal
	refused := testCryptoCreditUsdcPlan(controller.CryptoUsdcCreditStateRefused)
	refused.Refusals = []string{"the payment of 3.9 USDC is under the quote of 4.005505 USDC: synthetic"}
	exit, stdout, stderr, _, waits = run(false, refused, nil, refused.IntentId.String(), refused.Signature, true)
	if exit != 1 || waits != 0 || !strings.Contains(stdout, "state refused\n  refused: the payment of 3.9 USDC is under the quote") ||
		strings.Contains(stdout, "to credit exactly") || strings.Contains(stdout, "digest ") || strings.Contains(stdout, "credit $") ||
		!strings.Contains(stderr, controller.ErrCryptoUsdcCreditRefused.Error()) {
		t.Fatal("refusal", exit, stdout, stderr)
	}

	// a commit that credited: the sales notice, then the audit line
	credited := testCryptoCreditUsdcPlan(controller.CryptoUsdcCreditStateCredited)
	exit, stdout, stderr, _, waits = run(true, credited, nil, credited.IntentId.String(), credited.Signature, true)
	auditLine := "audit crypto_credit_usdc intent_id=" + credited.IntentId.String() + " tx=5SyntheticSignature network_id=" +
		credited.Invoice.NetworkId.String() + " received_atomic=4093109 quote_atomic=4005505 usd_nano_cents=4000000000 late=false digest=" +
		credited.Digest + "\n"
	if exit != 0 || stderr != "" || waits != 1 || !strings.HasSuffix(stdout, auditLine) ||
		!strings.Contains(stdout, "crypto credit-usdc: commit at") || !strings.Contains(stdout, "state credited\n") ||
		!strings.Contains(stdout, "credited: invoice "+credited.IntentId.String()+" is paid by 5SyntheticSignature; its unfulfilled record is resolved\n") ||
		strings.Contains(stdout, "to credit exactly") || strings.Contains(stdout, "not posted") {
		t.Fatal("commit", exit, stdout, stderr, waits)
	}
	// the notice did not post in time: said, and still the audit line
	exit, stdout, _, _, _ = run(true, credited, nil, credited.IntentId.String(), credited.Signature, false)
	if exit != 0 || !strings.Contains(stdout, "the sales channel notice was not posted before exit\n") || !strings.HasSuffix(stdout, auditLine) {
		t.Fatal("commit without the notice", exit, stdout)
	}

	// a commit with another digest: the plan as it is now, and the error
	exit, stdout, stderr, _, waits = run(true, plan, controller.ErrCryptoUsdcCreditDigestMismatch, plan.IntentId.String(), plan.Signature, true)
	if exit != 1 || waits != 0 || !strings.Contains(stdout, "digest "+plan.Digest) || strings.Contains(stdout, "audit ") ||
		!strings.Contains(stderr, controller.ErrCryptoUsdcCreditDigestMismatch.Error()) {
		t.Fatal("mismatch", exit, stdout, stderr)
	}
	// a failure without a plan
	exit, _, stderr, _, waits = run(true, nil, errors.New("synthetic failure"), plan.IntentId.String(), plan.Signature, true)
	if exit != 1 || waits != 0 || !strings.Contains(stderr, "synthetic failure") {
		t.Fatal("failure", exit, stderr)
	}
}

// The command end to end on the test database, for the shape of support
// inbox 8228: the dry run in a read-only process, the commit with the digest
// it printed, and the same commit again, refused. Nothing it prints is an
// email.
func TestCryptoCreditUsdcCommandCreditsTheInvoice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-credit-usdc", server.NewId())

		// a year quoted 40.005505 USDC an hour ago, expiring now
		reference := "SyntheticCreditUsdcReference" + server.NewId().String()
		quotedAt := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		tokenHash := sha256.Sum256([]byte("synthetic-status-token"))
		intent := &model.CryptoPaymentIntent{
			IntentId:                 server.NewId(),
			StatusTokenHash:          tokenHash[:],
			Asset:                    model.CryptoAssetUsdcSolana,
			Delivery:                 model.CryptoDeliveryAccount,
			NetworkId:                &networkId,
			Item:                     model.SolanaPlanYearly,
			Tier:                     "standard",
			PriceUsdNanoCents:        model.UsdToNanoCents(40),
			RegularPriceUsdNanoCents: model.UsdToNanoCents(40),
			Email:                    "buyer@synthetic.example",
			Address:                  "SyntheticRecipient1111111111111111111111111",
			SolanaReference:          &reference,
			RateUsd:                  1,
			QuotedAt:                 quotedAt,
			ExpiresAt:                quotedAt.Add(time.Hour),
			CreateTime:               quotedAt,
		}
		if err := model.CreateLinkedSolanaCryptoPaymentIntent(ctx, intent, 40, func() int64 { return 5505 }); err != nil {
			t.Fatal(err)
		}
		if intent.AmountAtomic != 40_005_505 {
			t.Fatal("quote", intent.AmountAtomic)
		}

		// the exchange sent the quote and its fee, without the reference, 32
		// minutes into the quote; the webhook recorded it unfulfilled
		signature := "SyntheticCreditUsdcSignature" + server.NewId().String()
		sentAt := quotedAt.Add(32 * time.Minute)
		note := "memo-less: no intent was quoted 40.093109 USDC"
		if err := model.RecordUnfulfilledSolanaPayment(ctx, &model.UnfulfilledSolanaPayment{
			TxSignature:     signature,
			Reason:          model.SolanaUnfulfilledReasonNoIntent,
			TokenAmountUsd:  40.093109,
			TransactionTime: &sentAt,
			MatchNote:       &note,
		}); err != nil {
			t.Fatal(err)
		}

		args := []string{"crypto", "credit-usdc", "--intent=" + intent.IntentId.String(), "--tx=" + signature}
		noEmail := func(output string) {
			if strings.Contains(output, "@") || strings.Contains(output, "synthetic.example") {
				t.Fatal("the command printed an email", output)
			}
		}

		// the dry run: its process is read only
		opts, err := parseCryptoCreditUsdcArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		var stdout, stderr bytes.Buffer
		exit := cryptoCreditUsdcCommand(opts, &stdout, &stderr)
		writeRefused := func() (refused bool) {
			defer func() {
				refused = recover() != nil
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE crypto_payment_intent SET credit_failure_count = credit_failure_count + 1 WHERE intent_id = $1`,
					intent.IntentId,
				))
			}, server.OptNoRetry())
			return false
		}()
		server.Testing_ReleasePgReadOnly()
		if exit != 0 || stderr.Len() != 0 || !writeRefused {
			t.Fatal("dry run", exit, stdout.String(), stderr.String(), writeRefused)
		}
		noEmail(stdout.String())
		t.Logf("dry run:\n%s", stdout.String())
		if !strings.Contains(stdout.String(), "credit $40.00: on time: the price\n") || !strings.Contains(stdout.String(), "  0.087604 usdc over the quote\n") {
			t.Fatal("dry run plan", stdout.String())
		}
		match := regexp.MustCompile(`(?m)^to credit exactly this: bringyourctl crypto credit-usdc --intent=\S+ --tx=\S+ --commit --expect=([0-9a-f]{64})$`).FindStringSubmatch(stdout.String())
		if match == nil {
			t.Fatal("the dry run printed no commit line", stdout.String())
		}
		digest := match[1]
		if model.GetCryptoPaymentIntent(ctx, intent.IntentId).CreditedAt != nil {
			t.Fatal("the dry run credited the invoice")
		}

		// the commit
		commitOpts, err := parseCryptoCreditUsdcArgs(append(args, "--commit", "--expect="+digest))
		if err != nil {
			t.Fatal(err)
		}
		stdout.Reset()
		stderr.Reset()
		if exit := cryptoCreditUsdcCommand(commitOpts, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
			t.Fatal("commit", exit, stdout.String(), stderr.String())
		}
		noEmail(stdout.String())
		t.Logf("commit:\n%s", stdout.String())
		if !strings.Contains(stdout.String(), "state credited\n") ||
			!strings.Contains(stdout.String(), "audit crypto_credit_usdc intent_id="+intent.IntentId.String()+" tx="+signature) {
			t.Fatal("commit output", stdout.String())
		}
		credited := model.GetCryptoPaymentIntent(ctx, intent.IntentId)
		if credited.CreditedAt == nil || *credited.CreditedUsdNanoCents != model.UsdToNanoCents(40) || !model.IsProNetwork(ctx, networkId) ||
			!model.IsSolanaPaymentCompleted(ctx, signature) {
			t.Fatal("the commit did not credit the invoice")
		}
		if _, err := model.GetUnfulfilledSolanaPayment(ctx, signature); err == nil {
			t.Fatal("the commit left the unfulfilled record")
		}

		// the same commit again is refused and writes nothing
		stdout.Reset()
		stderr.Reset()
		if exit := cryptoCreditUsdcCommand(commitOpts, &stdout, &stderr); exit != 1 ||
			!strings.Contains(stdout.String(), "refused: the invoice is already credited") ||
			!strings.Contains(stderr.String(), controller.ErrCryptoUsdcCreditRefused.Error()) {
			t.Fatal("rerun", exit, stdout.String(), stderr.String())
		}
		noEmail(stdout.String() + stderr.String())
		t.Logf("rerun:\n%s%s", stdout.String(), stderr.String())
	})
}
