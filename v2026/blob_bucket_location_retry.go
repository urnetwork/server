// Recover the pinned SDK's cold-location HTTP 429 gap before any retained
// upload, without replaying an object write or changing its retention proof.
package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// Bounds additional read-only discovery after the first throttle response.
// The initial request retains its caller's complete original lifetime.
type BlobBucketLocationRetrySettings struct {
	// Total discovery attempts, including the first; at most one disables preflight.
	MaxAttempts int
	// Additional retry window from the first 429 headers; it does not shorten
	// the original request/body lifetime. Non-positive disables preflight.
	RetryWindow time.Duration
	// Initial inter-request delay; non-positive disables preflight to avoid busy retries.
	InitialBackoff time.Duration
	// Maximum exponential delay, excluding longer Retry-After hints; non-positive disables preflight.
	MaxBackoff time.Duration
}

// Two delayed read-only retries absorb a brief throttle without a long loop.
func DefaultBlobBucketLocationRetrySettings() BlobBucketLocationRetrySettings {
	return BlobBucketLocationRetrySettings{
		MaxAttempts: 3, RetryWindow: 5 * time.Second,
		InitialBackoff: 250 * time.Millisecond, MaxBackoff: time.Second,
	}
}

// Settings are copied per client; test seams are installed before concurrent
// use. Each call owns its attempt count, response metadata and retry lifetime.
type blobBucketLocationRetry struct {
	settings BlobBucketLocationRetrySettings
	now      func() time.Time
	wait     func(context.Context, time.Duration) error
}

// Tests can advance elapsed time without waiting on the wall clock.
func (self *blobBucketLocationRetry) nowUtc() time.Time {
	if self.now != nil {
		return self.now()
	}
	return time.Now()
}

// Backoff is cancellable and uses no process-global timer or retry settings.
func (self *blobBucketLocationRetry) waitRetry(ctx context.Context, delay time.Duration) error {
	if self.wait != nil {
		return self.wait(ctx, delay)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

// A context-local holder captures only the one response header the SDK's
// ErrorResponse omits. No URL, key, credentials or body is copied or changed.
type blobBucketLocationProbe struct {
	retryAfter string
	receivedAt time.Time
	now        func() time.Time
}

// The private key cannot collide with application values in the caller context.
type blobBucketLocationProbeKey struct{}

// This observer never retries, reads, replaces, drains, closes or cancels a
// response body. The SDK remains its sole owner, including on error returns.
type blobBucketLocationProbeTransport struct{ base http.RoundTripper }

// Only explicitly marked preflight requests can retain Retry-After metadata.
func (self *blobBucketLocationProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := self.base.RoundTrip(request)
	if probe, ok := request.Context().Value(blobBucketLocationProbeKey{}).(*blobBucketLocationProbe); ok &&
		request.Method == http.MethodGet &&
		(request.URL.RawQuery == "location" || request.URL.RawQuery == "location=") &&
		response != nil && response.StatusCode == http.StatusTooManyRequests {
		probe.retryAfter = response.Header.Get("Retry-After")
		probe.receivedAt = probe.now()
	}
	return response, err
}

// Wrapped value and pointer errors both occur at owned/test boundaries.
func blobBucketLocationThrottled(err error) bool {
	var response minio.ErrorResponse
	var responsePointer *minio.ErrorResponse
	if !errors.As(err, &response) {
		if !errors.As(err, &responsePointer) || responsePointer == nil {
			return false
		}
		response = *responsePointer
	}
	return response.StatusCode == http.StatusTooManyRequests &&
		(response.Code == "TooManyRequests" || response.Code == "429 Too Many Requests")
}

// Hints can lengthen a wait, never shorten it. An overflowing numeric hint
// refuses an early retry rather than being ignored as a malformed short wait.
func blobBucketLocationRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	digits := true
	for _, character := range value {
		digits = digits && '0' <= character && character <= '9'
	}
	if digits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || uint64(math.MaxInt64/int64(time.Second)) < seconds {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(time.Duration(0), date.Sub(now)), true
}

// A cache hit performs no I/O. Successful discovery warms the same SDK client
// used by the subsequent write, retention query and exact-version read.
func (self *blobBucketLocationRetry) probe(ctx context.Context, client *minio.Client, bucket string) (*blobBucketLocationProbe, error) {
	probe := &blobBucketLocationProbe{now: self.nowUtc}
	_, err := client.GetBucketLocation(context.WithValue(ctx, blobBucketLocationProbeKey{}, probe), bucket)
	return probe, err
}

// Retry only the SDK gap before any upload. The first lookup/body keeps its
// original caller budget; finishing it after the window permits no retries.
// Other errors return control to the unchanged FPutObject path so its
// existing S3-code recovery is preserved;
// that fallback can add one failed discovery read, never an extra object PUT.
func (self *blobBucketLocationRetry) prepare(ctx context.Context, client *minio.Client, bucket string) error {
	if self.settings.MaxAttempts <= 1 || self.settings.RetryWindow <= 0 ||
		self.settings.InitialBackoff <= 0 || self.settings.MaxBackoff <= 0 {
		return nil
	}
	probe, err := self.probe(ctx, client, bucket)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !blobBucketLocationThrottled(err) {
		return nil
	}
	if probe.receivedAt.IsZero() {
		probe.receivedAt = self.nowUtc()
	}
	retryCtx, cancel := context.WithDeadline(ctx, probe.receivedAt.Add(self.settings.RetryWindow))
	defer cancel()
	delay := min(self.settings.InitialBackoff, self.settings.MaxBackoff)
	for attempt := 1; attempt < self.settings.MaxAttempts; attempt++ {
		now := self.nowUtc()
		nextDelay := delay
		responseTime := probe.receivedAt
		if responseTime.IsZero() {
			responseTime = now
		}
		if retryAfter, present := blobBucketLocationRetryAfter(probe.retryAfter, responseTime); present {
			nextDelay = max(nextDelay, responseTime.Add(retryAfter).Sub(now))
		}
		deadline, _ := retryCtx.Deadline()
		if retryCtx.Err() != nil || !now.Before(deadline) || deadline.Sub(now) <= nextDelay {
			return err
		}
		if waitErr := self.waitRetry(retryCtx, nextDelay); waitErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if retryCtx.Err() != nil || !self.nowUtc().Before(deadline) {
			return err
		}
		probe, err = self.probe(retryCtx, client, bucket)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if retryCtx.Err() != nil {
			return err
		}
		if !blobBucketLocationThrottled(err) {
			return nil
		}
		if self.settings.MaxBackoff-delay < delay {
			delay = self.settings.MaxBackoff
		} else {
			delay += delay
		}
	}
	return err
}
