package model

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.18's v1 (ABUSE-M6-01): trusted standing with each of QD-378's conditions missing in turn and a kill ending it,
// review mode always elapsing FS-49's weekend at once, notices to every Owner and Admin of network_member, and the
// team directory's limits tightened through §21's rows. The stack cases run with sidecar_pending.

// Review mode always counts every hour open: a Web Risk kill at Friday's close is due 24 h later, Saturday, where the
// business calendar holds it to Monday's open, and a report's 4 open hours are 4 elapsed hours.
func TestAbuseReviewAlwaysElapsesTheWeekend(t *testing.T) {
	settings := DefaultAbusePolicySettings()
	always, err := NewAbuseCalendar(AbuseReviewHours{Mode: AbuseReviewModeAlways}, settings)
	if err != nil {
		t.Fatal(err)
	}
	business, err := NewAbuseCalendar(DefaultAbuseReviewHours(), settings)
	if err != nil {
		t.Fatal(err)
	}
	fridayClose := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	if due := AbuseReviewDue(AbuseSignalWebRiskHit, AbuseTierFree, AbuseStandingStandard, fridayClose, always); !due.Equal(fridayClose.Add(24 * time.Hour)) {
		t.Fatalf("always: a kill due %v", due)
	}
	mondayOpen := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	if due := AbuseReviewDue(AbuseSignalWebRiskHit, AbuseTierFree, AbuseStandingStandard, fridayClose, business); !due.Equal(mondayOpen) {
		t.Fatalf("business: a kill due %v", due)
	}
	if due := AbuseReviewDue(AbuseSignalThreatReport, AbuseTierFree, AbuseStandingStandard, fridayClose, always); !due.Equal(fridayClose.Add(4 * time.Hour)) {
		t.Fatalf("always: a threat report due %v", due)
	}
	if !always.IsOpen(time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)) {
		t.Fatal("always is closed on a Sunday night")
	}
	// abuse.yml's mode reaches the calendar the clocks read
	defer server.Testing_PushAbuseConfig("business_hours:\n  mode: always\n")()
	if hours := AbuseReviewHoursOf(server.GetAbuseConfig()); hours.Mode != AbuseReviewModeAlways {
		t.Fatalf("the file's mode read as %q", hours.Mode)
	}
	if due := AbuseReviewDue(AbuseSignalWebRiskHit, AbuseTierFree, AbuseStandingStandard, fridayClose, abuseReportCalendar(DefaultAbuseSettings())); !due.Equal(fridayClose.Add(24 * time.Hour)) {
		t.Fatalf("the clocks' calendar under the file's always: %v", due)
	}
}

// One network's trusted facts, each set unless a case leaves it out.
type testTrustedNetwork struct {
	networkId server.Id
}

// A network 120 days old, Team for the last 40 days and the next 20, with a verified domain, no payment failure and
// no action; skip names what to leave out or break.
func newTestTrustedNetwork(t testing.TB, ctx context.Context, skip string) *testTrustedNetwork {
	t.Helper()
	network := &testTrustedNetwork{networkId: server.NewId()}
	networkId := network.networkId
	Testing_CreateNetwork(ctx, networkId, "trusted-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET create_time = $2 WHERE network_id = $1`, networkId, now.Add(-120*24*time.Hour)))
		grant := func(startTime time.Time, endTime time.Time, tier string, grantKind *string) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time, start_balance_byte_count, net_revenue_nano_cents, balance_byte_count, pro, tier, grant_kind)
					VALUES ($1, $2, $3, $4, 1024, 0, 1024, true, $5, $6)
				`,
				server.NewId(), networkId, startTime, endTime, tier, grantKind,
			))
		}
		credit := string(GrantKindTeamCredit)
		switch skip {
		case "team grant":
			// the credit line alone, which carries revenue before anything is collected
			grant(now.Add(-40*24*time.Hour), now.Add(20*24*time.Hour), string(NetworkTierTeam), &credit)
		case "a gap":
			grant(now.Add(-40*24*time.Hour), now.Add(-20*24*time.Hour), string(NetworkTierTeam), nil)
			grant(now.Add(-19*24*time.Hour), now.Add(20*24*time.Hour), string(NetworkTierTeam), nil)
		case "a short grant":
			grant(now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), string(NetworkTierTeam), nil)
		case "team tier":
			grant(now.Add(-40*24*time.Hour), now.Add(20*24*time.Hour), string(NetworkTierPro), nil)
		default:
			// two periods meeting, as the job writes them
			grant(now.Add(-40*24*time.Hour), now.Add(-10*24*time.Hour), string(NetworkTierTeam), nil)
			grant(now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour), string(NetworkTierTeam), nil)
		}
		if skip != "verified domain" {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_domain (network_id, domain, challenge, verify_time) VALUES ($1, $2, 'challenge', $3)`,
				networkId, "trusted-"+strings.ReplaceAll(networkId.String()[24:], "-", "")+".example", now,
			))
		} else {
			// claimed and never verified
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_domain (network_id, domain, challenge) VALUES ($1, $2, 'challenge')`,
				networkId, "unverified-"+strings.ReplaceAll(networkId.String()[24:], "-", "")+".example",
			))
		}
		if skip == "payment" {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO stripe_invoice_payment_failure (invoice_id, network_id, customer_id, subscription_id, hosted_invoice_url, attempt_count, first_fail_time, last_fail_time)
					VALUES ($1, $2, 'customer', 'subscription', 'https://invoice.example/1', 1, $3, $3)
				`,
				"invoice-"+networkId.String(), networkId, now.Add(-24*time.Hour),
			))
		}
	})
	return network
}

