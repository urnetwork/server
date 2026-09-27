// Cheap liveness releases retry waits without skipping sampled TLS identities.
package egresshealth

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A passing URL must not retain its provider slot while unrelated ordinary
// failures wait minutes for evidence which cannot change an already-known pass.
func TestBlackholeLivenessReleasesOrdinaryRetryWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		waiting := make(chan struct{})
		var waits, requests atomic.Int32
		client := &http.Client{Transport: echoStageRoundTripper(func(req *http.Request) (*http.Response, error) {
			requests.Add(1)
			if req.URL.Path != "/good" {
				return nil, errors.New("synthetic ordinary failure")
			}
			<-waiting
			return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		opts := Options{Budget: time.Hour, PerRequestTimeout: 10 * time.Second, LoadAttempts: 3,
			Sleep: func(ctx context.Context, _ time.Duration) error {
				if waits.Add(1) == 2 {
					close(waiting)
				}
				<-ctx.Done()
				return ctx.Err()
			},
		}
		dests := []Destination{
			{Name: "good", Class: ClassConnectivity, Url: "https://one.example/good", Expect: ExpectStatus, Status: 204},
			{Name: "ordinary-one", Class: ClassConnectivity, Url: "https://two.example/fail", Expect: ExpectStatus, Status: 204},
			{Name: "ordinary-two", Class: ClassConnectivity, Url: "https://three.example/fail", Expect: ExpectStatus, Status: 204},
		}
		start := time.Now()
		result := blackhole(context.Background(), client, dests, opts)
		if !result.Ok || requests.Load() != 3 || waits.Load() != 2 || time.Since(start) != 0 {
			t.Fatalf("known pass retained retry slots: ok=%t requests=%d waits=%d held=%s", result.Ok, requests.Load(), waits.Load(), time.Since(start))
		}
	})
}

// A later retry already in flight remains an integrity observation even when
// another URL passes. Liveness never cancels that request to hide its verdict.
func TestBlackholeLivenessWaitsForInflightRetryTlsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		retryStarted, releaseTls := make(chan struct{}), make(chan struct{})
		var candidateRequests atomic.Int32
		client := &http.Client{Transport: echoStageRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/candidate" {
				if candidateRequests.Add(1) == 1 {
					return nil, errors.New("synthetic transient failure")
				}
				close(retryStarted)
				select {
				case <-releaseTls:
					return nil, x509.UnknownAuthorityError{}
				case <-req.Context().Done():
					t.Error("liveness canceled an admitted integrity observation")
					return nil, req.Context().Err()
				}
			}
			<-retryStarted
			return &http.Response{StatusCode: 204, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		dests := []Destination{
			{Name: "good-one", Class: ClassConnectivity, Url: "https://one.example/good", Expect: ExpectStatus, Status: 204},
			{Name: "candidate", Class: ClassConnectivity, Url: "https://two.example/candidate", Expect: ExpectStatus, Status: 204},
			{Name: "good-two", Class: ClassConnectivity, Url: "https://three.example/good", Expect: ExpectStatus, Status: 204},
		}
		done := make(chan *BlackholeResult, 1)
		go func() {
			done <- blackhole(context.Background(), client, dests, Options{LoadAttempts: 3, Sleep: noSleep})
		}()
		<-retryStarted
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("known pass skipped the in-flight TLS retry")
		default:
		}
		close(releaseTls)
		result := <-done
		if result.Ok || result.Failure != FailureTlsAuthentication || candidateRequests.Load() != 2 {
			t.Fatalf("in-flight TLS failure hidden: ok=%t failure=%s requests=%d", result.Ok, result.Failure, candidateRequests.Load())
		}
	})
}
