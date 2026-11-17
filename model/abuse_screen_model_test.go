package model

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §21.6's screen at the server's gates (abuse_screen_model.go; ABUSE-M5-08): allow, deny and review through
// ScreenNetworkName, record owners, alias claims, renames and new networks; a review case opened once, a retry refused
// until a screen_allow, then passing; probation refusing with no case; a verified brand domain allowed; the new
// network's screen off with the dns gate. On the private stack, in a build with sidecar_pending.

// A network past probation (§21.7), so a review verdict holds a name rather than denying it.
func testScreenNetwork(t testing.TB, ctx context.Context) server.Id {
	t.Helper()
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "screen-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
	testScreenAgeNetwork(ctx, networkId, 30*24*time.Hour)
	return networkId
}

// Moves the network's creation back by age.
func testScreenAgeNetwork(ctx context.Context, networkId server.Id, age time.Duration) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network SET create_time = $2 WHERE network_id = $1`,
			networkId,
			server.NowUtc().Add(-age),
		))
	})
}

// The screen's brand cases of the network, by target.
func testScreenCases(t testing.TB, ctx context.Context, networkId server.Id) map[string][]*AbuseCase {
	t.Helper()
	targetCases := map[string][]*AbuseCase{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+abuseCaseColumns+` FROM abuse_case WHERE abuse_case.network_id = $1 ORDER BY abuse_case.case_id`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				abuseCase := scanAbuseCase(result)
				targetCases[abuseCase.Target] = append(targetCases[abuseCase.Target], abuseCase)
			}
		})
	})
	return targetCases
}

// A reviewer's screen_allow of name under the case, as the staff route's action kind records it.
func testScreenAllow(t testing.TB, ctx context.Context, caseId server.Id, name string) *AbuseAction {
	t.Helper()
	reviewerId := server.NewId()
	var action *AbuseAction
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		action, _, _, err = RecordAbuseActionInTx(ctx, tx, &AbuseActionArgs{
			ActionId:     server.NewId(),
			CaseId:       caseId,
			Kind:         AbuseActionKindScreenAllow,
			Target:       name,
			ReviewerId:   &reviewerId,
			ReviewReason: "the network's own support desk",
		}, server.NowUtc(), DefaultAbuseSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
	return action
}

// A brand's first domain from the screen's list, so no real hostname is written here.
func testScreenBrandDomain(t testing.TB, brand string) string {
	t.Helper()
	for _, screenBrand := range sidecar.ScreenBrands() {
		if screenBrand.Brand == brand && 0 < len(screenBrand.Domains) {
			return screenBrand.Domains[0]
		}
	}
	t.Fatalf("the screen lists no domain for %s", brand)
	return ""
}

// The pure screen with a network's facts: allow, deny, review with no case, and a reviewer's allow of one name only.
func TestScreenWithNetworkFactsVerdicts(t *testing.T) {
	networkId := server.NewId()
	facts := &NetworkScreenFacts{}
	if err := ScreenWithNetworkFacts(networkId, AbuseScreenSubjectName, "zephyr-shop", facts); err != nil {
		t.Fatalf("an ordinary name: %v", err)
	}
	if err := ScreenWithNetworkFacts(networkId, AbuseScreenSubjectName, "paypal", facts); !errors.Is(err, ErrAbuseScreenDenied) {
		t.Fatalf("an unverified brand: %v", err)
	}
	review := AbuseScreenReviewOf(ScreenWithNetworkFacts(networkId, AbuseScreenSubjectHost, "paypal-login", facts))
	if review == nil || review.CaseId != nil || review.Name != "paypal-login" || review.Subject != AbuseScreenSubjectHost {
		t.Fatalf("a brand with a keyword: %+v", review)
	}
	if refusal := review.Refusal(); refusal.LimitKey != AbuseScreenReviewLimitKey || refusal.Upgrade {
		t.Fatalf("the refusal: %+v", refusal)
	}
	allowed := &NetworkScreenFacts{AllowedNames: []string{"paypal-login"}}
	if err := ScreenWithNetworkFacts(networkId, AbuseScreenSubjectHost, "paypal-login", allowed); err != nil {
		t.Fatalf("an allowed name: %v", err)
	}
	if AbuseScreenReviewOf(ScreenWithNetworkFacts(networkId, AbuseScreenSubjectHost, "paypal-support", allowed)) == nil {
		t.Fatal("an allow lets another name pass")
	}
	// probation, and a network of which nothing is known, hold no name for review
	for _, probation := range []*NetworkScreenFacts{{Probation: true, AllowedNames: []string{"paypal-login"}}, nil} {
		if err := ScreenWithNetworkFacts(networkId, AbuseScreenSubjectHost, "paypal-support", probation); !errors.Is(err, ErrAbuseScreenDenied) {
			t.Fatalf("on probation: %v", err)
		}
	}
	// the reserved block guards names below <root>, and a network's own zone keeps www (owner decision 419)
	if err := ScreenWithNetworkFacts(networkId, AbuseScreenSubjectName, "www", facts); !errors.Is(err, ErrAbuseScreenDenied) {
		t.Fatalf("a reserved label: %v", err)
	}
	if err := ScreenWithNetworkFacts(networkId, AbuseScreenSubjectHost, "www", facts); err != nil {
		t.Fatalf("www in the network's zone: %v", err)
	}
	for _, standing := range []AbuseStanding{AbuseStandingProbation, AbuseStandingRestricted, "frozen"} {
		if !abuseStandingScreensAsProbation(standing) {
			t.Errorf("%s screens as standard", standing)
		}
	}
	for _, standing := range []AbuseStanding{AbuseStandingStandard, AbuseStandingTrusted} {
		if abuseStandingScreensAsProbation(standing) {
			t.Errorf("%s screens as probation", standing)
		}
	}
}

// A review verdict opens one case, category brand and reporter class screen, joined by the network; a retry is refused
// on the same case; a reviewer's screen_allow lets the retry pass; a lift of the allow sends the name back to review on
// the case still open.
func TestScreenNetworkNameReviewsOnceUntilAllowed(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := testScreenNetwork(t, ctx)

		if err := ScreenNetworkName(ctx, networkId, "zephyr-shop"); err != nil {
			t.Fatalf("an ordinary name: %v", err)
		}
		if err := ScreenNetworkName(ctx, networkId, "paypal"); !errors.Is(err, ErrAbuseScreenDenied) {
			t.Fatalf("an unverified brand: %v", err)
		}
		if cases := testScreenCases(t, ctx, networkId); len(cases) != 0 {
			t.Fatalf("an allow or a deny opened %d cases", len(cases))
		}

		review := AbuseScreenReviewOf(ScreenNetworkName(ctx, networkId, "paypal-support"))
		if review == nil || review.CaseId == nil {
			t.Fatalf("a brand with a keyword: %+v", review)
		}
		cases := testScreenCases(t, ctx, networkId)["paypal-support"]
		if len(cases) != 1 {
			t.Fatalf("%d cases", len(cases))
		}
		abuseCase := cases[0]
		if abuseCase.CaseId != *review.CaseId || abuseCase.Category != AbuseCategoryBrand ||
			abuseCase.ReporterClass != AbuseReporterClassScreen || abuseCase.TargetKind != AbuseTargetKindLabel ||
			abuseCase.JoinKind != AbuseJoinKindNetwork || abuseCase.JoinKey != networkId.String() ||
			abuseCase.State != AbuseCaseStateOpen || abuseCase.Barred || abuseCase.ReviewDueTime == nil {
			t.Fatalf("the case: %+v", abuseCase)
		}

		// a retry, past the coalescing window too, counts on the same case
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE abuse_case SET create_time = create_time - interval '30 days' WHERE case_id = $1`,
				abuseCase.CaseId,
			))
		})
		retry := AbuseScreenReviewOf(ScreenNetworkName(ctx, networkId, "paypal-support"))
		if retry == nil || retry.CaseId == nil || *retry.CaseId != abuseCase.CaseId {
			t.Fatalf("the retry: %+v", retry)
		}
		if cases := testScreenCases(t, ctx, networkId)["paypal-support"]; len(cases) != 1 {
			t.Fatalf("a retry opened another case: %d", len(cases))
		}

		allow := testScreenAllow(t, ctx, abuseCase.CaseId, "paypal-support")
		if allow.Kind != AbuseActionKindScreenAllow || allow.ReviewState != AbuseReviewStateUpheld || allow.NetworkId == nil || *allow.NetworkId != networkId {
			t.Fatalf("the allow: %+v", allow)
		}
		if err := ScreenNetworkName(ctx, networkId, "paypal-support"); err != nil {
			t.Fatalf("the retry after the allow: %v", err)
		}
		if names := GetNetworkScreenAllowedNames(ctx, networkId); !slices.Equal(names, []string{"paypal-support"}) {
			t.Fatalf("allowed names %v", names)
		}
		// another network's retry is not allowed by it
		otherNetworkId := testScreenNetwork(t, ctx)
		if AbuseScreenReviewOf(ScreenNetworkName(ctx, otherNetworkId, "paypal-support")) == nil {
			t.Fatal("another network's name passed on this network's allow")
		}

		// a lift of the allow: the name goes to review again, on the case still open
		server.Tx(ctx, func(tx server.PgTx) {
			reviewerId := server.NewId()
			_, _, _, _, err := RecordAbuseLiftInTx(ctx, tx, &AbuseLiftArgs{
				ActionId:       server.NewId(),
				CaseId:         abuseCase.CaseId,
				LiftedActionId: allow.ActionId,
				ReviewState:    AbuseReviewStateLifted,
				ReviewerId:     &reviewerId,
				ReviewReason:   "allowed in error",
			}, server.NowUtc(), DefaultAbuseSettings())
			if err != nil {
				t.Fatal(err)
			}
		})
		if review := AbuseScreenReviewOf(ScreenNetworkName(ctx, networkId, "paypal-support")); review == nil {
			t.Fatal("the name passed after its allow was lifted")
		}
	})
}

