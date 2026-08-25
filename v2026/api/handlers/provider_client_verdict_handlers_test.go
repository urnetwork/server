package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
)

// validClientVerdictBody is a well-formed egress-dead report: the client sent
// and was acknowledged, and nothing came back. Tests mutate a copy to exercise
// one rule at a time.
//
// Note what is NOT in it: reporter_network_id. The reporter is the session.
func validClientVerdictBody(exitClientId server.Id) map[string]any {
	return map[string]any{
		"exit_client_id":    exitClientId.String(),
		"reason":            model.ProviderClientVerdictReasonNoReceiveAck,
		"send_ack_count":    64,
		"send_ack_bytes":    8192,
		"receive_ack_count": 0,
		"receive_ack_bytes": 0,
		"syn_sent":          3,
		"syn_received":      0,
		"window_seconds":    30,
	}
}

// verdictReporter is one reporting session. A signed token is not a session on
// its own: the endpoint validates the token against live rows, so a reporter
// exists as a network and its admin user before it can report anything.
type verdictReporter struct {
	networkId server.Id
	userId    server.Id
}

// newVerdictReporter creates the network behind one reporting session.
func newVerdictReporter(ctx context.Context) verdictReporter {
	reporter := verdictReporter{
		networkId: server.NewId(),
		userId:    server.NewId(),
	}
	model.Testing_CreateNetwork(
		ctx,
		reporter.networkId,
		fmt.Sprintf("verdict-%s", reporter.networkId),
		reporter.userId,
	)
	return reporter
}

// postClientVerdict posts as reporter, or unauthenticated when reporter is the
// zero value.
func postClientVerdict(
	t testing.TB,
	reporter verdictReporter,
	body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %s", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/network/provider-verdict", bytes.NewReader(buf))
	if reporter.networkId != (server.Id{}) {
		byJwt := jwt.NewByJwt(reporter.networkId, reporter.userId, "test", false, false)
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", byJwt.Sign()))
	}
	w := httptest.NewRecorder()
	ProviderClientVerdictSubmit(w, req)
	return w
}

// Fails closed. Without a session there is no reporter network, and a verdict
// with no reporter is a verdict that cannot be counted or capped.
func TestProviderClientVerdictRejectsUnauthenticated(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		exitClientId := server.NewId()

		w := postClientVerdict(t, verdictReporter{}, validClientVerdictBody(exitClientId))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 for an unauthenticated report", w.Code)
		}

		verdicts := model.GetProviderClientVerdictsInWindow(
			context.Background(),
			exitClientId,
			server.NowUtc(),
		)
		if len(verdicts) != 0 {
			t.Fatalf("stored %d verdicts from an unauthenticated request, want 0", len(verdicts))
		}
	})
}

// THE REPORTER IS THE SESSION. Two sessions posting a byte-identical body must
// store two different reporter networks -- and a body that tries to name its
// own reporter never gets in at all.
//
// The second half is a deviation worth being explicit about: because the args
// type decodes strictly, a body carrying reporter_network_id is REJECTED with a
// 400 rather than silently ignored. Either way it can never be honoured, and a
// loud rejection beats a client that believes it is choosing its own reporter
// id and is quietly overruled.
func TestProviderClientVerdictReporterComesFromTheSession(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		exitClientId := server.NewId()
		reporterA := newVerdictReporter(ctx)
		reporterB := newVerdictReporter(ctx)
		liar := server.NewId()

		if w := postClientVerdict(t, reporterA, validClientVerdictBody(exitClientId)); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w := postClientVerdict(t, reporterB, validClientVerdictBody(exitClientId)); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}

		// a body naming another network, posted by reporterA
		lying := validClientVerdictBody(exitClientId)
		lying["reporter_network_id"] = liar.String()
		if w := postClientVerdict(t, reporterA, lying); w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for a body naming its own reporter_network_id", w.Code)
		}

		verdicts := model.GetProviderClientVerdictsInWindow(ctx, exitClientId, server.NowUtc())
		reporters := []server.Id{}
		for _, verdict := range verdicts {
			reporters = append(reporters, verdict.ReporterNetworkId)
		}
		if len(reporters) != 2 {
			t.Fatalf("stored %d verdicts, want 2 (the lying body must not have been stored)", len(reporters))
		}
		if !slices.Contains(reporters, reporterA.networkId) ||
			!slices.Contains(reporters, reporterB.networkId) {
			t.Fatalf("reporters = %v, want exactly the two session networks %s and %s",
				reporters, reporterA.networkId, reporterB.networkId)
		}
		if slices.Contains(reporters, liar) {
			t.Fatalf("reporters = %v: the body's reporter_network_id was honoured", reporters)
		}
	})
}

