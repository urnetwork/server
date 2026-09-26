package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/urnetwork/server/qualityprobe/bandwidth"
)

type reserveBandwidthBody struct {
	ClientId  string `json:"client_id"`
	ByteCount int64  `json:"byte_count"`
}

// ReserveBandwidth takes deployment-wide byte budget for one active bandwidth
// measurement, before a single byte is pulled through the provider's tunnel.
//
// Active probe bytes are real, paid contract traffic on any deployment where
// payouts are planned, so they are rationed by an hourly budget server-side.
// The server answers 429 when the current hour's bucket is spent -- it
// deliberately does NOT defer the reservation into a later hour, because this
// prober measures over a tunnel it has open right now and has no use for
// budget it cannot spend. A 429 is therefore a clean skip, reported as
// bandwidth.ErrNoBudget, not a probe failure.
//
// A server with no reservation endpoint (404) answers
// bandwidth.ErrUnsupported, which is likewise a skip: the prober keeps working
// against a deployment that has not shipped these endpoints yet, it just
// records no bandwidth.
func (c *Client) ReserveBandwidth(ctx context.Context, providerClientId string, byteCount int64) error {
	buf, err := json.Marshal(reserveBandwidthBody{ClientId: providerClientId, ByteCount: byteCount})
	if err != nil {
		return err
	}

	url := strings.TrimRight(c.ServerUrl, "/") + "/network/provider-bandwidth-reserve"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-UR-Operator-Secret", c.OperatorSecret)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w (provider %s)", bandwidth.ErrNoBudget, providerClientId)
	case http.StatusNotFound:
		return bandwidth.ErrUnsupported
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%w: status %d: %s", ErrRejected, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}

type submitBandwidthBody struct {
	ClientId        string  `json:"client_id"`
	Source          string  `json:"source"`
	BytesPerSecond  float64 `json:"bytes_per_second"`
	SampleByteCount int64   `json:"sample_byte_count"`
}

// SubmitBandwidth records one measured figure under the source that produced
// it.
//
// source is what keeps the two targets apart in storage: the server's
// provider_bandwidth row is keyed on (client_id, source), so the operator and
// cdn figures for one provider land in separate rows and neither overwrites
// the other. Sending the wrong tag -- or none -- would collapse them into one
// row and lose exactly the divergence the second target exists to expose, so
// the server validates it against its known set and rejects anything else.
func (c *Client) SubmitBandwidth(
	ctx context.Context,
	providerClientId string,
	source string,
	bytesPerSecond float64,
	sampleByteCount int64,
) error {
	buf, err := json.Marshal(submitBandwidthBody{
		ClientId:        providerClientId,
		Source:          source,
		BytesPerSecond:  bytesPerSecond,
		SampleByteCount: sampleByteCount,
	})
	if err != nil {
		return err
	}

	url := strings.TrimRight(c.ServerUrl, "/") + "/network/provider-bandwidth-result"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-UR-Operator-Secret", c.OperatorSecret)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return bandwidth.ErrUnsupported
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%w: status %d: %s", ErrRejected, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}