// On probation a review verdict is a plain refusal and opens no case (§21.7); a network that does not exist is refused.
func TestScreenNetworkNameOnProbationOpensNoCase(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "screen-probation", server.NewId())
		for _, name := range []string{"paypal-support", "paypal"} {
			if err := ScreenNetworkName(ctx, networkId, name); !errors.Is(err, ErrAbuseScreenDenied) {
				t.Fatalf("%s on probation: %v", name, err)
			}
		}
		if cases := testScreenCases(t, ctx, networkId); len(cases) != 0 {
			t.Fatalf("probation opened %d cases", len(cases))
		}
		if err := ScreenNetworkName(ctx, networkId, "zephyr-shop"); err != nil {
			t.Fatalf("an ordinary name on probation: %v", err)
		}
		if err := ScreenNetworkName(ctx, server.NewId(), "zephyr-shop"); !errors.Is(err, ErrAbuseScreenDenied) {
			t.Fatalf("no network: %v", err)
		}
	})
}

// A brand's own name passes once the network verified one of the brand's domains (a network_domain fixture row), and
// a brand with a keyword still goes to review.
func TestScreenNetworkNameVerifiedBrandAllowed(t *testing.T) {
	requireLegalHoldTables(t)
	if !server.SidecarMg6Linked() {
		t.Skip("MG6 is pending: network_domain runs with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := testScreenNetwork(t, ctx)
		if err := ScreenNetworkHost(ctx, networkId, "paypal"); !errors.Is(err, ErrAbuseScreenDenied) {
			t.Fatalf("before the domain: %v", err)
		}
		testDnsNetworkDomain(t, ctx, networkId, testScreenBrandDomain(t, "paypal"), true)
		if err := ScreenNetworkHost(ctx, networkId, "paypal"); err != nil {
			t.Fatalf("the brand with its domain verified: %v", err)
		}
		if err := ScreenNetworkName(ctx, networkId, "paypal"); err != nil {
			t.Fatalf("the brand as a name with its domain verified: %v", err)
		}
		if AbuseScreenReviewOf(ScreenNetworkHost(ctx, networkId, "paypal-support")) == nil {
			t.Fatal("a brand with a keyword passed on the domain alone")
		}
		facts := ScreenFactsForNetwork(ctx, networkId, "paypal", server.NowUtc())
		if facts.Probation || len(facts.VerifiedDomains) != 1 || facts.ReviewAllowed {
			t.Fatalf("the facts: %+v", facts)
		}
	})
}

