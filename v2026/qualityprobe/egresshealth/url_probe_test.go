// URL turns have one observable outcome and leave retry pacing to the server.
package egresshealth

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Passing and failing URL turns each issue exactly one configured request.
func TestUrlProbeRecordsOneOutcomeWithoutHiddenRetries(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		var requests atomic.Int32
		client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			requests.Add(1)
			if !strings.HasSuffix(request.URL.Hostname(), ".example") {
				t.Errorf("unconfigured URL requested: %s", request.URL)
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("synthetic body")), Header: http.Header{}}, nil
		})}
		destinations := []Destination{}
		for i := range 50 {
			destinations = append(destinations, Destination{Name: fmt.Sprintf("synthetic-%d", i), Url: fmt.Sprintf("https://site-%d.example/", i), Class: ClassSite})
		}
		result, err := Check(context.Background(), client, Options{
			UrlProbe: true, Destinations: destinations, Rand: rand.New(rand.NewSource(7)),
			Sleep: func(context.Context, time.Duration) error {
				t.Error("URL turn retained a tunnel while waiting for a retry")
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		wantOk := 0
		if status == http.StatusOK {
			wantOk = 1
		}
		if requests.Load() != 1 || result.Total != 1 || result.OkCount != wantOk || len(result.Checks) != 1 {
			t.Fatalf("status %d: requests=%d ok=%d total=%d checks=%d; expected one retained URL outcome", status, requests.Load(), result.OkCount, result.Total, len(result.Checks))
		}
		if result.UrlProbeEvidence == nil {
			t.Fatal("URL outcome lost its versioned evidence")
		}
		if err := result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
			t.Fatalf("actual measured evidence fails ingress validation: %v", err)
		}
	}
}

// Missing configuration must never cause a request from the legacy table.
func TestUrlProbeRequiresConfiguredDestinations(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("missing catalog opened a request")
		return nil, nil
	})}
	if _, err := Check(context.Background(), client, Options{UrlProbe: true}); err != ErrNoDestinations {
		t.Fatalf("missing URL catalog error = %v", err)
	}
}

// Normal source selection is an equal random draw, separate from the random
// destination draw. Missing country coverage stays an explicit local gap.
func TestUrlProbeDrawsGeneralAndCountrySourcesEqually(t *testing.T) {
	for _, countryAvailable := range []bool{false, true} {
		for ordinal := range 4 {
			seed := int64(ordinal)
			wantHost, wantSource := "general.example", "general"
			country := []Destination(nil)
			if countryAvailable {
				country = []Destination{{Name: "synthetic-country", Url: "https://country.example/", Class: ClassSite}}
				if rand.New(rand.NewSource(seed)).Intn(2) == 1 {
					wantHost, wantSource = "country.example", "country"
				}
			} else {
				wantSource = "general_country_unavailable"
			}
			client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Hostname() != wantHost {
					t.Errorf("ordinal=%d country=%t: host=%s want=%s", ordinal, countryAvailable, request.URL.Hostname(), wantHost)
				}
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("synthetic error")), Header: http.Header{}}, nil
			})}
			result, err := Check(context.Background(), client, Options{
				Rand:     rand.New(rand.NewSource(seed)),
				UrlProbe: true, OutcomeCount: ordinal, CountryDestinations: country,
				Destinations: []Destination{{Name: "synthetic-general", Url: "https://general.example/", Class: ClassSite}},
			})
			if err != nil || result.UrlSource != wantSource || result.Total != 1 || result.OkCount != 0 {
				t.Fatalf("ordinal=%d country=%t: result=%+v error=%v", ordinal, countryAvailable, result, err)
			}
		}
	}
}
