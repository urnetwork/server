package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const paymentReadTestId = "synthetic-transaction"

type paymentReadTestBody struct {
	reader    io.Reader
	afterRead func()
	closeErr  error
	closed    *int
	readBytes int
}

func (self *paymentReadTestBody) Read(buffer []byte) (int, error) {
	n, err := self.reader.Read(buffer)
	self.readBytes += n
	if self.afterRead != nil {
		afterRead := self.afterRead
		self.afterRead = nil
		afterRead()
	}
	return n, err
}

func (self *paymentReadTestBody) Close() error {
	if self.closed != nil {
		*self.closed++
	}
	return self.closeErr
}

func paymentReadTestResponse(status int, payload string, closed *int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Body: &paymentReadTestBody{
			reader: strings.NewReader(payload),
			closed: closed,
		},
	}
}

func paymentReadTestPayload(id string) string {
	return fmt.Sprintf(`{"data":{"transaction":{"id":%q,"state":"CONFIRMED"}}}`, id)
}

func paymentReadTestCircle(hooks paymentReadHooks) *CoreCircleApiClient {
	return &CoreCircleApiClient{
		readHooks: hooks,
		readToken: func() string { return "synthetic-read-token" },
	}
}

func requirePaymentReadRequest(t *testing.T, request *http.Request) {
	t.Helper()
	if request.Method != http.MethodGet || request.Body != nil {
		t.Fatalf("payment observation was not a body-free GET: %s", request.Method)
	}
	deadline, ok := request.Context().Deadline()
	if !ok || time.Until(deadline) > 60*time.Second {
		t.Fatal("attempt lacks the caller-owned finite deadline")
	}
}

func awaitPaymentReadResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("payment read did not join after explicit release/cancellation")
		return nil
	}
}

func awaitPaymentReadBarrier(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("payment read did not reach explicit barrier")
	}
}

// Only the retry clock is accelerated; the actual Core GET builds every request,
// consumes each response body, and checks the processor transaction identity.
func TestPaymentReadCircleSurvivesTransientOutageBeyondOneMinute(t *testing.T) {
	started := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	now := started
	calls, closed := 0, 0
	client := paymentReadTestCircle(paymentReadHooks{
		now: func() time.Time { return now },
		wait: func(ctx context.Context, delay time.Duration) error {
			if ctx.Err() != nil || closed != calls || delay < time.Second || delay >= 2*time.Second {
				t.Fatal("retry did not close prior response before a bounded pause")
			}
			now = now.Add(delay)
			return nil
		},
		do: func(request *http.Request) (*http.Response, error) {
			requirePaymentReadRequest(t, request)
			if request.URL.Path != "/v1/w3s/transactions/"+paymentReadTestId || request.Header.Get("Authorization") != "Bearer synthetic-read-token" || closed != calls {
				t.Fatal("actual transaction request or prior body ownership changed")
			}
			calls++
			if now.Sub(started) < 65*time.Second {
				return paymentReadTestResponse(http.StatusServiceUnavailable, "temporary outage", &closed), nil
			}
			return paymentReadTestResponse(http.StatusOK, paymentReadTestPayload(paymentReadTestId), &closed), nil
		},
	})
	result, err := client.GetTransaction(context.Background(), paymentReadTestId)
	if err != nil || result == nil || result.Transaction.Id != paymentReadTestId || calls < 2 || closed != calls || now.Sub(started) < 65*time.Second || now.Sub(started) >= 67*time.Second {
		t.Fatalf("actual GET did not recover after the virtual 65-second outage: result=%v err=%v calls=%d closed=%d elapsed=%v", result, err, calls, closed, now.Sub(started))
	}
}

