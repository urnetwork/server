package egresshealth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

type syntheticLoadStageError struct {
	stage string
}

func (self syntheticLoadStageError) Error() string             { return "private-token.example/path" }
func (self syntheticLoadStageError) ProviderHttpStage() string { return self.stage }

func TestFailedLoadStageSurvivesHttpWrappingWithoutLeaking(t *testing.T) {
	client := &http.Client{Transport: echoStageRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, syntheticLoadStageError{stage: "dial_dns_or_socket"}
	})}
	result := fetch(context.Background(), client,
		Destination{Name: "synthetic", Class: ClassSite, Url: "https://example.test/check"},
		time.Second, DefaultRequestProfile(), time.Now)
	if result.Ok || result.FailureStage != "dial_dns_or_socket" {
		t.Fatalf("failed load stage = %q, ok=%t", result.FailureStage, result.Ok)
	}
	run := &Result{Checks: []CheckResult{result}}
	if got := run.FailureStageSummary(); got != "dial_dns_or_socket:1" {
		t.Fatalf("failure stages = %q", got)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "FailureStage") || strings.Contains(string(encoded), "dial_dns_or_socket") {
		t.Fatal("local stage leaked into the ingestion encoding")
	}
}

func TestFailureStageSummaryBoundsUntrustedStages(t *testing.T) {
	var nilRun *Result
	if got := nilRun.FailureStageSummary(); got != "" {
		t.Fatalf("nil stages = %q", got)
	}
	run := &Result{Checks: []CheckResult{
		{FailureStage: "private-token.example/path"},
		{FailureStage: "tls"},
		{FailureStage: "tls", Ok: true},
		{FailureStage: "run_ended", NotMeasured: true},
	}}
	if got := run.FailureStageSummary(); got != "tls:1,run_ended:1,unknown:1" {
		t.Fatalf("bounded stages = %q", got)
	}
}

func TestRequestFailureStagesDistinguishContextAndTransport(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "request_timeout"},
		{context.Canceled, "request_canceled"},
		{io.ErrUnexpectedEOF, "request_eof"},
		{errors.New("private-token.example/path"), "request_unknown"},
	}
	for _, test := range cases {
		client := &http.Client{Transport: echoStageRoundTripper(func(*http.Request) (*http.Response, error) {
			return nil, test.err
		})}
		result := fetch(context.Background(), client,
			Destination{Name: "synthetic", Class: ClassSite, Url: "https://example.test/check"},
			time.Second, DefaultRequestProfile(), time.Now)
		if result.Ok || result.FailureStage != test.want {
			t.Fatalf("failure stage = %q, want %q", result.FailureStage, test.want)
		}
		if got := (&Result{Checks: []CheckResult{result}}).FailureStageSummary(); got != test.want+":1" {
			t.Fatalf("summary = %q, want %q", got, test.want+":1")
		}
	}
}

// A generic net/http deadline can hide whether the request ever obtained a
// provider connection. Preserve that distinction without copying URLs or
// changing the measured failure, guard, or retry behavior.
func TestRequestTimeoutProgressSeparatesConnectionFromResponse(t *testing.T) {
	for _, test := range []struct {
		phase int
		want  string
	}{
		{phase: 0, want: "request_connect_timeout"},
		{phase: 1, want: "request_write_timeout"},
		{phase: 2, want: "request_response_timeout"},
		{phase: 3, want: "request_timeout"},
	} {
		client := &http.Client{Transport: echoStageRoundTripper(func(req *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(req.Context())
			if trace == nil {
				return nil, errors.New("request progress trace missing")
			}
			trace.GetConn("synthetic.example")
			if test.phase >= 1 {
				trace.GotConn(httptrace.GotConnInfo{})
			}
			if test.phase >= 2 {
				trace.WroteRequest(httptrace.WroteRequestInfo{})
			}
			if test.phase >= 3 {
				trace.GotFirstResponseByte()
			}
			return nil, context.DeadlineExceeded
		})}
		result := fetch(context.Background(), client,
			Destination{Name: "synthetic", Class: ClassSite, Url: "https://example.test/check"},
			time.Second, DefaultRequestProfile(), time.Now)
		if result.Ok || result.FailureStage != test.want {
			t.Fatalf("phase=%d stage=%q, want %q", test.phase, result.FailureStage, test.want)
		}
		if got := (&Result{Checks: []CheckResult{result}}).FailureStageSummary(); got != test.want+":1" {
			t.Fatalf("phase=%d summary=%q", test.phase, got)
		}
	}
}

func TestRequestTimeoutProgressSubstagesDoNotGuessAfterCompletion(t *testing.T) {
	cases := []struct {
		name    string
		advance func(*httptrace.ClientTrace)
		want    string
	}{
		{name: "dns pending", advance: func(trace *httptrace.ClientTrace) {
			trace.DNSStart(httptrace.DNSStartInfo{Host: "synthetic.example"})
		}, want: "request_dns_timeout"},
		{name: "dns completed", advance: func(trace *httptrace.ClientTrace) {
			trace.DNSStart(httptrace.DNSStartInfo{Host: "synthetic.example"})
			trace.DNSDone(httptrace.DNSDoneInfo{})
		}, want: "request_connect_timeout"},
		{name: "dial pending", advance: func(trace *httptrace.ClientTrace) {
			trace.ConnectStart("tcp", "192.0.2.1:443")
		}, want: "request_dial_timeout"},
		{name: "dial completed", advance: func(trace *httptrace.ClientTrace) {
			trace.ConnectStart("tcp", "192.0.2.1:443")
			trace.ConnectDone("tcp", "192.0.2.1:443", nil)
		}, want: "request_connect_timeout"},
		{name: "tls pending", advance: func(trace *httptrace.ClientTrace) {
			trace.TLSHandshakeStart()
		}, want: "request_tls_timeout"},
		{name: "tls completed", advance: func(trace *httptrace.ClientTrace) {
			trace.TLSHandshakeStart()
			trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
		}, want: "request_connect_timeout"},
	}
	for _, test := range cases {
		client := &http.Client{Transport: echoStageRoundTripper(func(req *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(req.Context())
			if trace == nil {
				return nil, errors.New("request progress trace missing")
			}
			trace.GetConn("synthetic.example")
			test.advance(trace)
			return nil, context.DeadlineExceeded
		})}
		result := fetch(context.Background(), client,
			Destination{Name: "synthetic", Class: ClassSite, Url: "https://example.test/check"},
			time.Second, DefaultRequestProfile(), time.Now)
		if result.FailureStage != test.want {
			t.Fatalf("%s: failure stage = %q, want %q", test.name, result.FailureStage, test.want)
		}
		if got := (&Result{Checks: []CheckResult{result}}).FailureStageSummary(); got != test.want+":1" {
			t.Fatalf("%s: summary = %q, want %q", test.name, got, test.want+":1")
		}
	}
}