func TestProviderClientVerdictRejectsBadBodies(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		reporter := newVerdictReporter(context.Background())

		cases := []struct {
			name   string
			mutate func(body map[string]any)
		}{
			{
				// an open reason column fills with whatever a client build
				// happens to send, and then nothing can be counted by it
				name: "unknown reason",
				mutate: func(body map[string]any) {
					body["reason"] = "slow"
				},
			},
			{
				name: "empty reason",
				mutate: func(body map[string]any) {
					body["reason"] = ""
				},
			},
			{
				// a misspelled count decodes to zero, and zero receive acks is
				// exactly the value that means "egress dead" -- so a typo would
				// not be a malformed report, it would be a counting one
				name: "unknown field",
				mutate: func(body map[string]any) {
					body["recieve_ack_count"] = 0
				},
			},
			{
				name: "negative receive ack count",
				mutate: func(body map[string]any) {
					body["receive_ack_count"] = -1
				},
			},
			{
				name: "negative send ack bytes",
				mutate: func(body map[string]any) {
					body["send_ack_bytes"] = -8192
				},
			},
			{
				name: "negative syn received",
				mutate: func(body map[string]any) {
					body["syn_received"] = -1
				},
			},
			{
				name: "negative window",
				mutate: func(body map[string]any) {
					body["window_seconds"] = -30
				},
			},
			{
				name: "missing exit client id",
				mutate: func(body map[string]any) {
					delete(body, "exit_client_id")
				},
			},
		}

		for _, testCase := range cases {
			exitClientId := server.NewId()
			body := validClientVerdictBody(exitClientId)
			testCase.mutate(body)

			w := postClientVerdict(t, reporter, body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400: %s", testCase.name, w.Code, w.Body.String())
			}

			// 400 BEFORE any store, never store-then-flag: a row that is in
			// this table is a row that counts
			verdicts := model.GetProviderClientVerdictsInWindow(
				context.Background(),
				exitClientId,
				server.NowUtc(),
			)
			if len(verdicts) != 0 {
				t.Errorf("%s: stored %d verdicts on a rejected body, want 0", testCase.name, len(verdicts))
			}
		}
	})
}

func TestProviderClientVerdictStoresValidReport(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		exitClientId := server.NewId()
		reporter := newVerdictReporter(ctx)

		w := postClientVerdict(t, reporter, validClientVerdictBody(exitClientId))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}

		verdicts := model.GetProviderClientVerdictsInWindow(ctx, exitClientId, server.NowUtc())
		if len(verdicts) != 1 {
			t.Fatalf("stored %d verdicts, want 1", len(verdicts))
		}
		verdict := verdicts[0]
		if verdict.ReporterNetworkId != reporter.networkId {
			t.Fatalf(
				"reporter = %s, want the session network %s",
				verdict.ReporterNetworkId,
				reporter.networkId,
			)
		}
		if verdict.Reason != model.ProviderClientVerdictReasonNoReceiveAck {
			t.Fatalf("reason = %q", verdict.Reason)
		}
		if verdict.SendAckCount != 64 || verdict.SendAckBytes != 8192 {
			t.Fatalf("send acks = %d/%dB", verdict.SendAckCount, verdict.SendAckBytes)
		}
		if verdict.ReceiveAckCount != 0 || verdict.ReceiveAckBytes != 0 {
			t.Fatalf("receive acks = %d/%dB", verdict.ReceiveAckCount, verdict.ReceiveAckBytes)
		}
		if verdict.WindowSeconds != 30 {
			t.Fatalf("window seconds = %d", verdict.WindowSeconds)
		}
	})
}

// testing_connectProbeableProvider is the api/handlers copy of the model test
// fixture: a connected, valid provider holding a Public provide key, which is
// what GetProviderEgressLocationDue requires before it will offer a provider.
func testing_connectProbeableProvider(
	t testing.TB,
	ctx context.Context,
	clientId server.Id,
	locationId server.Id,
	clientAddress string,
) {
	t.Helper()
	model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")

	handlerId := model.CreateNetworkClientHandler(ctx)
	connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
	if err != nil {
		t.Fatalf("connect client: %s", err)
	}
	if err := model.SetConnectionLocation(ctx, connectionId, locationId, &model.ConnectionLocationScores{}); err != nil {
		t.Fatalf("set connection location: %s", err)
	}
	model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
		model.ProvideModePublic: []byte("provide-secret"),
	})
}

