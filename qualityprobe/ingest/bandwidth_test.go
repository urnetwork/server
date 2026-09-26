package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/qualityprobe/bandwidth"
)

// TestReserveBandwidthMapsStatusToOutcome pins the reservation contract. The
// 429 case is the one that matters most: the server returns it when the
// current hour's byte bucket is spent, and the prober must treat that as a
// clean skip (bandwidth.ErrNoBudget) rather than as a probe failure -- and
// certainly rather than measuring anyway.
func TestReserveBandwidthMapsStatusToOutcome(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{name: "reserved", status: http.StatusOK, body: `{"reservation_id":"019f8835-158d-6fd8-e9dd-fd0e4c6d6792","bucket_start":"2026-07-31T10:00:00Z"}`},
		{name: "hourly budget spent", status: http.StatusTooManyRequests, body: "budget reached", wantErr: bandwidth.ErrNoBudget},
		{name: "server has no endpoint", status: http.StatusNotFound, body: "", wantErr: bandwidth.ErrUnsupported},
		{name: "wrong secret", status: http.StatusUnauthorized, body: "", wantErr: ErrUnauthorized},
		{name: "server error", status: http.StatusInternalServerError, body: "boom", wantErr: ErrRejected},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotMethod, gotPath, gotSecret string
			var gotBody reserveBandwidthBody
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				gotSecret = r.Header.Get("X-UR-Operator-Secret")
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &gotBody)
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			client := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret", Http: srv.Client()}
			err := client.ReserveBandwidth(context.Background(), "provider-1", bandwidth.MaxSampleBytes)

			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("ReserveBandwidth err = %v, want nil", err)
				}
			} else if !errors.Is(err, c.wantErr) {
				t.Fatalf("ReserveBandwidth err = %v, want %v", err, c.wantErr)
			}

			if gotMethod != http.MethodPost {
				t.Errorf("method = %s, want POST", gotMethod)
			}
			if gotPath != "/network/provider-bandwidth-reserve" {
				t.Errorf("path = %q", gotPath)
			}
			if gotSecret != "s3cret" {
				t.Errorf("operator secret header = %q", gotSecret)
			}
			if gotBody.ClientId != "provider-1" || gotBody.ByteCount != bandwidth.MaxSampleBytes {
				t.Errorf("body = %+v, want the provider and the per-probe byte count", gotBody)
			}
		})
	}
}

// TestSubmitBandwidthCarriesTheSource: the source tag is what keeps the two
// targets in separate rows server-side (the row is keyed on
// (client_id, source)). A submission without it, or with the wrong one,
// collapses both targets onto one row and loses the divergence signal.
func TestSubmitBandwidthCarriesTheSource(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{name: "operator target", source: bandwidth.SourceOperator},
		{name: "cdn target", source: bandwidth.SourceCdn},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath string
			var gotBody submitBandwidthBody
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &gotBody)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			client := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret", Http: srv.Client()}
			if err := client.SubmitBandwidth(context.Background(), "provider-1", c.source, 12_345_678, 5*1024*1024); err != nil {
				t.Fatalf("SubmitBandwidth err = %v", err)
			}

			if gotPath != "/network/provider-bandwidth-result" {
				t.Errorf("path = %q", gotPath)
			}
			if gotBody.Source != c.source {
				t.Errorf("source = %q, want %q", gotBody.Source, c.source)
			}
			if gotBody.ClientId != "provider-1" || gotBody.BytesPerSecond != 12_345_678 || gotBody.SampleByteCount != 5*1024*1024 {
				t.Errorf("body = %+v", gotBody)
			}
		})
	}
}

// TestSubmitBandwidthMapsStatusToOutcome: a server without the endpoint is a
// skip, not a hard failure, for the same reason as the reservation.
func TestSubmitBandwidthMapsStatusToOutcome(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{name: "stored", status: http.StatusOK},
		{name: "server has no endpoint", status: http.StatusNotFound, wantErr: bandwidth.ErrUnsupported},
		{name: "wrong secret", status: http.StatusUnauthorized, wantErr: ErrUnauthorized},
		{name: "rejected", status: http.StatusBadRequest, wantErr: ErrRejected},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
			}))
			defer srv.Close()

			client := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret", Http: srv.Client()}
			err := client.SubmitBandwidth(context.Background(), "provider-1", bandwidth.SourceCdn, 1, 1)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("SubmitBandwidth err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("SubmitBandwidth err = %v, want %v", err, c.wantErr)
			}
		})
	}
}
