package session

// Owner decision 3, 2026-11-18, and 3b: a revocation check Redis does not
// answer accepts the credential and counts the acceptance; a caller that ended
// during the check is neither accepted nor counted. These tests need neither
// PostgreSQL nor Redis: the record path answers an unreachable Redis without
// SQL, and the authentication pool is the stalled-dial stand-in for the
// 2026-11-18 bursts of "redis: connection pool: failed to dial ... i/o
// timeout" (server.Testing_StallAuthRedisDials).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// How far past the check budget an unanswered check may return, for
// scheduling. Before decision 3 the same checks failed after the two second
// authentication budget.
const uncheckedAcceptanceSlack = 500 * time.Millisecond

func testingUncheckedAcceptances(path string, credential string) float64 {
	return testutil.ToFloat64(sessionCheckUnavailableCounter.WithLabelValues(path, credential))
}

// A top-level client's tagged credential: its root is itself.
func testingTaggedClientCredential() *ByJwt {
	network := NewByJwt(server.NewId(), server.NewId(), "unchecked", false, false)
	return network.Client(server.NewId(), server.NewId())
}

// A child of parent, tagged with parent's root.
func testingTaggedChildCredential(parent *ByJwt) *ByJwt {
	return parent.Client(*parent.DeviceId, server.NewId())
}

// A network sign-in credential with a session of its own.
func testingTaggedSessionCredential() *ByJwt {
	credential := NewByJwt(server.NewId(), server.NewId(), "unchecked", false, false)
	sessionId := server.NewId()
	credential.SessionId = &sessionId
	return credential
}

func TestRevocationCheckPathNamesTheCaller(t *testing.T) {
	for source, path := range map[StateQuerySource]string{
		StateQueryApiControl:      revocationCheckPathApi,
		StateQueryApiMint:         revocationCheckPathApi,
		StateQueryApiOther:        revocationCheckPathApi,
		StateQueryConnectH1:       revocationCheckPathConnectH1,
		StateQueryConnectH3:       revocationCheckPathConnectH3,
		StateQueryHostedBootstrap: revocationCheckPathHosted,
		StateQueryHostedControl:   revocationCheckPathHosted,
		StateQueryProberControl:   revocationCheckPathProber,
		StateQueryUnknown:         revocationCheckPathOther,
	} {
		if got := revocationCheckPathFromContext(WithStateQuerySource(context.Background(), source)); got != path {
			t.Errorf("source %v counts on path %q, want %q", stateQuerySourceLabels[source], got, path)
		}
	}
	lease := withRevocationCheckPath(WithStateQuerySource(context.Background(), StateQueryConnectH1), revocationCheckPathLease)
	if got := revocationCheckPathFromContext(lease); got != revocationCheckPathLease {
		t.Errorf("a lease recheck counts on path %q", got)
	}
	if got := revocationCheckPathFromContext(context.Background()); got != revocationCheckPathOther {
		t.Errorf("a caller without a source counts on path %q", got)
	}
}