// End to end: three distinct networks report a provider egress-dead and the
// prober is offered that provider on its next poll -- and nothing else moves.
//
// The due cutoff here is the ENDPOINT's own arithmetic (providerEgressDueAge),
// not a hand-picked one. model cannot import that constant, so this is the test
// that fails if the reprioritise target and the due cutoff ever drift apart,
// instead of the feature silently doing nothing.
func TestProviderClientVerdictQuorumMakesTheProviderDue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		city := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		model.CreateLocation(ctx, city)

		reported := server.NewId()
		quiet := server.NewId()
		testing_connectProbeableProvider(t, ctx, reported, city.LocationId, "0.0.0.1:0")
		testing_connectProbeableProvider(t, ctx, quiet, city.LocationId, "0.0.0.2:0")
		model.UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		// both probed an hour ago: neither is due, and neither has a probe
		// attempt row, so nothing is deferred by the attempt backoff either
		for _, clientId := range []server.Id{reported, quiet} {
			model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
				ClientId:    clientId,
				LocationId:  city.LocationId,
				CountryCode: "us",
				ObservedAt:  now.Add(-time.Hour),
				Verdict:     "verified",
			})
		}

		due := func() []server.Id {
			return model.GetProviderEgressLocationDue(
				ctx,
				server.NowUtc().Add(-providerEgressDueAge),
				server.NowUtc().Add(-model.ProviderEgressProbeAttemptBackoff),
				100,
			)
		}
		if slices.Contains(due(), reported) {
			t.Fatal("the provider was already due before any verdict")
		}

		// two reporters are not a quorum
		for range 2 {
			w := postClientVerdict(t, newVerdictReporter(ctx), validClientVerdictBody(reported))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
		}
		if slices.Contains(due(), reported) {
			t.Fatal("two reporters made the provider due, want three")
		}

		w := postClientVerdict(t, newVerdictReporter(ctx), validClientVerdictBody(reported))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}

		if !slices.Contains(due(), reported) {
			t.Fatal("a met quorum did not make the provider due for probing")
		}
		// the effect is scoped to the reported provider
		if slices.Contains(due(), quiet) {
			t.Fatal("an unreported provider became due")
		}

		// AND NOTHING IN THE SELECTION PATH MOVED. The location still resolves,
		// with the same location id, country and verdict -- a met quorum
		// schedules a probe and does not demote, exclude or rescore anything.
		// (Nothing in selection reads observed_at at all; the freshness lookup
		// below is the closest thing to it that exists.)
		fresh := model.GetFreshProviderEgressLocation(ctx, reported, model.ProviderEgressLocationMaxAge)
		if fresh == nil {
			t.Fatal("the reported provider's location stopped being fresh")
		}
		if fresh.LocationId != city.LocationId {
			t.Fatalf("location id = %s, want %s", fresh.LocationId, city.LocationId)
		}
		if fresh.CountryCode != "us" {
			t.Fatalf("country code = %q, want us", fresh.CountryCode)
		}
		if fresh.Verdict != "verified" {
			t.Fatalf("verdict = %q, want verified: a client verdict must not touch the probe verdict", fresh.Verdict)
		}
	})
}

// The quorum brings the next probe forward; it does not override the attempt
// backoff. A provider the prober tried minutes ago stays deferred, so the most
// a quorum can buy -- honest or manufactured -- is one probe per provider per
// ProviderEgressProbeAttemptBackoff. That bound is what keeps the cost of
// griefing at one probe, and it is why the test above deliberately has no
// attempt row.
func TestProviderClientVerdictQuorumDoesNotOverrideTheAttemptBackoff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		city := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		model.CreateLocation(ctx, city)

		clientId := server.NewId()
		testing_connectProbeableProvider(t, ctx, clientId, city.LocationId, "0.0.0.3:0")
		model.UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		model.SetProviderEgressLocation(ctx, &model.ProviderEgressLocation{
			ClientId:    clientId,
			LocationId:  city.LocationId,
			CountryCode: "us",
			ObservedAt:  now.Add(-time.Hour),
		})
		model.SetProviderEgressProbeAttempt(ctx, &model.ProviderEgressProbeAttempt{
			ClientId:  clientId,
			AttemptAt: now.Add(-5 * time.Minute),
		})

		for range model.ProviderClientVerdictQuorum {
			w := postClientVerdict(t, newVerdictReporter(ctx), validClientVerdictBody(clientId))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
		}

		due := model.GetProviderEgressLocationDue(
			ctx,
			server.NowUtc().Add(-providerEgressDueAge),
			server.NowUtc().Add(-model.ProviderEgressProbeAttemptBackoff),
			100,
		)
		if slices.Contains(due, clientId) {
			t.Fatal("a met quorum overrode the probe attempt backoff")
		}
	})
}
