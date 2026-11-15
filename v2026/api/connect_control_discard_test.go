package api

// POST /connect/control answers a pack of only PeerAudit frames empty before
// it reads the credential (controller/connect_control_discard.go), behind the
// route's intended network and lag.dev guest gates. Pure, as the other route
// table tests: tokens are signed with the local vault key, and the counters
// show that a discarded request attempts no authentication, asks the
// per-client limit for no decision and reads no revocation record.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// An audit as connect builds one, with synthetic ids and counts.
func connectControlDiscardAudit(abuse bool) *protocol.PeerAudit {
	return &protocol.PeerAudit{
		PeerId:        server.NewId().Bytes(),
		StreamId:      server.NewId().Bytes(),
		Duration:      60,
		Abuse:         abuse,
		SendByteCount: 64 * 1024,
		SendCount:     48,
	}
}

// A control request body with one frame per message.
func connectControlDiscardBody(t testing.TB, messages ...proto.Message) []byte {
	t.Helper()
	frames := make([]*protocol.Frame, 0, len(messages))
	for _, message := range messages {
		frame, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	packBytes, err := proto.Marshal(&protocol.Pack{Frames: frames})
	for _, frame := range frames {
		connect.MessagePoolReturn(frame.MessageBytes)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(&controller.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(packBytes)})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func connectControlDiscardServe(handler http.Handler, body []byte, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/connect/control", bytes.NewReader(body))
	r.RemoteAddr = "198.51.100.42:4000"
	r.Header.Set("Content-Type", "application/json")
	for name, value := range header {
		r.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// The sum of every child of a counter family.
func connectControlDiscardFamilyTotal(t testing.TB, name string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := float64(0)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			total += metric.GetCounter().GetValue()
		}
	}
	return total
}

// The discard count of one abuse and bearer label pair.
func connectControlDiscardLabeledCount(t testing.TB, abuse string, bearer string) float64 {
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

// The families a request that reads a credential always moves, and the
// discard count.
var connectControlDiscardFamilies = []string{
	"urnetwork_session_auth_attempts_total",
	"urnetwork_connect_control_client_limit_total",
	"urnetwork_jwt_state_record_checks_total",
	"urnetwork_connect_control_discarded_requests_total",
}

func connectControlDiscardTotals(t testing.TB) map[string]float64 {
	totals := map[string]float64{}
	for _, name := range connectControlDiscardFamilies {
		totals[name] = connectControlDiscardFamilyTotal(t, name)
	}
	return totals
}

// No credential, one that does not verify, and a valid client token are each
// answered the empty pack, and none of them attempts authentication, asks the
// per-client limit or reads a revocation record. Each is counted with
// bearer=absent or bearer=present by the header alone. The same requests
// with a frame that acts authenticate, and without a valid credential are a
// 401.
func TestConnectControlRouteDiscardsAuditWithoutCredential(t *testing.T) {
	handler := router.NewRouter(t.Context(), Routes())
	clientToken := session.NewByJwt(server.NewId(), server.NewId(), "synthetic", false, false).Client(server.NewId(), server.NewId()).Testing_Sign()
	// one audit a request, as every connect version sends
	audit := connectControlDiscardBody(t, connectControlDiscardAudit(true))
	authorizations := []string{"", "Bearer synthetic-unverifiable-token", "Bearer " + clientToken}

	before := connectControlDiscardTotals(t)
	for i, authorization := range authorizations {
		// labeled by whether a bearer credential was presented, unverified
		bearer := "present"
		if i == 0 {
			bearer = "absent"
		}
		labeledBefore := connectControlDiscardLabeledCount(t, "claimed", bearer)
		w := connectControlDiscardServe(handler, audit, map[string]string{"Authorization": authorization})
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"pack":"","error":null}` {
			t.Fatalf("authorization %.20q: %d %s, want the empty pack", authorization, w.Code, w.Body.String())
		}
		if labeled := connectControlDiscardLabeledCount(t, "claimed", bearer) - labeledBefore; labeled != 1 {
			t.Fatalf("authorization %.20q counted %v discards with abuse=claimed bearer=%s, want 1", authorization, labeled, bearer)
		}
	}
	after := connectControlDiscardTotals(t)
	for _, name := range connectControlDiscardFamilies[:3] {
		if after[name] != before[name] {
			t.Fatalf("discarded requests moved %s by %v", name, after[name]-before[name])
		}
	}
	if discarded := after["urnetwork_connect_control_discarded_requests_total"] - before["urnetwork_connect_control_discarded_requests_total"]; discarded != float64(len(authorizations)) {
		t.Fatalf("%v discards counted, want %d", discarded, len(authorizations))
	}

	acts := connectControlDiscardBody(t, connectControlDiscardAudit(false), &protocol.CreateContract{DestinationId: server.NewId().Bytes(), TransferByteCount: 1024 * 1024})
	for _, authorization := range authorizations[:2] {
		attemptsBefore := connectControlDiscardFamilyTotal(t, "urnetwork_session_auth_attempts_total")
		w := connectControlDiscardServe(handler, acts, map[string]string{"Authorization": authorization})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("authorization %.20q with a contract frame: %d %s, want 401", authorization, w.Code, w.Body.String())
		}
		if attempts := connectControlDiscardFamilyTotal(t, "urnetwork_session_auth_attempts_total") - attemptsBefore; attempts != 1 {
			t.Fatalf("a request with a contract frame attempted authentication %v times, want 1", attempts)
		}
	}
}

// The route's gates still run before the discard: a lag.dev guest's token is
// refused 403, and a credential of another network than the one the request
// names is refused 412, even for a pack of only audits.
func TestConnectControlRouteDiscardKeepsRouteGates(t *testing.T) {
	handler := router.NewRouter(t.Context(), Routes())
	audits := connectControlDiscardBody(t, connectControlDiscardAudit(false))

	guest := session.NewByJwt(server.NewId(), server.NewId(), "lag-guest", false, false)
	guest.LagGuest = true
	guestToken := guest.Client(server.NewId(), server.NewId()).Testing_Sign()
	discardedBefore := connectControlDiscardFamilyTotal(t, "urnetwork_connect_control_discarded_requests_total")
	w := connectControlDiscardServe(handler, audits, map[string]string{"Authorization": "Bearer " + guestToken})
	if w.Code != http.StatusForbidden {
		t.Fatalf("a lag.dev guest's audit: %d %s, want 403", w.Code, w.Body.String())
	}

	clientToken := session.NewByJwt(server.NewId(), server.NewId(), "synthetic", false, false).Client(server.NewId(), server.NewId()).Testing_Sign()
	w = connectControlDiscardServe(handler, audits, map[string]string{
		"Authorization":                 "Bearer " + clientToken,
		session.IntendedNetworkIdHeader: server.NewId().String(),
	})
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("an audit meant for another network: %d %s, want 412", w.Code, w.Body.String())
	}
	if discarded := connectControlDiscardFamilyTotal(t, "urnetwork_connect_control_discarded_requests_total") - discardedBefore; discarded != 0 {
		t.Fatalf("requests a gate refused were counted as %v discards", discarded)
	}
}

// Audit-only packs the discard declines: two audits, and one audit longer
// than its 512-character cap. Every connect version sends one audit of at
// most about 220 characters.
func connectControlDiscardDeclinedBodies(t testing.TB) map[string][]byte {
	t.Helper()
	longAudit := connectControlDiscardAudit(false)
	longAudit.PeerId = make([]byte, 512)
	return map[string][]byte{
		"two audits":        connectControlDiscardBody(t, connectControlDiscardAudit(false), connectControlDiscardAudit(true)),
		"an audit too long": connectControlDiscardBody(t, longAudit),
	}
}

// An audit-only pack the discard declines takes the authenticated path: no
// credential, or one that does not verify, is a 401, each request attempts
// authentication once, and none is counted as a discard.
func TestConnectControlRouteAuthenticatesAuditPacksItDoesNotDiscard(t *testing.T) {
	handler := router.NewRouter(t.Context(), Routes())
	for name, body := range connectControlDiscardDeclinedBodies(t) {
		for _, authorization := range []string{"", "Bearer synthetic-unverifiable-token"} {
			before := connectControlDiscardTotals(t)
			w := connectControlDiscardServe(handler, body, map[string]string{"Authorization": authorization})
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s, authorization %.20q: %d %s, want 401", name, authorization, w.Code, w.Body.String())
			}
			after := connectControlDiscardTotals(t)
			if attempts := after["urnetwork_session_auth_attempts_total"] - before["urnetwork_session_auth_attempts_total"]; attempts != 1 {
				t.Fatalf("%s attempted authentication %v times, want 1", name, attempts)
			}
			if discarded := after["urnetwork_connect_control_discarded_requests_total"] - before["urnetwork_connect_control_discarded_requests_total"]; discarded != 0 {
				t.Fatalf("%s was counted as %v discards", name, discarded)
			}
		}
	}
}
