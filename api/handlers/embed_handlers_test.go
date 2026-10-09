package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// The Embed routes (EMBED1.md): data caps, ACL groups, the network's Embed
// state and the public Services contact form. Pure tests: no database or redis.

// taggedJsonFieldNames lists a struct's exported json names, sorted.
func taggedJsonFieldNames(structType reflect.Type) []string {
	names := []string{}
	for i := range structType.NumField() {
		field := structType.Field(i)
		if !field.IsExported() || field.Anonymous {
			continue
		}
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		names = append(names, tag)
	}
	slices.Sort(names)
	return names
}

func assertJsonFieldNames(t *testing.T, value any, want []string) {
	t.Helper()
	got := taggedJsonFieldNames(reflect.TypeOf(value))
	if !slices.Equal(got, want) {
		t.Fatalf("%T fields = %v, want exactly %v", value, got, want)
	}
}

// Every authenticated Embed route refuses a request without a token before
// the model runs.
func TestEmbedRoutesRequireAToken(t *testing.T) {
	for _, route := range []struct {
		method  string
		path    string
		body    string
		handler http.HandlerFunc
	}{
		{http.MethodPost, "/network/client-data-cap", `{"client_id":"00000000-0000-0000-0000-000000000000"}`, NetworkClientDataCapSet},
		{http.MethodGet, "/network/client-data-cap?client_id=00000000-0000-0000-0000-000000000000", "", NetworkClientDataCapGet},
		{http.MethodGet, "/network/client-data-caps?limit=10", "", NetworkClientDataCapsList},
		{http.MethodPost, "/network/client-acl-group", `{"client_id":"00000000-0000-0000-0000-000000000000","acl_group":"isolated"}`, NetworkClientAclGroupSet},
		{http.MethodGet, "/network/client-acl-group?client_id=00000000-0000-0000-0000-000000000000", "", NetworkClientAclGroupGet},
		{http.MethodGet, "/network/embed", "", NetworkEmbedGet},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		w := httptest.NewRecorder()
		route.handler(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401 without a token", route.method, route.path, w.Code)
		}
	}
}

// The public contact form reads its body before anything else: a malformed
// body is refused with 400 and never reaches the rate limit or the store.
func TestServicesContactSalesRejectsAMalformedBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/services/contact-sales", strings.NewReader(`{`))
	w := httptest.NewRecorder()
	ServicesContactSales(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on a malformed body", w.Code)
	}
}

// The contact form's rate-limit refusal ("429 ..." from
// model/services_lead_model.go) becomes HTTP 429 with the message after the
// code; the spec documents the 429.
func TestServicesContactSalesRateLimitIsHttp429(t *testing.T) {
	w := httptest.NewRecorder()
	statusError := router.RaiseHttpError(errors.New("429 Too many requests. Please try again later."), w)
	if !statusError || w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d (status error %t), want 429", w.Code, statusError)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "Too many requests. Please try again later." {
		t.Fatalf("body = %q", body)
	}
	// no window is known, so no Retry-After is sent
	if retryAfter := w.Header().Get("Retry-After"); retryAfter != "" {
		t.Fatalf("Retry-After = %q, want none", retryAfter)
	}
}

// The wire names are the contract the docs, the OpenAPI spec and the embed
// examples are written against: renaming one breaks every customer backend
// while every server test still passes.
func TestEmbedWireFieldNames(t *testing.T) {
	assertJsonFieldNames(t, model.SetClientDataCapArgs{}, []string{
		"client_id",
		"monthly_byte_limit",
		"reset_total",
		"total_byte_limit",
	})
	assertJsonFieldNames(t, model.ClientDataCap{}, []string{
		"capped",
		"capped_reason",
		"client_id",
		"monthly_byte_limit",
		"monthly_period_end",
		"monthly_period_start",
		"monthly_used_byte_count",
		"total_byte_limit",
		"total_period_start",
		"total_used_byte_count",
	})
	assertJsonFieldNames(t, model.ClientDataCapResult{}, []string{"error"})
	assertJsonFieldNames(t, model.ListClientDataCapsResult{}, []string{"clients", "error", "next_cursor"})

	assertJsonFieldNames(t, model.SetNetworkClientAclGroupArgs{}, []string{"acl_group", "client_id"})
	assertJsonFieldNames(t, model.NetworkClientAclGroup{}, []string{"acl_group", "client_id"})
	assertJsonFieldNames(t, model.NetworkClientAclGroupResult{}, []string{"error"})

	assertJsonFieldNames(t, model.NetworkEmbed{}, []string{"active_client_count", "client_limit", "enabled"})
	assertJsonFieldNames(t, model.NetworkEmbedResult{}, []string{"error"})

	assertJsonFieldNames(t, model.ServicesContactSalesArgs{}, []string{
		"company",
		"email",
		"message",
		"monthly_active_users",
		"monthly_data_budget_byte_count",
		"name",
		"website",
	})
	assertJsonFieldNames(t, model.ServicesContactSalesResult{}, []string{"error", "request_id"})

	// the group names are the API's values
	if model.NetworkClientAclGroupDefault != "default" || model.NetworkClientAclGroupIsolated != "isolated" {
		t.Fatalf("acl group names = %q, %q", model.NetworkClientAclGroupDefault, model.NetworkClientAclGroupIsolated)
	}
	// the gated routes' refusal, which the spec quotes and backends match on
	if model.NetworkEmbedNotEnabledMessage != "Embed isn't enabled for this network." {
		t.Fatalf("embed refusal = %q", model.NetworkEmbedNotEnabledMessage)
	}
}
