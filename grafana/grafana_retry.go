package grafana

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The load balancer in front of Grafana (warpctl's nginx limit_req, nodelay)
// answers a burst from one address with 429 before the request reaches
// Grafana. LoadDefaults makes one call per dashboard plus the public-dashboard
// checks, and an open dashboard in a browser on the same address spends the
// same budget, so a first call can be refused outright (2026-11-07: "Could not
// read folder (429)" twice from the owner's Mac while Grafana logged nothing).
//
// grafanaSend retries 429 for every method -- nginx refused the request, so it
// never ran and a repeat cannot double an effect -- and 502, 503 and 504, which
// a rolling Grafana block answers, for GET only: those may have reached Grafana
// and been applied. Waits follow Retry-After when it gives whole seconds (capped
// at grafanaRetryMaxWait), otherwise 1 s doubling to the cap.
const grafanaRetryAttempts = 6

const grafanaRetryMaxWait = 16 * time.Second

// Replaced in tests so retries do not sleep.
var grafanaRetrySleep = func(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func grafanaRetryable(method string, statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests:
		return true
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return method == http.MethodGet
	default:
		return false
	}
}

// The wait before the next attempt, after the given (1-based) attempt.
func grafanaRetryWait(attempt int, retryAfter string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && 0 < seconds {
		return min(time.Duration(seconds)*time.Second, grafanaRetryMaxWait)
	}
	return min(time.Second<<(attempt-1), grafanaRetryMaxWait)
}

var errGrafanaResponseTooLarge = errors.New("Grafana response exceeded the size limit")

// One Grafana API call with basic auth: body is the encoded JSON request body
// (nil for none) and headers the extra request headers. Returns the final
// status and at most responseLimit bytes of body; a longer body is an error.
func grafanaSend(
	ctx context.Context,
	httpClient *http.Client,
	method string,
	url string,
	username string,
	password string,
	body []byte,
	headers map[string]string,
	responseLimit int64,
) (int, []byte, error) {
	for attempt := 1; ; attempt++ {
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		request, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
		if err != nil {
			return 0, nil, err
		}
		request.SetBasicAuth(username, password)
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response, err := httpClient.Do(request)
		if err != nil {
			return 0, nil, err
		}
		responseBody, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
		response.Body.Close()
		if err != nil {
			return 0, nil, err
		}
		if attempt < grafanaRetryAttempts && grafanaRetryable(method, response.StatusCode) {
			if err := grafanaRetrySleep(ctx, grafanaRetryWait(attempt, response.Header.Get("Retry-After"))); err != nil {
				return 0, nil, err
			}
			continue
		}
		if responseLimit < int64(len(responseBody)) {
			return 0, nil, errGrafanaResponseTooLarge
		}
		return response.StatusCode, responseBody, nil
	}
}
