package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/controller"
)

// `POST /network/extender-activate` (connect/EXTENDER.md C2).

// The route requires a client jwt. An activation is attributed to a network
// and a client and is rate limited per user, so an unauthenticated caller must
// never reach the probe -- which would otherwise dial an address of the
// caller's choosing.
func TestExtenderActivateRequiresAClientJwt(t *testing.T) {
	req := httptest.NewRequest(
		http.MethodPost,
		"/network/extender-activate",
		strings.NewReader(`{"public_key_hex":"aa","carriers":["tcp"]}`),
	)
	w := httptest.NewRecorder()

	ExtenderActivate(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a client jwt", w.Code)
	}
}

// A malformed body is refused before anything else, so a caller cannot reach
// the probe with a request the handler could not read.
func TestExtenderActivateRejectsAMalformedBody(t *testing.T) {
	req := httptest.NewRequest(
		http.MethodPost,
		"/network/extender-activate",
		strings.NewReader(`{`),
	)
	w := httptest.NewRecorder()

	ExtenderActivate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on a malformed body", w.Code)
	}
}

// The argument names are the wire contract of C2. A provider on a shipped
// binary sends exactly these, so renaming one silently breaks activation for
// every deployed provider while every server test still passes.
func TestExtenderActivateArgsFieldNames(t *testing.T) {
	want := []string{
		"carriers",
		"dns_port",
		"dns_ports",
		"dns_tld",
		// the lag.dev measurement request and opt-out (lag.dev/PLAN.md C4)
		"measurement_csr",
		"measurement_disabled",
		"public_key_hex",
		"tcp_port",
		"udp_port",
	}
	got := jsonFieldNames(reflect.TypeOf(controller.ExtenderActivateArgs{}))
	if !slices.Equal(got, want) {
		t.Fatalf("ExtenderActivateArgs fields = %v, want exactly %v", got, want)
	}
}

// The answer shape of C2, for the same reason: the provider reads `activated`,
// `error`, `record`, `bootstrap` and `allowed_hosts` to decide what to do next.
func TestExtenderActivateResultFieldNames(t *testing.T) {
	want := []string{
		"activated",
		"allowed_hosts",
		"bootstrap",
		"carriers",
		"dns_ports",
		"error",
		"expire_time",
		// the operator's id of the extender, which names its lag.dev
		// measurement certificate request (lag.dev/PLAN.md N4)
		"extender_id",
		"ip",
		"ip_version",
		"record",
		// the optional carriers that were left out of an accepted activation,
		// and why (EXTENDER.md C2a, connect#230)
		"refused_carriers",
		// when the operator has room for the next post, on a refusal that
		// judged nothing about the caller (connect#230)
		"retry_after_seconds",
	}
	got := jsonFieldNames(reflect.TypeOf(controller.ExtenderActivateResult{}))
	if !slices.Equal(got, want) {
		t.Fatalf("ExtenderActivateResult fields = %v, want exactly %v", got, want)
	}
	wantRefusal := []string{"carrier", "error", "port"}
	if got := jsonFieldNames(reflect.TypeOf(controller.ExtenderCarrierRefusal{})); !slices.Equal(got, wantRefusal) {
		t.Fatalf("ExtenderCarrierRefusal fields = %v, want exactly %v", got, wantRefusal)
	}
}

// An accepted activation names each optional carrier it left out (C2a,
// connect#230): the carrier, the port it was probed on and why, one entry per
// dns port. The document is literal, because it is what an extender reads.
// An answer that left nothing out carries no list at all.
func TestExtenderActivateRefusedCarriersWireForm(t *testing.T) {
	resultBytes, err := json.Marshal(&controller.ExtenderActivateResult{
		Activated: true,
		Carriers:  []string{"tcp", "dns"},
		DnsPorts:  []int{4053},
		RefusedCarriers: []*controller.ExtenderCarrierRefusal{
			{Carrier: "quic", Port: 443, Error: "timeout: no recent network activity"},
			{Carrier: "dns", Port: 53, Error: "context deadline exceeded"},
		},
	})
	if err != nil {
		t.Fatalf("the activation result did not encode: %v", err)
	}
	connect.AssertEqual(
		t,
		string(resultBytes),
		`{"activated":true,"carriers":["tcp","dns"],"dns_ports":[4053],"refused_carriers":[`+
			`{"carrier":"quic","port":443,"error":"timeout: no recent network activity"},`+
			`{"carrier":"dns","port":53,"error":"context deadline exceeded"}]}`,
	)

	acceptedBytes, err := json.Marshal(&controller.ExtenderActivateResult{
		Activated:       true,
		Carriers:        []string{"tcp"},
		RefusedCarriers: []*controller.ExtenderCarrierRefusal{},
	})
	if err != nil {
		t.Fatalf("the activation result did not encode: %v", err)
	}
	connect.AssertEqual(t, string(acceptedBytes), `{"activated":true,"carriers":["tcp"]}`)
}

