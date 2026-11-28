package session

// The hosted path's check budget (HostedRevocationCheckBudget). These tests
// need neither PostgreSQL nor Redis: the record read is a stand-in that
// answers late, and a credential the record answers asks no SQL.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
)

// The record fields a valid credential reads, in the order the check asks
// for them: the network's user and credential cutoff, no deletion, an active
// client of the credential's device (owned by parentClientId when the caller
// names one), an active root, and an unrevoked session.
func testingValidRevocationRecord(byJwt *ByJwt, parentClientId *server.Id, fields []string) []any {
	values := make([]any, len(fields))
	for i, field := range fields {
		switch {
		case field == revocationFieldUser:
			values[i] = byJwt.UserId.String()
		case field == revocationFieldCredential:
			values[i] = "0"
		case field == revocationFieldDeleted:
			values[i] = nil
		case strings.HasPrefix(field, "c:"):
			source := revocationAbsent
			if parentClientId != nil {
				source = parentClientId.String()
			}
			values[i] = fmt.Sprintf("%s %s %s", revocationActive, byJwt.DeviceId, source)
		case strings.HasPrefix(field, "r:"), strings.HasPrefix(field, "s:"):
			values[i] = revocationActive
		}
	}
	return values
}

// Root cause (2026-11-21): the proxy hosts ran their CPUs at 99.6-99.8%, each
// proxy process collecting garbage 4 to 12 times a minute at its memory
// limit, and 5-15% of the hosted authority's record reads took longer than
// RevocationCheckBudget inside the process while Redis answered in
// microseconds. Each such credential was accepted unchecked: about 9% of the
// hosted checks, and the SessionChecksUnchecked alert held for hours. A hosted
// read that answers after 250ms but within two seconds is now checked, and the
// record decides; every other path keeps RevocationCheckBudget.
//
// The record stand-in answers only a read whose budget leaves it 400ms, and
// otherwise waits out the read's deadline: no timer decides the outcome.
func TestHostedRevocationRecordReadThatAnswersLateIsChecked(t *testing.T) {
	defer Testing_SetRevocationRecordEnabled(true)()
	const answerAfter = 400 * time.Millisecond
	if answerAfter <= RevocationCheckBudget || HostedRevocationCheckBudget <= answerAfter {
		t.Fatalf("the stand-in answers after %s, which is not between the budgets %s and %s", answerAfter, RevocationCheckBudget, HostedRevocationCheckBudget)
	}
	read := readRevocationRecord
	defer func() { readRevocationRecord = read }()

	parent := testingTaggedClientCredential()
	for _, test := range []struct {
		name       string
		source     StateQuerySource
		credential *ByJwt
		parentId   *server.Id
		path       string
		checked    bool
	}{
		{"hosted control", StateQueryHostedControl, testingTaggedChildCredential(parent), parent.ClientId, revocationCheckPathHosted, true},
		{"hosted discovery", StateQueryHostedDiscovery, testingTaggedChildCredential(parent), parent.ClientId, revocationCheckPathHosted, true},
		{"hosted retire", StateQueryHostedRetire, parent, nil, revocationCheckPathHosted, true},
		{"hosted bootstrap", StateQueryHostedBootstrap, parent, nil, revocationCheckPathHosted, true},
		{"api control", StateQueryApiControl, testingTaggedClientCredential(), nil, revocationCheckPathApi, false},
		{"connect h1", StateQueryConnectH1, testingTaggedClientCredential(), nil, revocationCheckPathConnectH1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			readRevocationRecord = func(ctx context.Context, key string, fields []string) ([]any, error) {
				if deadline, ok := ctx.Deadline(); ok && answerAfter <= time.Until(deadline) {
					return testingValidRevocationRecord(test.credential, test.parentId, fields), nil
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			ctx := WithStateQuerySource(t.Context(), test.source)
			labels := stateQuerySourceLabels[test.source]
			valid := testutil.ToFloat64(revocationRecordCheckCounter.WithLabelValues(labels[0], labels[1], "client", "record_valid"))
			unavailable := testutil.ToFloat64(revocationRecordCheckCounter.WithLabelValues(labels[0], labels[1], "client", "record_unavailable"))
			accepted := testingUncheckedAcceptances(test.path, revocationCheckCredentialClient)
			queries := testingStateQueries(test.source, true)

			var err error
			if test.parentId != nil {
				err = ValidateByJwtStateForParent(ctx, test.credential, *test.parentId)
			} else {
				err = ValidateByJwtState(ctx, test.credential, true)
			}
			if err != nil {
				t.Fatalf("the check refused a valid credential: %v", err)
			}

			gotValid := testutil.ToFloat64(revocationRecordCheckCounter.WithLabelValues(labels[0], labels[1], "client", "record_valid")) - valid
			gotUnavailable := testutil.ToFloat64(revocationRecordCheckCounter.WithLabelValues(labels[0], labels[1], "client", "record_unavailable")) - unavailable
			gotAccepted := testingUncheckedAcceptances(test.path, revocationCheckCredentialClient) - accepted
			if test.checked {
				if gotValid != 1 || gotUnavailable != 0 || gotAccepted != 0 {
					t.Errorf("a %s read answered after %s: %.0f checked, %.0f unavailable, %.0f accepted unchecked; want it checked within the hosted budget %s", test.name, answerAfter, gotValid, gotUnavailable, gotAccepted, HostedRevocationCheckBudget)
				}
			} else if gotValid != 0 || gotUnavailable != 1 || gotAccepted != 1 {
				t.Errorf("a %s read answered after %s: %.0f checked, %.0f unavailable, %.0f accepted unchecked; want it accepted unchecked at %s", test.name, answerAfter, gotValid, gotUnavailable, gotAccepted, RevocationCheckBudget)
			}
			if got := testingStateQueries(test.source, true) - queries; got != 0 {
				t.Errorf("the check asked SQL %.0f times", got)
			}
		})
	}
}

// Only the hosted path outside a mint waits the hosted budget. A hosted mint
// holds one of its owner's registration slots, and a check inside a
// transaction waits the transaction's Redis budget, which only the hosted
// proxy's own session mint lengthens (MintHostedSession).
func TestRevocationCheckBudgetIsLongerOnlyOnTheHostedPath(t *testing.T) {
	for source := range stateQuerySourceCount {
		want := RevocationCheckBudget
		if stateQuerySourceLabels[source][0] == "hosted" && source != StateQueryHostedMint {
			want = HostedRevocationCheckBudget
		}
		ctx := WithStateQuerySource(context.Background(), source)
		if got := revocationCheckBudget(ctx); got != want {
			t.Errorf("a %s/%s check waits %s, want %s", stateQuerySourceLabels[source][0], stateQuerySourceLabels[source][1], got, want)
		}
		if got := SessionMintRedisBudgetFor(ctx); got != SessionMintRedisBudget {
			t.Errorf("a %s/%s transaction waits %s for each Redis step, want %s", stateQuerySourceLabels[source][0], stateQuerySourceLabels[source][1], got, SessionMintRedisBudget)
		}
	}
	for _, source := range []StateQuerySource{StateQueryHostedControl, StateQueryHostedBootstrap, StateQueryHostedRetire} {
		if got := revocationCheckBudget(WithStateQuerySource(context.Background(), source)); got != HostedRevocationCheckBudget {
			t.Errorf("a hosted %s check waits %s, want %s", stateQuerySourceLabels[source][1], got, HostedRevocationCheckBudget)
		}
	}
	lease := withRevocationCheckPath(WithStateQuerySource(context.Background(), StateQueryHostedControl), revocationCheckPathLease)
	if got := revocationCheckBudget(lease); got != RevocationCheckBudget {
		t.Errorf("a lease recheck waits %s, want %s", got, RevocationCheckBudget)
	}
	if got := revocationCheckBudget(context.Background()); got != RevocationCheckBudget {
		t.Errorf("a caller without a source waits %s, want %s", got, RevocationCheckBudget)
	}
	hostedSession := withSessionMintRedisBudget(WithStateQuerySource(context.Background(), StateQueryHostedMint), hostedSessionMintRedisBudget)
	if got := SessionMintRedisBudgetFor(hostedSession); got != HostedRevocationCheckBudget {
		t.Errorf("the hosted proxy's own session mint waits %s for each Redis step, want %s", got, HostedRevocationCheckBudget)
	}
	if RevocationCheckBudget != 250*time.Millisecond || SessionMintRedisBudget != RevocationCheckBudget || HostedRevocationCheckBudget != 2*time.Second {
		t.Errorf("budgets %s, mint %s, hosted %s; want 250ms, 250ms and 2s", RevocationCheckBudget, SessionMintRedisBudget, HostedRevocationCheckBudget)
	}
}