// A record owner's host label through §12.8's write: a review answers QuotaRefusal{limit_key: screen_review} and
// writes nothing, its case opened once though the write's transaction rolled back, and a reviewer's allow lets the
// retry land. An unverified brand stays refused as before.
func TestDnsRecordsScreenReviewHoldsUntilAllowed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		owner := "paypal-login." + world.apex
		for range 2 {
			serial := world.serial(t, ctx)
			_, err := world.write(ctx, testDnsUpsert(owner, "TXT", `"x"`))
			apiError := testDnsApiError(t, err)
			if apiError.Status != http.StatusTooManyRequests || apiError.Code != sidecar.DnsApiCodeQuota ||
				apiError.Refusal == nil || apiError.Refusal.LimitKey != AbuseScreenReviewLimitKey {
				t.Fatalf("a held host: %+v", apiError)
			}
			if world.serial(t, ctx) != serial {
				t.Fatal("a held host wrote")
			}
		}
		cases := testScreenCases(t, ctx, world.networkId)["paypal-login"]
		if len(cases) != 1 || cases[0].TargetKind != AbuseTargetKindHost || cases[0].Category != AbuseCategoryBrand {
			t.Fatalf("the cases: %+v", cases)
		}
		testScreenAllow(t, ctx, cases[0].CaseId, "paypal-login")
		if _, err := world.write(ctx, testDnsUpsert(owner, "TXT", `"x"`)); err != nil {
			t.Fatalf("the allowed host: %v", err)
		}
		_, err := world.write(ctx, testDnsUpsert("paypal."+world.apex, "TXT", `"x"`))
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusForbidden || apiError.Code != sidecar.DnsApiCodeRefused {
			t.Fatalf("an unverified brand: %+v", apiError)
		}
	})
}

