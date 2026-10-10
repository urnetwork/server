package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Only explicit unavailable status is transient; an empty or malformed 200
// remains a decode fault, and unrelated server errors retain their classes.
func TestBlackholeDueUnavailableHasExactStatusBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"unavailable", 503, "synthetic-private-upstream-detail", ErrBlackholeDueUnavailable},
		{"rate", 429, "synthetic error", ErrRejected},
		{"internal", 500, "synthetic error", ErrRejected},
		{"gateway", 502, "synthetic error", ErrRejected},
		{"gateway_timeout", 504, "synthetic error", ErrRejected},
		{"unauthorized", 401, "synthetic error", ErrUnauthorized},
		{"unsupported", 404, "synthetic error", ErrBlackholeUnsupported},
		{"empty_success", 200, "", io.EOF},
		{"healthy_empty", 200, `{"providers":[]}`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer endpoint.Close()
			client := &Client{ServerUrl: endpoint.URL, Http: endpoint.Client()}
			due, err := client.BlackholeDue(t.Context(), 250)
			if len(due) != 0 || !errors.Is(err, test.want) || errors.Is(err, ErrBlackholeDueUnavailable) != (test.status == 503) {
				t.Fatalf("incorrect due status class: count=%d error=%v", len(due), err)
			}
			if test.status == 503 && (!errors.Is(err, ErrRejected) || strings.Contains(err.Error(), test.body)) {
				t.Fatal("unavailable lost rejection compatibility or exposed upstream body")
			}
		})
	}
}

func TestBlackholeDueMalformedSuccessIsNotUnavailable(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "not-json") }))
	defer endpoint.Close()
	client := &Client{ServerUrl: endpoint.URL, Http: endpoint.Client()}
	_, err := client.BlackholeDue(t.Context(), 250)
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) || errors.Is(err, ErrBlackholeDueUnavailable) {
		t.Fatalf("malformed 200 became transient unavailable: %v", err)
	}
}

// The read retry sentinel never authorizes retrying a potentially committed
// result submission whose response was unavailable.
func TestBlackholeSubmissionUnavailableIsNotDueRetry(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer endpoint.Close()
	client := &Client{ServerUrl: endpoint.URL, Http: endpoint.Client()}
	err := client.SubmitBlackholeChecks(context.Background(), []BlackholeCheck{{ClientId: "synthetic-provider", Ok: true, CheckedAt: time.Now()}})
	if !errors.Is(err, ErrRejected) || errors.Is(err, ErrBlackholeDueUnavailable) {
		t.Fatalf("submission inherited read retry authority: %v", err)
	}
}
