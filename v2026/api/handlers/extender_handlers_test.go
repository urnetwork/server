package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/controller"
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
		// the run of the process that posted (connect/EXTENDER.md G3)
		"previous_exit",
		"public_key_hex",
		// the key of the extender's tcp camouflage (connect/EXTENDER.md P1, P6)
		"reality_public_key_hex",
		"run_id",
		"run_start_time",
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
		// the operator keeps each address's ports, on every answer
		// (EXTENDER.md S10)
		"address_ports",
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
		// every probe step the activation ran, timed (EXTENDER.md C2,
		// connect#230)
		"probe_steps",
		"record",
		// which refusal the extender acts on, `key_other_network` for a key
		// another network holds (EXTENDER.md C2c)
		"refusal",
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
	// each left out carrier says how long its probe ran and what was left of
	// the probe budget when it started (connect#230)
	wantRefusal := []string{"budget_left_ms", "carrier", "elapsed_ms", "error", "port"}
	if got := jsonFieldNames(reflect.TypeOf(controller.ExtenderCarrierRefusal{})); !slices.Equal(got, wantRefusal) {
		t.Fatalf("ExtenderCarrierRefusal fields = %v, want exactly %v", got, wantRefusal)
	}
	wantStep := []string{"budget_left_ms", "elapsed_ms", "ip_version", "name", "outcome", "port"}
	if got := jsonFieldNames(reflect.TypeOf(controller.ExtenderProbeStep{})); !slices.Equal(got, wantStep) {
		t.Fatalf("ExtenderProbeStep fields = %v, want exactly %v", got, wantStep)
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
			{
				Carrier:      "quic",
				Port:         443,
				Error:        "connection refused",
				ElapsedMs:    12,
				BudgetLeftMs: 9400,
			},
			{
				Carrier:      "dns",
				Port:         53,
				Error:        "no answer within the 9.4 s left of the 10 s probe budget: context deadline exceeded",
				ElapsedMs:    9400,
				BudgetLeftMs: 9400,
			},
		},
	})
	if err != nil {
		t.Fatalf("the activation result did not encode: %v", err)
	}
	connect.AssertEqual(
		t,
		string(resultBytes),
		`{"activated":true,"carriers":["tcp","dns"],"dns_ports":[4053],"refused_carriers":[`+
			`{"carrier":"quic","port":443,"error":"connection refused","elapsed_ms":12,"budget_left_ms":9400},`+
			`{"carrier":"dns","port":53,"error":"no answer within the 9.4 s left of the 10 s probe budget: context deadline exceeded",`+
			`"elapsed_ms":9400,"budget_left_ms":9400}]}`,
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

// Every answer after the probe lists its steps, refusals included (C2,
// connect#230): the step, its family and port, how long it ran, what was left
// of the probe budget when it started, and how it ended. The refusal's last
// step is the one that refused it. The document is literal, because it is
// what an extender reads; an answer refused before the probe carries no
// list at all.
func TestExtenderActivateProbeStepsWireForm(t *testing.T) {
	refusalBytes, err := json.Marshal(&controller.ExtenderActivateResult{
		Activated: false,
		Error:     "the forward did not answer within the 0.5 s left of the 10 s probe budget: context deadline exceeded",
		ProbeSteps: []*controller.ExtenderProbeStep{
			{Name: "tcp", IpVersion: 4, Port: 443, ElapsedMs: 9500, BudgetLeftMs: 10000, Outcome: "ok"},
			{Name: "forward", IpVersion: 4, Port: 443, ElapsedMs: 500, BudgetLeftMs: 500, Outcome: "timeout"},
		},
	})
	if err != nil {
		t.Fatalf("the activation result did not encode: %v", err)
	}
	connect.AssertEqual(
		t,
		string(refusalBytes),
		`{"activated":false,"probe_steps":[`+
			`{"name":"tcp","ip_version":4,"port":443,"elapsed_ms":9500,"budget_left_ms":10000,"outcome":"ok"},`+
			`{"name":"forward","ip_version":4,"port":443,"elapsed_ms":500,"budget_left_ms":500,"outcome":"timeout"}],`+
			`"error":"the forward did not answer within the 0.5 s left of the 10 s probe budget: context deadline exceeded"}`,
	)

	unprobedBytes, err := json.Marshal(&controller.ExtenderActivateResult{
		Activated: false,
		Error:     "an activation must offer the tcp carrier",
	})
	if err != nil {
		t.Fatalf("the activation result did not encode: %v", err)
	}
	connect.AssertEqual(t, string(unprobedBytes), `{"activated":false,"error":"an activation must offer the tcp carrier"}`)
}

// The answer stays readable to the providers already shipped, which decode it
// into connect's ExtenderActivateResult: the step list and the timing of the
// refused carriers are fields that type does not have, so they are skipped,
// and what it does read -- whether the extender is active, why not, the
// carriers, the record -- reads as before. Both a refusal and an accepted
// activation are read here exactly as connect's activation post reads them.
func TestExtenderActivateAnswerReadsOnAShippedProvider(t *testing.T) {
	steps := []*controller.ExtenderProbeStep{
		{Name: "tcp", IpVersion: 6, Port: 443, ElapsedMs: 120, BudgetLeftMs: 10000, Outcome: "ok"},
		{Name: "forward", IpVersion: 6, Port: 443, ElapsedMs: 310, BudgetLeftMs: 9880, Outcome: "ok"},
		{Name: "dns", IpVersion: 6, Port: 4053, ElapsedMs: 9570, BudgetLeftMs: 9570, Outcome: "timeout"},
	}
	for _, result := range []*controller.ExtenderActivateResult{
		{
			Activated:  false,
			Error:      "the forward failed after 0.31 s: extender hello answered with status 502",
			ProbeSteps: steps[:2],
		},
		{
			Activated: true,
			Ip:        "2001:db8::10",
			IpVersion: 6,
			Carriers:  []string{"tcp"},
			RefusedCarriers: []*controller.ExtenderCarrierRefusal{
				{
					Carrier:      "dns",
					Port:         4053,
					Error:        "no answer within the 9.57 s left of the 10 s probe budget: context deadline exceeded",
					ElapsedMs:    9570,
					BudgetLeftMs: 9570,
				},
			},
			ProbeSteps: steps,
			Record:     "cmVjb3Jk",
		},
	} {
		resultBytes, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("the activation result did not encode: %v", err)
		}
		shipped := &connect.ExtenderActivateResult{}
		if err := json.Unmarshal(resultBytes, shipped); err != nil {
			t.Fatalf("a shipped provider cannot read the answer %s: %v", resultBytes, err)
		}
		connect.AssertEqual(t, shipped.Activated, result.Activated)
		connect.AssertEqual(t, shipped.Error, result.Error)
		connect.AssertEqual(t, shipped.Ip, result.Ip)
		connect.AssertEqual(t, shipped.IpVersion, result.IpVersion)
		if !slices.Equal(shipped.Carriers, result.Carriers) {
			t.Fatalf("a shipped provider read carriers %v, want %v", shipped.Carriers, result.Carriers)
		}
		connect.AssertEqual(t, shipped.Record, result.Record)
	}
}