// A rename behind the dns gate: a held name refused as screen_review naming its case, then renamed once allowed; a
// brand without its domain is not available. With the gate off the rename keeps ValidateDnsName's rule.
func TestRenameScreensBehindTheDnsGate(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		renamer := testNetworkRenamer(server.NowUtc)
		networkId, clientSession := testRenameNetwork(t, ctx, "rename-screened-network")
		testScreenAgeNetwork(ctx, networkId, 30*24*time.Hour)

		// the gate off: the label rules refuse a brand as invalid, opening no case
		requireRenameRefused(t, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "paypal-support"}), RenameNetworkRefusalInvalid)

		t.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  dns:\n    enabled: true\n"))
		result := renamer.rename(clientSession, &RenameNetworkArgs{NewName: "paypal-support"})
		requireRenameRefused(t, result, RenameNetworkRefusalScreenReview)
		cases := testScreenCases(t, ctx, networkId)["paypal-support"]
		if len(cases) != 1 || !strings.Contains(result.Error.Message, cases[0].CaseId.String()) || strings.Contains(result.Error.Message, "paypal") {
			t.Fatalf("the refusal %q and cases %+v", result.Error.Message, cases)
		}
		requireRenameRefused(t, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "paypal-help"}), RenameNetworkRefusalNotAvailable)
		testScreenAllow(t, ctx, cases[0].CaseId, "paypal-support")
		requireRenamed(t, ctx, renamer.rename(clientSession, &RenameNetworkArgs{NewName: "paypal-support"}), networkId, "paypal-support")

		// someone who is not the owner opens no case
		stranger := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: server.NewId()})
		requireRenameRefused(t, renamer.rename(stranger, &RenameNetworkArgs{NewName: "apple-support"}), RenameNetworkRefusalNotOwner)
		if cases := testScreenCases(t, ctx, networkId)["apple-support"]; len(cases) != 0 {
			t.Fatalf("a stranger's rename opened %d cases", len(cases))
		}
	})
}

// A new network's name: unscreened while the dns gate is off, and behind it screened on probation's facts, so a brand
// and a brand with a keyword are both not available and an ordinary name passes; NetworkCheck answers alike, and a
// generated name the screen refuses is drawn again.
func TestScreenNewNetworkNameBehindTheDnsGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, restore := Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		check := func(name string) bool {
			t.Helper()
			result, err := NetworkCheck(&NetworkCheckArgs{NetworkName: name}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			return result.Available
		}
		for _, name := range []string{"paypal-support", "paypal", "zephyr-shop"} {
			if err := ScreenNewNetworkName(name); err != nil {
				t.Fatalf("%s with the gate off: %v", name, err)
			}
			if !check(name) {
				t.Fatalf("%s with the gate off is not available", name)
			}
		}

		t.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  dns:\n    enabled: true\n"))
		for _, name := range []string{"paypal-support", "paypal"} {
			if err := ScreenNewNetworkName(name); !errors.Is(err, ErrAbuseScreenDenied) || strings.Contains(err.Error(), "paypal") {
				t.Fatalf("%s behind the gate: %v", name, err)
			}
			if check(name) {
				t.Fatalf("%s behind the gate is available", name)
			}
		}
		if err := ScreenNewNetworkName("zephyr-shop"); err != nil || !check("zephyr-shop") {
			t.Fatalf("an ordinary name behind the gate: %v", err)
		}

		draws := []string{"apple-wallet-12", "zephyr-otter-7"}
		drawn, err := drawScreenedNetworkName(func() (string, error) {
			name := draws[0]
			draws = draws[1:]
			return name, nil
		}, DefaultAbuseScreenSettings())
		if err != nil || drawn != "zephyr-otter-7" {
			t.Fatalf("drew %q: %v", drawn, err)
		}
		settings := DefaultAbuseScreenSettings()
		settings.GeneratedNameDrawCount = 2
		if _, err := drawScreenedNetworkName(func() (string, error) { return "apple-wallet-12", nil }, settings); !errors.Is(err, ErrAbuseScreenDenied) {
			t.Fatalf("a run of refused draws: %v", err)
		}
	})
}