// Root cause (2026-11-18, 14:18 to 17:38Z): with revocation_record_enabled
// on, a tagged credential whose record read could not dial its Redis master
// was refused with ErrAuthUnavailable, a 503, for every caller: the api
// (POST /connect/control's minute-long 503 bursts), connect admission, and
// the hosted parent authority. Each refusal waited the two second
// authentication budget first. Now every caller accepts within the check
// budget, counts the acceptance by path and credential, and asks SQL nothing.
func TestRevocationRecordUnansweredAcceptsWithinTheCheckBudget(t *testing.T) {
	defer Testing_SetRevocationRecordEnabled(true)()
	stalled, pop := server.Testing_StallAuthRedisDials(48)
	defer pop()
	parent := testingTaggedClientCredential()
	for _, test := range []struct {
		name          string
		source        StateQuerySource
		credential    *ByJwt
		parentId      *server.Id
		path          string
		label         string
		requireClient bool
	}{
		{"api client", StateQueryApiControl, testingTaggedClientCredential(), nil, revocationCheckPathApi, revocationCheckCredentialClient, false},
		{"api session", StateQueryApiOther, testingTaggedSessionCredential(), nil, revocationCheckPathApi, revocationCheckCredentialSession, false},
		{"connect h1", StateQueryConnectH1, testingTaggedClientCredential(), nil, revocationCheckPathConnectH1, revocationCheckCredentialClient, true},
		{"connect h3", StateQueryConnectH3, testingTaggedClientCredential(), nil, revocationCheckPathConnectH3, revocationCheckCredentialClient, true},
		{"hosted parent", StateQueryHostedControl, testingTaggedChildCredential(parent), parent.ClientId, revocationCheckPathHosted, revocationCheckCredentialClient, true},
		{"prober", StateQueryProberControl, testingTaggedClientCredential(), nil, revocationCheckPathProber, revocationCheckCredentialClient, true},
		{"other", StateQueryUnknown, testingTaggedClientCredential(), nil, revocationCheckPathOther, revocationCheckCredentialClient, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := WithStateQuerySource(t.Context(), test.source)
			accepted := testingUncheckedAcceptances(test.path, test.label)
			unavailable := testutil.ToFloat64(revocationRecordCheckCounter.WithLabelValues(stateQuerySourceLabels[test.source][0], stateQuerySourceLabels[test.source][1], map[bool]string{true: "client", false: "account"}[test.credential.ClientId != nil], "record_unavailable"))
			queries := testingStateQueries(test.source, test.credential.ClientId != nil)
			start := time.Now()
			var err error
			if test.parentId != nil {
				err = ValidateByJwtStateForParent(ctx, test.credential, *test.parentId)
			} else {
				err = ValidateByJwtState(ctx, test.credential, test.requireClient)
			}
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("an unanswered revocation check refused the credential: %v", err)
			}
			if elapsed < RevocationCheckBudget || RevocationCheckBudget+uncheckedAcceptanceSlack < elapsed {
				t.Errorf("the unanswered check took %s, want the %s budget", elapsed, RevocationCheckBudget)
			}
			if got := testingUncheckedAcceptances(test.path, test.label) - accepted; got != 1 {
				t.Errorf("counted %.0f unchecked acceptances on %s/%s, want 1", got, test.path, test.label)
			}
			if got := testutil.ToFloat64(revocationRecordCheckCounter.WithLabelValues(stateQuerySourceLabels[test.source][0], stateQuerySourceLabels[test.source][1], map[bool]string{true: "client", false: "account"}[test.credential.ClientId != nil], "record_unavailable")) - unavailable; got != 1 {
				t.Errorf("counted %.0f unavailable record checks, want 1", got)
			}
			if got := testingStateQueries(test.source, test.credential.ClientId != nil) - queries; got != 0 {
				t.Errorf("an unanswered record fell back to %.0f state queries", got)
			}
		})
	}
	if stalled.Dials() == 0 {
		t.Fatal("no check reached the stalled pool")
	}
}

// A caller that ended during the check gets the unavailable error, as before,
// and is not counted: nothing waited for an acceptance. Its own deadline ends
// it before the check budget does.
func TestRevocationRecordUnansweredRefusesAnEndedCaller(t *testing.T) {
	defer Testing_SetRevocationRecordEnabled(true)()
	_, pop := server.Testing_StallAuthRedisDials(48)
	defer pop()
	credential := testingTaggedClientCredential()
	accepted := testingUncheckedAcceptances(revocationCheckPathConnectH1, revocationCheckCredentialClient)
	ctx, cancel := context.WithTimeout(WithStateQuerySource(t.Context(), StateQueryConnectH1), RevocationCheckBudget/5)
	defer cancel()
	err := ValidateByJwtState(ctx, credential, true)
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("an ended caller's check answered %v", err)
	}
	if got := testingUncheckedAcceptances(revocationCheckPathConnectH1, revocationCheckCredentialClient) - accepted; got != 0 {
		t.Fatalf("counted %.0f acceptances for an ended caller", got)
	}
	canceled, cancelNow := context.WithCancel(WithStateQuerySource(t.Context(), StateQueryApiControl))
	cancelNow()
	if err := ValidateByJwtState(canceled, credential, false); !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("a canceled caller's check answered %v", err)
	}
}