func TestPaymentReadCircleRetriesTypedTransportTimeoutAndDisconnect(t *testing.T) {
	now := time.Now()
	calls, closed := 0, 0
	client := paymentReadTestCircle(paymentReadHooks{
		now:  func() time.Time { return now },
		wait: func(ctx context.Context, delay time.Duration) error { now = now.Add(delay); return ctx.Err() },
		do: func(request *http.Request) (*http.Response, error) {
			requirePaymentReadRequest(t, request)
			calls++
			if calls == 1 {
				return nil, &net.OpError{Op: "read", Net: "tcp", Err: context.DeadlineExceeded}
			}
			if calls == 2 {
				return nil, fmt.Errorf("transport peer closed: %w", io.EOF)
			}
			return paymentReadTestResponse(http.StatusOK, paymentReadTestPayload(paymentReadTestId), &closed), nil
		},
	})
	result, err := client.GetTransaction(context.Background(), paymentReadTestId)
	if err != nil || result == nil || calls != 3 || closed != 1 {
		t.Fatalf("typed transport failures did not recover: %v calls=%d closed=%d", err, calls, closed)
	}
}

func TestPaymentReadCircleExhaustsOwnedBudget(t *testing.T) {
	for _, budget := range []time.Duration{0, 60 * time.Second} {
		started := time.Now()
		now := started
		calls, closed := 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			now: func() time.Time { return now },
			wait: func(ctx context.Context, delay time.Duration) error {
				if closed != calls || delay <= 0 || delay >= 2*time.Second {
					t.Fatal("invalid body ownership or retry delay")
				}
				now = now.Add(delay)
				return ctx.Err()
			},
			do: func(request *http.Request) (*http.Response, error) {
				requirePaymentReadRequest(t, request)
				calls++
				return paymentReadTestResponse(http.StatusServiceUnavailable, "still unavailable", &closed), nil
			},
		})
		client.readSettings = PaymentReadSettings{Budget: budget}
		result, err := client.GetTransaction(context.Background(), paymentReadTestId)
		expectedBudget := budget
		if expectedBudget == 0 {
			expectedBudget = 300 * time.Second
		}
		if result != nil || !errors.Is(err, context.DeadlineExceeded) || now.Sub(started) != expectedBudget || calls < 2 || closed != calls {
			t.Fatalf("GET did not retain its full virtual budget %v: %v elapsed=%v calls=%d closed=%d", expectedBudget, err, now.Sub(started), calls, closed)
		}
	}
}

func TestPaymentReadCirclePermanentResponsesDoNotRetry(t *testing.T) {
	cases := []struct {
		status  int
		payload string
	}{
		{status: http.StatusUnauthorized, payload: "authentication refused"},
		{status: http.StatusForbidden, payload: "authorization refused"},
		{status: http.StatusNotFound, payload: "unknown transaction"},
		{status: http.StatusNotImplemented, payload: "unsupported operation"},
		{status: http.StatusOK, payload: "{malformed"},
		{status: http.StatusOK, payload: paymentReadTestPayload("different-transaction")},
	}
	for _, item := range cases {
		calls, closed, waits := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") },
			do: func(request *http.Request) (*http.Response, error) {
				requirePaymentReadRequest(t, request)
				calls++
				return paymentReadTestResponse(item.status, item.payload, &closed), nil
			},
		})
		result, err := client.GetTransaction(context.Background(), paymentReadTestId)
		if result != nil || err == nil || calls != 1 || closed != 1 || waits != 0 {
			t.Fatalf("permanent status/payload retried: status=%d result=%v err=%v calls=%d closed=%d waits=%d", item.status, result, err, calls, closed, waits)
		}
	}
}

func TestPaymentReadCircleJoinedHardCauseDominatesRetry(t *testing.T) {
	hard := errors.New("synthetic authentication or custody refusal")
	for _, withResponse := range []bool{false, true} {
		calls, closed, waits := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") },
			do: func(request *http.Request) (*http.Response, error) {
				requirePaymentReadRequest(t, request)
				calls++
				if !withResponse {
					return nil, errors.Join(context.DeadlineExceeded, hard)
				}
				response := paymentReadTestResponse(http.StatusServiceUnavailable, "temporary status", &closed)
				response.Body.(*paymentReadTestBody).closeErr = hard
				return response, nil
			},
		})
		result, err := client.GetTransaction(context.Background(), paymentReadTestId)
		if result != nil || !errors.Is(err, hard) || calls != 1 || waits != 0 || withResponse && closed != 1 {
			t.Fatalf("joined hard cause was retried: response=%v err=%v calls=%d closed=%d waits=%d", withResponse, err, calls, closed, waits)
		}
	}
}

func TestPaymentReadCircleCanceledTransportJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, exited := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	client := paymentReadTestCircle(paymentReadHooks{
		do: func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			close(entered)
			defer close(exited)
			<-request.Context().Done()
			return nil, request.Context().Err()
		},
	})
	done := make(chan error, 1)
	go func() { _, err := client.GetTransaction(ctx, paymentReadTestId); done <- err }()
	awaitPaymentReadBarrier(t, entered)
	cancel()
	if err := awaitPaymentReadResult(t, done); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatalf("canceled GET retried or lost cause: %v calls=%d", err, calls.Load())
	}
	select {
	case <-exited:
	default:
		t.Fatal("GET returned before its actual transport invocation joined")
	}
}

func TestPaymentReadCircleCanceledPauseDoesNotSendAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	calls, closed := 0, 0
	client := paymentReadTestCircle(paymentReadHooks{
		wait: func(ctx context.Context, delay time.Duration) error {
			if closed != 1 {
				return errors.New("retry pause preceded response closure")
			}
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		},
		do: func(request *http.Request) (*http.Response, error) {
			calls++
			return paymentReadTestResponse(http.StatusServiceUnavailable, "temporary outage", &closed), nil
		},
	})
	done := make(chan error, 1)
	go func() { _, err := client.GetTransaction(ctx, paymentReadTestId); done <- err }()
	awaitPaymentReadBarrier(t, entered)
	cancel()
	if err := awaitPaymentReadResult(t, done); !errors.Is(err, context.Canceled) || calls != 1 || closed != 1 {
		t.Fatalf("canceled pause admitted another request: %v calls=%d closed=%d", err, calls, closed)
	}
}

func TestPaymentReadCircleCanceledCompleteBodyCannotBeAccepted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls, closed := 0, 0
	client := paymentReadTestCircle(paymentReadHooks{
		do: func(request *http.Request) (*http.Response, error) {
			calls++
			response := paymentReadTestResponse(http.StatusOK, paymentReadTestPayload(paymentReadTestId), &closed)
			response.Body.(*paymentReadTestBody).afterRead = cancel
			return response, nil
		},
	})
	result, err := client.GetTransaction(ctx, paymentReadTestId)
	if result != nil || !errors.Is(err, context.Canceled) || calls != 1 || closed != 1 {
		t.Fatalf("canceled full response was admitted or leaked: %v result=%v calls=%d closed=%d", err, result, calls, closed)
	}
}

func TestPaymentReadCircleResponseBoundAndClose(t *testing.T) {
	calls, closed, waits := 0, 0, 0
	body := &paymentReadTestBody{reader: strings.NewReader(strings.Repeat("x", maximumPaymentReadBytes+4096)), closed: &closed}
	client := paymentReadTestCircle(paymentReadHooks{
		wait: func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") },
		do: func(request *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
		},
	})
	result, err := client.GetTransaction(context.Background(), paymentReadTestId)
	if result != nil || err == nil || !strings.Contains(err.Error(), "response exceeds") || body.readBytes != maximumPaymentReadBytes+1 || calls != 1 || closed != 1 || waits != 0 {
		t.Fatalf("oversized body was not bounded, refused and closed: %v bytes=%d calls=%d closed=%d waits=%d", err, body.readBytes, calls, closed, waits)
	}
}

func TestPaymentReadCallerPolicyAndCanceledAdmission(t *testing.T) {
	settings, err := (PaymentReadSettings{}).normalized()
	if err != nil || settings.Budget != 300*time.Second || settings.AttemptTimeout != 60*time.Second {
		t.Fatalf("incorrect production default: %+v %v", settings, err)
	}
	for _, invalid := range []PaymentReadSettings{
		{Budget: 59 * time.Second}, {Budget: -time.Second}, {Budget: 16 * time.Minute},
		{AttemptTimeout: -time.Second}, {AttemptTimeout: 61 * time.Second},
	} {
		if _, err := NewCoreCircleApiClient(invalid); err == nil {
			t.Fatalf("Circle admitted invalid policy %+v", invalid)
		}
		if _, err := NewCoreCoinbaseClient(invalid); err == nil {
			t.Fatalf("Coinbase admitted invalid policy %+v", invalid)
		}
	}
	circle := &CoreCircleApiClient{readToken: func() string { t.Fatal("canceled read accessed token configuration"); return "" }}
	coinbase := &CoreCoinbaseClient{readHost: func() string { t.Fatal("canceled read accessed host configuration"); return "" }}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelExpired()
	for _, ctx := range []context.Context{nil, canceled, expired} {
		if result, err := circle.GetTransaction(ctx, paymentReadTestId); result != nil || err == nil {
			t.Fatal("Circle admitted absent/canceled owner")
		}
		if result, err := coinbase.FetchExchangeRates(ctx, "USD"); result != nil || err == nil {
			t.Fatal("Coinbase admitted absent/canceled owner")
		}
	}
}