// A refusal that judged nothing about the caller -- an operator busy probing
// other activations -- says when to come back in `retry_after_seconds`, beside
// the message (connect#230). The activator waits exactly that long instead of
// backing off, so the name is the wire. Every other answer leaves it out.
func TestExtenderActivateRetryAfterWireForm(t *testing.T) {
	busyBytes, err := json.Marshal(&controller.ExtenderActivateResult{
		Activated:         false,
		Error:             "The operator is busy probing other extenders. Please try again in 2 minutes.",
		RetryAfterSeconds: 90,
	})
	if err != nil {
		t.Fatalf("the activation result did not encode: %v", err)
	}
	connect.AssertEqual(
		t,
		string(busyBytes),
		`{"activated":false,"error":"The operator is busy probing other extenders. `+
			`Please try again in 2 minutes.","retry_after_seconds":90}`,
	)

	refusedBytes, err := json.Marshal(&controller.ExtenderActivateResult{
		Activated: false,
		Error:     "the tcp carrier did not answer: connection refused",
	})
	if err != nil {
		t.Fatalf("the activation result did not encode: %v", err)
	}
	connect.AssertEqual(
		t,
		string(refusedBytes),
		`{"activated":false,"error":"the tcp carrier did not answer: connection refused"}`,
	)
}

// The dns port list travels both ways under the same name (L2): a provider
// posts the ports it is listening on and reads back the ports the operator
// recorded. Both documents are literal here, because they are the wire: a
// shipped provider writes and reads exactly this, whatever the go fields are
// named.
func TestExtenderActivateDnsPortsWireForm(t *testing.T) {
	args := &controller.ExtenderActivateArgs{}
	if err := json.Unmarshal([]byte(
		`{"public_key_hex":"aabb","tcp_port":443,"udp_port":443,"dns_port":4053,`+
			`"dns_ports":[53,4053],"dns_tld":"ur.xyz.","carriers":["tcp","quic","dns"]}`,
	), args); err != nil {
		t.Fatalf("the activation args document did not decode: %v", err)
	}
	if !slices.Equal(args.DnsPorts, []int{53, 4053}) {
		t.Fatalf("dns_ports decoded to %v, want [53 4053]", args.DnsPorts)
	}
	connect.AssertEqual(t, args.DnsPort, 4053)

	// an args document that predates the list leaves it empty, which is what
	// the handler reads as the one configured port
	oldArgs := &controller.ExtenderActivateArgs{}
	if err := json.Unmarshal([]byte(
		`{"public_key_hex":"aabb","dns_port":4053,"carriers":["tcp"]}`,
	), oldArgs); err != nil {
		t.Fatalf("the old activation args document did not decode: %v", err)
	}
	connect.AssertEqual(t, len(oldArgs.DnsPorts), 0)

	resultBytes, err := json.Marshal(&controller.ExtenderActivateResult{
		Activated: true,
		DnsPorts:  []int{53, 4053},
	})
	if err != nil {
		t.Fatalf("the activation result did not encode: %v", err)
	}
	connect.AssertEqual(t, string(resultBytes), `{"activated":true,"dns_ports":[53,4053]}`)

	// and a refusal, or an activation with no dns carrier, carries no list at
	// all rather than an empty one
	emptyBytes, err := json.Marshal(&controller.ExtenderActivateResult{Activated: true})
	if err != nil {
		t.Fatalf("the activation result did not encode: %v", err)
	}
	connect.AssertEqual(t, string(emptyBytes), `{"activated":true}`)
}

// The hello field that carries the extender trust anchor (C7). A client with
// no root keys trusts no record at all, so this name is load bearing.
func TestHelloResultCarriesTheExtenderRootPublicKeys(t *testing.T) {
	got := jsonFieldNames(reflect.TypeOf(controller.HelloResult{}))
	if !slices.Contains(got, "extender_root_public_keys") {
		t.Fatalf("HelloResult fields = %v, want extender_root_public_keys among them", got)
	}
}

// The json names of one struct, sorted.
func jsonFieldNames(structType reflect.Type) []string {
	names := []string{}
	for i := range structType.NumField() {
		tag, _, _ := strings.Cut(structType.Field(i).Tag.Get("json"), ",")
		names = append(names, tag)
	}
	slices.Sort(names)
	return names
}