// Every check that needs no Redis still refuses while Redis does not answer:
// the remembered refusals, the identity shape and the session horizon.
func TestRevocationRecordUnansweredKeepsTheChecksThatNeedNoRedis(t *testing.T) {
	defer Testing_SetRevocationRecordEnabled(true)()
	_, pop := server.Testing_StallAuthRedisDials(48)
	defer pop()
	ctx := WithStateQuerySource(t.Context(), StateQueryApiControl)

	expired := testingTaggedClientCredential()
	expired.ExpiresAt.Time = server.NowUtc().Add(-SessionGrace - time.Hour)
	if err := ValidateByJwtState(ctx, expired, false); err == nil || errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("a credential past its horizon answered %v", err)
	}

	noDevice := testingTaggedClientCredential()
	noDevice.DeviceId = nil
	if err := ValidateByJwtState(ctx, noDevice, false); err == nil || errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("a client credential without a device answered %v", err)
	}

	removed := testingTaggedClientCredential()
	key, _ := newStateRefusalKey(removed, nil)
	stateRefusals.remember(key, time.Now())
	if err := ValidateByJwtState(ctx, removed, false); !errors.Is(err, ErrByJwtInactive) {
		t.Fatalf("a remembered refusal answered %v", err)
	}
}

// The new metrics must carry none of the stats push's grouping labels: the
// push refuses the whole process's metrics when one does (server/grafana.go,
// client_golang push.go), and Mimir adds service from the grouping.
func TestUncheckedAcceptanceMetricsCarryNoPushGroupingLabel(t *testing.T) {
	grouping := []string{"env", "service", "block", "host", "instance", "job"}
	for _, collector := range []prometheus.Collector{sessionCheckUnavailableCounter, sessionMintUntaggedCounter, sessionMintUnextendedCounter} {
		descs := make(chan *prometheus.Desc, 8)
		collector.Describe(descs)
		close(descs)
		for desc := range descs {
			text := desc.String()
			for _, label := range grouping {
				if strings.Contains(text, `"`+label+`"`) {
					t.Errorf("%s declares the push grouping label %s", text, label)
				}
			}
		}
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(sessionCheckUnavailableCounter, sessionMintUntaggedCounter, sessionMintUnextendedCounter)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, family := range families {
		names[family.GetName()] = true
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				for _, grouped := range grouping {
					if label.GetName() == grouped {
						t.Errorf("%s carries the push grouping label %s", family.GetName(), grouped)
					}
				}
			}
		}
	}
	for _, name := range []string{"urnetwork_session_check_unavailable_total", "urnetwork_session_mint_untagged_total", "urnetwork_session_mint_unextended_total"} {
		if !names[name] {
			t.Errorf("%s is not exported", name)
		}
	}
	// every label value exists from the start, so a rate is zero, not absent
	if got := testutil.CollectAndCount(sessionCheckUnavailableCounter); got < len(revocationCheckPaths)*2 {
		t.Errorf("the unchecked count exports %d series, want every path and credential", got)
	}
	if got := testutil.CollectAndCount(sessionMintUntaggedCounter); got < 3 {
		t.Errorf("the untagged mint count exports %d series, want signin, code and client", got)
	}
	if got := testutil.CollectAndCount(sessionMintUnextendedCounter); got < 3 {
		t.Errorf("the unextended mint count exports %d series, want signin, code and client", got)
	}
}