func TestPaymentReadConcurrentOwnersRemainIndependent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	enteredCanceled, enteredHealthy, releaseHealthy := make(chan struct{}), make(chan struct{}), make(chan struct{})
	client := paymentReadTestCircle(paymentReadHooks{
		do: func(request *http.Request) (*http.Response, error) {
			id := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
			if id == "canceled-owner" {
				close(enteredCanceled)
				<-request.Context().Done()
				return nil, request.Context().Err()
			}
			close(enteredHealthy)
			select {
			case <-request.Context().Done():
				return nil, request.Context().Err()
			case <-releaseHealthy:
			}
			return paymentReadTestResponse(http.StatusOK, paymentReadTestPayload(id), nil), nil
		},
	})
	canceledDone, healthyDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := client.GetTransaction(ctx, "canceled-owner"); canceledDone <- err }()
	go func() {
		result, err := client.GetTransaction(context.Background(), "healthy-owner")
		if err == nil && (result == nil || result.Transaction.Id != "healthy-owner") {
			err = errors.New("healthy owner lost its result")
		}
		healthyDone <- err
	}()
	awaitPaymentReadBarrier(t, enteredCanceled)
	awaitPaymentReadBarrier(t, enteredHealthy)
	cancel()
	if err := awaitPaymentReadResult(t, canceledDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled owner failed to join: %v", err)
	}
	close(releaseHealthy)
	if err := awaitPaymentReadResult(t, healthyDone); err != nil {
		t.Fatalf("one canceled owner stopped its independent peer: %v", err)
	}
}

func TestPaymentReadCoinbaseTransientRecoveryAndCurrencyIdentity(t *testing.T) {
	for _, returnedCurrency := range []string{"USD", "EUR"} {
		now := time.Now()
		calls, closed, waits := 0, 0, 0
		client := &CoreCoinbaseClient{
			readHost: func() string { return "rates.example" },
			readHooks: paymentReadHooks{
				now:  func() time.Time { return now },
				wait: func(ctx context.Context, delay time.Duration) error { waits++; now = now.Add(delay); return ctx.Err() },
				do: func(request *http.Request) (*http.Response, error) {
					requirePaymentReadRequest(t, request)
					if request.URL.Host != "rates.example" || request.URL.Path != "/v2/exchange-rates" || request.URL.Query().Get("currency") != "USD" || closed != calls {
						t.Fatal("exchange-rate request or body ownership changed")
					}
					calls++
					if calls == 1 {
						return paymentReadTestResponse(http.StatusServiceUnavailable, "temporary outage", &closed), nil
					}
					return paymentReadTestResponse(http.StatusOK, fmt.Sprintf(`{"data":{"currency":%q,"rates":{"USD":"1"}}}`, returnedCurrency), &closed), nil
				},
			},
		}
		result, err := client.FetchExchangeRates(context.Background(), "USD")
		if calls != 2 || closed != 2 || waits != 1 {
			t.Fatalf("exchange-rate GET retry ownership failed: %v calls=%d closed=%d waits=%d", err, calls, closed, waits)
		}
		if returnedCurrency == "USD" {
			if err != nil || result == nil || result.Rates["USD"] != "1" {
				t.Fatalf("exchange-rate GET did not recover: %v result=%v", err, result)
			}
		} else if err == nil || result != nil || !strings.Contains(err.Error(), "currency mismatch") {
			t.Fatalf("wrong currency was admitted: %v result=%v", err, result)
		}
	}
}