// An action of kind on the network, in a case of its own, a person's or a machine's, lifted as wrong when asked.
func (self *testTrustedNetwork) act(t testing.TB, ctx context.Context, kind AbuseActionKind, liftedWrong bool, at time.Time) {
	t.Helper()
	settings := DefaultAbuseSettings()
	networkId := self.networkId
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err := OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      AbuseCategoryPhishing,
			Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: "shop.trusted.example"},
			ReporterClass: AbuseReporterClassWebRisk,
			JoinKind:      AbuseJoinKindHost,
			JoinKey:       "shop.trusted.example",
		}, at, settings)
		if err != nil {
			t.Fatal(err)
		}
		action, _, _, err := RecordAbuseActionInTx(ctx, tx, &AbuseActionArgs{ActionId: server.NewId(), CaseId: caseId, Kind: kind, Target: "shop"}, at, settings)
		if err != nil {
			t.Fatal(err)
		}
		if liftedWrong {
			reviewerId := server.NewId()
			if _, _, _, _, err := RecordAbuseLiftInTx(ctx, tx, &AbuseLiftArgs{
				ActionId: server.NewId(), CaseId: caseId, LiftedActionId: action.ActionId, ReviewState: AbuseReviewStateLiftedWrong, ReviewerId: &reviewerId,
			}, at, settings); err != nil {
				t.Fatal(err)
			}
		}
		ReevaluateNetworkAbuseStateInTx(ctx, tx, networkId, server.NowUtc(), settings)
	})
}

// The network's standing now, read from Postgres.
func (self *testTrustedNetwork) standing(t testing.TB, ctx context.Context) AbuseStanding {
	t.Helper()
	standing, found := GetNetworkAbuseStanding(ctx, self.networkId, server.NowUtc())
	if !found {
		t.Fatal("no network")
	}
	return standing.Standing
}

// trusted with every condition, and standard with each missing in turn: no Team grant but its credit line, a gap in
// the paid window, a grant younger than it, a Pro tier, a domain claimed and not verified, an unresolved payment
// failure, an action inside 90 days; an action lifted as wrong, or older than 90 days, leaves it; a kill restricts
// the network, which ends trusted; and the gate's memory answers alike.
func TestAbuseStandingTrustedNeedsEachCondition(t *testing.T) {
	requireLegalHoldTables(t)
	if !server.SidecarMg6Linked() {
		t.Skip("trusted reads MG6's tables: run with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		trusted := newTestTrustedNetwork(t, ctx, "")
		if standing := trusted.standing(t, ctx); standing != AbuseStandingTrusted {
			t.Fatalf("every condition: %s", standing)
		}
		for _, skip := range []string{"team grant", "a gap", "a short grant", "team tier", "verified domain", "payment"} {
			network := newTestTrustedNetwork(t, ctx, skip)
			if standing := network.standing(t, ctx); standing != AbuseStandingStandard {
				t.Errorf("without %s: %s", skip, standing)
			}
		}
		recent := newTestTrustedNetwork(t, ctx, "")
		recent.act(t, ctx, AbuseActionKindNoticeForward, false, server.NowUtc().Add(-30*24*time.Hour))
		if standing := recent.standing(t, ctx); standing != AbuseStandingStandard {
			t.Errorf("an action 30 days ago: %s", standing)
		}
		wrong := newTestTrustedNetwork(t, ctx, "")
		wrong.act(t, ctx, AbuseActionKindHostKill, true, server.NowUtc().Add(-30*24*time.Hour))
		if standing := wrong.standing(t, ctx); standing != AbuseStandingTrusted {
			t.Errorf("an action lifted as wrong: %s", standing)
		}
		old := newTestTrustedNetwork(t, ctx, "")
		old.act(t, ctx, AbuseActionKindNoticeForward, false, server.NowUtc().Add(-100*24*time.Hour))
		if standing := old.standing(t, ctx); standing != AbuseStandingTrusted {
			t.Errorf("an action 100 days ago: %s", standing)
		}

		// a kill now restricts the network, which comes before trusted
		trusted.act(t, ctx, AbuseActionKindHostKill, false, server.NowUtc())
		if standing := trusted.standing(t, ctx); standing != AbuseStandingRestricted {
			t.Fatalf("after a kill: %s", standing)
		}

		// the gate's load carries the same facts into its memory
		var networkIdFacts map[server.Id]*networkAbuseFacts
		server.Db(ctx, func(conn server.PgConn) {
			networkIdFacts = loadNetworkAbuseFactsWithConn(ctx, conn, []server.Id{old.networkId, recent.networkId}, server.NowUtc(), true)
		})
		if !networkIdFacts[old.networkId].trusted || networkIdFacts[recent.networkId].trusted {
			t.Fatalf("the loaded facts: %+v %+v", networkIdFacts[old.networkId], networkIdFacts[recent.networkId])
		}
		entry := &abuseGateEntry{found: true, trusted: true}
		if standing := entry.standing(server.NowUtc()); standing != AbuseStandingTrusted {
			t.Fatalf("the gate's entry: %s", standing)
		}
		entry.probationEnd = server.NowUtc().Add(time.Hour)
		if standing := entry.standing(server.NowUtc()); standing != AbuseStandingProbation {
			t.Fatalf("the gate's entry on probation: %s", standing)
		}
	})
}

