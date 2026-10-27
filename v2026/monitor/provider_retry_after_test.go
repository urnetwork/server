// Retry delay authority must be bounded before seconds become nanoseconds.
// These controls use only in-memory HTTP responses and injected waits.
package monitor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This accepted integer is the first whole second above MaxInt64 nanoseconds.
// A 32-bit Atoi rejects it, so a skipped 32-bit control is not causal evidence.
func TestProviderHttpRetryAfterOverflowUsesCappedWait(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("accepted-seconds overflow boundary requires a 64-bit int target")
	}
	t.Logf("control target int_bits=%d", strconv.IntSize)
	if providerRetryMaximum != 30*time.Second || providerRequestAttempts != 3 {
		t.Fatal("control precondition: existing retry policy changed")
	}
	const header = "9223372037"
	if seconds, err := strconv.Atoi(header); err != nil || seconds <= 30 {
		t.Fatal("control precondition: overflowing seconds were not accepted")
	}
	calls, waits := 0, 0
	const requestBody = `{"query":"synthetic"}`
	client := newProviderHTTP(providerDoerFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(request.Body)
		if err != nil || string(body) != requestBody {
			t.Fatal("control precondition: query body was not replayed unchanged")
		}
		status, responseBody := http.StatusTooManyRequests, "private-retry-body"
		if calls == 3 {
			status, responseBody = http.StatusOK, "complete"
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {header}}, Body: io.NopCloser(strings.NewReader(responseBody))}, nil
	}))
	client.wait = func(ctx context.Context, delay time.Duration) error {
		waits++
		if ctx.Err() != nil || calls != waits || calls > 2 {
			t.Fatal("control precondition: live response-to-wait boundary not reached")
		}
		if delay != 30*time.Second {
			t.Fatal("accepted overflowing Retry-After bypassed the 30-second wait cap")
		}
		return nil
	}
	data, err := client.bytes(context.Background(), http.MethodPost, "https://api.example.test/report", "synthetic report", nil, []byte(requestBody), 1024)
	if err != nil || string(data) != "complete" || calls != 3 || waits != 2 {
		t.Fatal("capped retry did not preserve three attempts, body replay and successful completion")
	}
}

func TestProviderRetryDelayIntegerBoundaries(t *testing.T) {
	type delayCase struct {
		name   string
		header string
		want   time.Duration
	}
	cases := []delayCase{
		{name: "zero", header: "0", want: 0},
		{name: "one", header: "1", want: time.Second},
		{name: "below cap", header: "29", want: 29 * time.Second},
		{name: "cap", header: "30", want: 30 * time.Second},
		{name: "above cap", header: "31", want: 30 * time.Second},
		{name: "whitespace", header: " 31 ", want: 30 * time.Second},
		{name: "existing plus grammar", header: "+1", want: time.Second},
		{name: "target max int", header: strconv.Itoa(int(^uint(0) >> 1)), want: 30 * time.Second},
	}
	if strconv.IntSize == 64 {
		cases = append(cases,
			delayCase{name: "last safe duration second", header: "9223372036", want: 30 * time.Second},
			delayCase{name: "first overflowing duration second", header: "9223372037", want: 30 * time.Second},
		)
	}
	for _, test := range cases {
		if _, err := strconv.Atoi(strings.TrimSpace(test.header)); err != nil {
			t.Fatalf("%s: control precondition: integer header was not accepted", test.name)
		}
		if got := providerRetryDelay(test.header, 2*time.Second); got != test.want {
			t.Fatalf("%s: accepted seconds did not retain the bounded nonnegative delay", test.name)
		}
	}
}

func TestProviderRetryDelayDatesAndInvalidFallback(t *testing.T) {
	for _, test := range []struct {
		name     string
		header   string
		fallback time.Duration
		want     time.Duration
	}{
		{name: "past date", header: "Thu, 01 Jan 1970 00:00:00 GMT", fallback: 2 * time.Second, want: 0},
		{name: "future date", header: "Fri, 31 Dec 9999 23:59:59 GMT", fallback: 2 * time.Second, want: 30 * time.Second},
		{name: "blank", fallback: 2 * time.Second, want: 2 * time.Second},
		{name: "negative", header: "-1", fallback: 2 * time.Second, want: 2 * time.Second},
		{name: "fraction", header: "1.5", fallback: 2 * time.Second, want: 2 * time.Second},
		{name: "malformed", header: "private-invalid-retry-header", fallback: 2 * time.Second, want: 2 * time.Second},
		{name: "unrepresentable int", header: "9223372036854775808", fallback: 2 * time.Second, want: 2 * time.Second},
		{name: "fallback cap", header: "invalid", fallback: time.Minute, want: 30 * time.Second},
	} {
		if got := providerRetryDelay(test.header, test.fallback); got != test.want {
			t.Fatalf("%s: existing HTTP-date or invalid-header fallback policy changed", test.name)
		}
	}
}

func TestProviderHttpRetryAfterAttemptLimitAndPrivacy(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		calls, waits := 0, 0
		client := newProviderHTTP(providerDoerFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {strconv.Itoa(int(^uint(0) >> 1))}}, Body: io.NopCloser(strings.NewReader("private-provider-error-body"))}, nil
		}))
		client.wait = func(ctx context.Context, delay time.Duration) error {
			waits++
			if ctx.Err() != nil || delay != 30*time.Second {
				t.Fatal("exhausted retry lost its live bounded wait")
			}
			return nil
		}
		data, err := client.bytes(context.Background(), http.MethodGet, "https://api.example.test/report?signature=private-query-marker", "synthetic report", nil, nil, 1024)
		var visibility *providerVisibilityError
		if len(data) != 0 || !errors.As(err, &visibility) || visibility.class != providerAPIClass || calls != 3 || waits != 2 {
			t.Fatal("retry exhaustion changed attempt cap or provider visibility class")
		}
		for _, private := range []string{"private-provider-error-body", "private-query-marker", "api.example.test", strconv.Itoa(int(^uint(0) >> 1))} {
			if strings.Contains(err.Error(), private) {
				t.Fatal("retry exhaustion exposed private source data")
			}
		}
	}
}

func TestProviderHttpRetryAfterCancellationStopsNextAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls, waits := 0, 0
	client := newProviderHTTP(providerDoerFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Context().Err() != nil {
			t.Fatal("control precondition: response source did not start live")
		}
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"31"}}, Body: io.NopCloser(strings.NewReader("private-canceled-provider-body"))}, nil
	}))
	client.wait = func(parent context.Context, delay time.Duration) error {
		waits++
		if parent.Err() != nil || delay != 30*time.Second {
			t.Fatal("control precondition: capped wait did not start live")
		}
		cancel()
		// The real wait sees an already-ended parent and a positive capped
		// delay. Only cancellation is ready; no sleep or timer race is needed.
		return waitProviderRetry(parent, delay)
	}
	data, err := client.bytes(ctx, http.MethodGet, "https://api.example.test/report?token=private-canceled-query", "synthetic report", nil, nil, 1024)
	if len(data) != 0 || !errors.Is(err, context.Canceled) || calls != 1 || waits != 1 {
		t.Fatal("parent cancellation admitted another provider attempt")
	}
	for _, private := range []string{"private-canceled-provider-body", "private-canceled-query", "api.example.test"} {
		if strings.Contains(err.Error(), private) {
			t.Fatal("canceled retry exposed private source data")
		}
	}
}
