package handlers

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

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
		"dns_tld",
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
		"error",
		"expire_time",
		"ip",
		"ip_version",
		"record",
	}
	got := jsonFieldNames(reflect.TypeOf(controller.ExtenderActivateResult{}))
	if !slices.Equal(got, want) {
		t.Fatalf("ExtenderActivateResult fields = %v, want exactly %v", got, want)
	}
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