// An action's notice reaches every Owner and Admin of network_member who is not pending, once each, besides the admin
// of record: not an Auditor, not a pending Admin, and not a member with no email.
func TestAbuseNoticeReachesEveryOwnerAndAdmin(t *testing.T) {
	if !server.SidecarMg6Linked() {
		t.Skip("network_member is MG6's: run with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		adminUserId := server.NewId()
		adminAuth := Testing_CreateNetwork(ctx, networkId, "notice-members-network", adminUserId)
		member := func(role NetworkMemberRole, pending bool, userAuth string) {
			userId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_user (user_id, user_name, auth_type, user_auth) VALUES ($1, 'member', $2, $3)`,
					userId, AuthTypePassword, userAuth,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_member (member_id, network_id, user_id, role, pending) VALUES ($1, $2, $3, $4, $5)`,
					server.NewId(), networkId, userId, string(role), pending,
				))
			})
		}
		member(NetworkMemberRoleAdmin, false, "first-admin@notice.example")
		member(NetworkMemberRoleAdmin, false, "second-admin@notice.example")
		member(NetworkMemberRoleOwner, false, "owner@notice.example")
		member(NetworkMemberRoleAuditor, false, "auditor@notice.example")
		member(NetworkMemberRoleAdmin, true, "pending-admin@notice.example")
		member(NetworkMemberRoleAdmin, false, "+15555550100")
		var recipients []string
		server.Tx(ctx, func(tx server.PgTx) {
			recipients = AbuseNoticeRecipientsInTx(ctx, tx, networkId)
		})
		want := []string{adminAuth, "first-admin@notice.example", "owner@notice.example", "second-admin@notice.example"}
		slices.Sort(want)
		if !slices.Equal(recipients, want) {
			t.Fatalf("recipients %v, want %v", recipients, want)
		}
	})
}

// The team directory's request limits are §21's to tighten from M6: a row citing the case reaches the network's
// limits, as GET /sidecar/limits gives them, and a lift clears it.
func TestAbuseTightensTheDirectoryLimits(t *testing.T) {
	requireLegalHoldTables(t)
	if !server.SidecarMg6Linked() {
		t.Skip("the directory keys wait for MG6: run with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{
			"directory_requests_pending": 50,
			"directory_requests_per_day": 100,
			"directory_requests_per_run": 10,
			"directory_target_pending":   5,
		}))
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "directory-limit-network", server.NewId())
		caseId := server.NewId()
		for _, limitKey := range abuseDirectoryLimitKeys {
			if err := ValidateAbuseLimitTighten(limitKey, 1, caseId); err != nil {
				t.Errorf("%s: %v", limitKey, err)
			}
		}
		target := AbuseLimitTightenTarget("directory_requests_per_day", 3)
		if err := SetAbuseLimitRow(ctx, networkId, caseId, target); err != nil {
			t.Fatal(err)
		}
		if limit := NetworkSidecarLimits(ctx, networkId).Limit("directory_requests_per_day"); limit != 3 {
			t.Fatalf("the tightened limit reads %d", limit)
		}
		if limit := NetworkSidecarLimits(ctx, networkId).Limits()["directory_requests_per_day"]; limit != 3 {
			t.Fatalf("GET /sidecar/limits' body reads %d", limit)
		}
		if cleared, err := ClearAbuseLimitRow(ctx, networkId, caseId, target); err != nil || !cleared {
			t.Fatalf("the lift: %t %v", cleared, err)
		}
	})
}
