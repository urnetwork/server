// A hosted device's control pack of only PeerAudit frames is answered empty
// before contract admission and before its credential is read
// (controller/connect_control_discard.go), while a closed authority or an
// ended request still reports its lifecycle. The authority is synthetic and
// no database is needed: a call that reached authentication would fail its
// local preflight with a 401.
package localclient

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func authorityDiscardTestAudit(abuse bool) *protocol.PeerAudit {
	return &protocol.PeerAudit{
		PeerId:        server.NewId().Bytes(),
		StreamId:      server.NewId().Bytes(),
		Duration:      60,
		Abuse:         abuse,
		SendByteCount: 64 * 1024,
		SendCount:     48,
	}
}

// The discard count of one abuse and bearer label pair.
func authorityDiscardTestCount(t testing.TB, abuse string, bearer string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_connect_control_discarded_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["abuse"] == abuse && labels["bearer"] == bearer {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// No credential, one that does not verify and the authority's own expired
// token each get the empty result, with the contract creation bound full,
// and no admission refusal is counted. A pack with a frame that acts, or with
// two audits, is authenticated, and its credential fails the preflight.
func TestAuthorityDiscardsAuditBeforeAuthenticate(t *testing.T) {
	defer session.Testing_SetRejectExpired(true)()
	admission := NewContractAdmission(1)
	defer admission.Close()
	authority := contractAdmissionTestAuthority(admission)
	expired := session.NewByJwt(authority.networkId, authority.userId, "synthetic", false, false).Client(authority.deviceId, authority.clientId)
	expired.ExpiresAt = gojwt.NewNumericDate(time.Now().Add(-time.Hour))
	// one audit a request, as every connect version sends
	audits := contractAdmissionTestArgs(t, authorityDiscardTestAudit(true))

	if !admission.tryAcquire() {
		t.Fatal("a fresh bound refused its first slot")
	}
	defer admission.release()
	refusals := testutil.ToFloat64(contractAdmissionRefusals)
	for _, token := range []string{"", "synthetic-unverifiable-token", expired.Testing_Sign()} {
		// labeled by whether a token was presented, unverified
		bearer := "present"
		if token == "" {
			bearer = "absent"
		}
		labeledBefore := authorityDiscardTestCount(t, "claimed", bearer)
		result, err := authority.ConnectControl(t.Context(), token, audits)
		if err != nil || result == nil || result.Pack != "" || result.Error != nil {
			t.Fatalf("an audit-only pack = %+v, %#v; want the empty result", result, err)
		}
		if labeled := authorityDiscardTestCount(t, "claimed", bearer) - labeledBefore; labeled != 1 {
			t.Fatalf("counted %v discards with abuse=claimed bearer=%s, want 1", labeled, bearer)
		}
	}
	if counted := testutil.ToFloat64(contractAdmissionRefusals) - refusals; counted != 0 {
		t.Fatalf("audit-only packs counted %v admission refusals", counted)
	}

	mixed := contractAdmissionTestArgs(t, authorityDiscardTestAudit(false), &protocol.CloseContract{ContractId: server.NewId().Bytes()})
	_, err := authority.ConnectControl(t.Context(), "", mixed)
	contractAdmissionExpectStatus(t, err, http.StatusUnauthorized, "a pack with a close")
	creates := contractAdmissionTestArgs(t, authorityDiscardTestAudit(false), contractAdmissionTestCreate())
	_, err = authority.ConnectControl(t.Context(), "", creates)
	contractAdmissionExpectStatus(t, err, http.StatusUnauthorized, "a pack with a creation and an audit")
	// more than one audit is not a shape any connect version sends
	twoAudits := contractAdmissionTestArgs(t, authorityDiscardTestAudit(false), authorityDiscardTestAudit(true))
	_, err = authority.ConnectControl(t.Context(), "", twoAudits)
	contractAdmissionExpectStatus(t, err, http.StatusUnauthorized, "a pack of two audits")
}

// A closed authority, and a request whose context has ended, report their
// lifecycle for an audit-only pack too, and count no discard.
func TestAuthorityDiscardStillReportsLifecycle(t *testing.T) {
	audits := contractAdmissionTestArgs(t, authorityDiscardTestAudit(false))
	discards := func() float64 {
		families, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			t.Fatal(err)
		}
		total := float64(0)
		for _, family := range families {
			if family.GetName() != "urnetwork_connect_control_discarded_requests_total" {
				continue
			}
			for _, metric := range family.GetMetric() {
				total += metric.GetCounter().GetValue()
			}
		}
		return total
	}
	before := discards()

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := contractAdmissionTestAuthority(nil).ConnectControl(ended, "", audits); !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended request answered %v, want its cancellation", err)
	}
	closed := contractAdmissionTestAuthority(nil)
	closed.closed.Store(true)
	if _, err := closed.ConnectControl(t.Context(), "", audits); !errors.Is(err, context.Canceled) {
		t.Fatalf("a closed authority answered %v, want context.Canceled", err)
	}
	if counted := discards() - before; counted != 0 {
		t.Fatalf("refused requests counted %v discards", counted)
	}
}