// tls/'s facts carry the network's allowed hosts (TlsListenFacts.ScreenAllowedHosts), which ValidateListen reads from
// memory per heartbeat.
func TestTlsListenFactsCarryScreenAllows(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := testScreenNetwork(t, ctx)
		review := AbuseScreenReviewOf(ScreenNetworkHost(ctx, networkId, "apple-login"))
		if review == nil || review.CaseId == nil {
			t.Fatalf("the review: %+v", review)
		}
		facts, err := GetTlsListenFacts(ctx, networkId, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		if len(facts.ScreenAllowedHosts) != 0 {
			t.Fatalf("allowed hosts before the allow: %v", facts.ScreenAllowedHosts)
		}
		testScreenAllow(t, ctx, *review.CaseId, "apple-login")
		facts, err = GetTlsListenFacts(ctx, networkId, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(facts.ScreenAllowedHosts, []string{"apple-login"}) {
			t.Fatalf("allowed hosts: %v", facts.ScreenAllowedHosts)
		}
	})
}

// A screen_allow names one lowercase label on a network's brand case, and is a reviewer's.
func TestAbuseScreenAllowRefusesWhatNoGateScreens(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := testScreenNetwork(t, ctx)
		review := AbuseScreenReviewOf(ScreenNetworkName(ctx, networkId, "paypal-support"))
		if review == nil || review.CaseId == nil {
			t.Fatalf("the review: %+v", review)
		}
		reviewerId := server.NewId()
		record := func(args *AbuseActionArgs) error {
			var err error
			server.Tx(ctx, func(tx server.PgTx) {
				_, _, _, err = RecordAbuseActionInTx(ctx, tx, args, server.NowUtc(), DefaultAbuseSettings())
			})
			return err
		}
		for _, target := range []string{"paypal.support", "Paypal-Support", "", "-paypal", strings.Repeat("a", 64)} {
			err := record(&AbuseActionArgs{ActionId: server.NewId(), CaseId: *review.CaseId, Kind: AbuseActionKindScreenAllow, Target: target, ReviewerId: &reviewerId})
			if !errors.Is(err, ErrAbuseActionInvalid) {
				t.Errorf("target %q: %v", target, err)
			}
		}
		if err := record(&AbuseActionArgs{ActionId: server.NewId(), CaseId: *review.CaseId, Kind: AbuseActionKindScreenAllow, Target: "paypal-support"}); !errors.Is(err, ErrAbuseActionInvalid) {
			t.Fatalf("a machine's allow: %v", err)
		}
		// a case of another category takes no allow
		var otherCaseId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			otherCaseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
				NetworkId:     &networkId,
				Category:      AbuseCategoryPhishing,
				Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: "paypal-support"},
				ReporterClass: AbuseReporterClassStaff,
				JoinKind:      AbuseJoinKindNetwork,
				JoinKey:       networkId.String(),
			}, server.NowUtc(), DefaultAbuseSettings())
			if err != nil {
				t.Fatal(err)
			}
		})
		if err := record(&AbuseActionArgs{ActionId: server.NewId(), CaseId: otherCaseId, Kind: AbuseActionKindScreenAllow, Target: "paypal-support", ReviewerId: &reviewerId}); !errors.Is(err, ErrAbuseActionInvalid) {
			t.Fatalf("an allow on a phishing case: %v", err)
		}
		if err := record(&AbuseActionArgs{ActionId: server.NewId(), CaseId: *review.CaseId, Kind: AbuseActionKindScreenAllow, Target: "paypal-support", ReviewerId: &reviewerId}); err != nil {
			t.Fatalf("the allow: %v", err)
		}
	})
}
