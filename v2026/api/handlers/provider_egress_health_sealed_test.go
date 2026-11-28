package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The ingest decodes a run strictly, so a field it does not know refuses the
// whole run. A post-quantum turn whose session sealed while its IPv4 HTTPS
// leg wrote no request sends e2e.sealed (owner, 2026-11-24), so this api must
// know the field before any prober sends it; a field no release knows still
// refuses the run.
func TestProviderEgressHealthBodyAcceptsASealWithoutARequest(t *testing.T) {
	body := `{"client_id":"` + server.NewId().String() + `","ok_count":0,"total_count":1,"url_probe_evidence":{"e2e":` +
		`{"pq":"unmeasured","key_exchange":"X25519MLKEM768","sealed_before_request":false,"window":"resizable","sealed":true}}}`
	read := func(body string) (*SubmitProviderEgressHealthArgs, *httptest.ResponseRecorder, bool) {
		request := httptest.NewRequest(http.MethodPost, "/network/provider-egress-health", strings.NewReader(body))
		recorder := httptest.NewRecorder()
		var args SubmitProviderEgressHealthArgs
		ok := readStrictOperatorRequestBody(recorder, request, &args)
		return &args, recorder, ok
	}

	args, recorder, ok := read(body)
	if !ok {
		t.Fatalf("the strict body refused a seal without a request: %d %s", recorder.Code, recorder.Body.String())
	}
	if args.UrlProbeEvidence == nil || args.UrlProbeEvidence.E2e == nil || !args.UrlProbeEvidence.E2e.Sealed {
		t.Fatalf("decoded evidence %+v", args.UrlProbeEvidence)
	}

	_, recorder, ok = read(strings.Replace(body, `"sealed":true`, `"sealed_maybe":true`, 1))
	if ok || recorder.Code != http.StatusBadRequest {
		t.Fatalf("a field no release knows was accepted: %d", recorder.Code)
	}
}