// The run of the process that posted (connect/EXTENDER.md G3) decodes from the
// document an extender sends, and a document that predates it, which is what
// every extender on an older release sends, leaves all three unset. The
// document is literal because it is the wire.
func TestExtenderActivateRunWireForm(t *testing.T) {
	args := &controller.ExtenderActivateArgs{}
	if err := json.Unmarshal([]byte(
		`{"public_key_hex":"aabb","carriers":["tcp"],`+
			`"run_id":"00112233445566778899aabbccddeeff",`+
			`"run_start_time":"2026-11-02T03:04:05.000006Z","previous_exit":"sigkill"}`,
	), args); err != nil {
		t.Fatalf("the activation args document did not decode: %v", err)
	}
	connect.AssertEqual(t, args.RunId, "00112233445566778899aabbccddeeff")
	connect.AssertEqual(t, args.RunStartTime, "2026-11-02T03:04:05.000006Z")
	connect.AssertEqual(t, args.PreviousExit, "sigkill")

	// a start that is no time still decodes, so it is judged and dropped with
	// the rest of the run rather than refusing the request
	malformedArgs := &controller.ExtenderActivateArgs{}
	if err := json.Unmarshal([]byte(
		`{"public_key_hex":"aabb","carriers":["tcp"],"run_start_time":"yesterday"}`,
	), malformedArgs); err != nil {
		t.Fatalf("a malformed run start failed the whole request: %v", err)
	}
	connect.AssertEqual(t, malformedArgs.RunStartTime, "yesterday")

	oldArgs := &controller.ExtenderActivateArgs{}
	if err := json.Unmarshal([]byte(
		`{"public_key_hex":"aabb","carriers":["tcp"]}`,
	), oldArgs); err != nil {
		t.Fatalf("the old activation args document did not decode: %v", err)
	}
	connect.AssertEqual(t, oldArgs.RunId, "")
	connect.AssertEqual(t, oldArgs.RunStartTime, "")
	connect.AssertEqual(t, oldArgs.PreviousExit, "")
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
