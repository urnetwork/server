// A claimed URL turn requires an identity-bearing completion acknowledgement.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe"
)

// Preserves the completed-turn timestamp and claim identity on every retry.
// An old server's successful legacy response is explicitly unsupported.
func (self *Client) ReportUrlProbeCompletion(ctx context.Context, completion qualityprobe.UrlProbeCompletion) error {
	if completion.ClaimOrdinal <= 0 || completion.CompletedAt.IsZero() {
		return fmt.Errorf("ingest: claimed URL completion identity is required")
	}
	completion.ProbeFailure = truncateUtf8(completion.ProbeFailure, MaxProbeFailureLen)
	data, err := json.Marshal(completion)
	if err != nil {
		return err
	}
	url := strings.TrimRight(self.ServerUrl, "/") + "/network/provider-egress-attempt"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-UR-Operator-Secret", self.OperatorSecret)
	response, err := self.httpClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		var receipt struct {
			ClaimOrdinal int64     `json:"claim_ordinal"`
			AttemptAt    time.Time `json:"attempt_at"`
			ReceivedAt   time.Time `json:"received_at"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&receipt); err != nil {
			return err
		}
		if receipt.ClaimOrdinal != completion.ClaimOrdinal || receipt.AttemptAt.IsZero() || receipt.ReceivedAt.IsZero() {
			return qualityprobe.ErrUrlProbeCompletionUnsupported
		}
		return nil
	case http.StatusNotFound:
		return qualityprobe.ErrUrlProbeCompletionUnsupported
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("%w: status %d: %s", ErrRejected, response.StatusCode, strings.TrimSpace(string(message)))
	}
}
