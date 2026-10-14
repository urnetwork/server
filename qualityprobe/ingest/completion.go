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

	"github.com/urnetwork/server/qualityprobe"
)

// The client-side half of ReportUrlProbeCompletion, shared by every transport:
// a completion without its claim identity or completion time is refused before
// any request, and the failure class is cut to the server's column width.
func PrepareUrlProbeCompletion(completion qualityprobe.UrlProbeCompletion) (qualityprobe.UrlProbeCompletion, error) {
	if completion.ClaimOrdinal <= 0 || completion.CompletedAt.IsZero() {
		return completion, fmt.Errorf("ingest: claimed URL completion identity is required")
	}
	completion.ProbeFailure = truncateUtf8(completion.ProbeFailure, MaxProbeFailureLen)
	return completion, nil
}

// What a completion acknowledgement carries back.
type UrlProbeCompletionReceipt struct {
	ClaimOrdinal int64     `json:"claim_ordinal"`
	AttemptAt    time.Time `json:"attempt_at"`
	ReceivedAt   time.Time `json:"received_at"`
}

// An acknowledgement certifies durable completion only when it names this
// claim and carries both server times. An old server's successful legacy
// response does not, and is explicitly unsupported.
func CheckUrlProbeCompletionReceipt(completion qualityprobe.UrlProbeCompletion, receipt UrlProbeCompletionReceipt) error {
	if receipt.ClaimOrdinal != completion.ClaimOrdinal || receipt.AttemptAt.IsZero() || receipt.ReceivedAt.IsZero() {
		return qualityprobe.ErrUrlProbeCompletionUnsupported
	}
	return nil
}

// Preserves the completed-turn timestamp and claim identity on every retry.
// An old server's successful legacy response is explicitly unsupported.
func (self *Client) ReportUrlProbeCompletion(ctx context.Context, completion qualityprobe.UrlProbeCompletion) error {
	completion, err := PrepareUrlProbeCompletion(completion)
	if err != nil {
		return err
	}
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
		var receipt UrlProbeCompletionReceipt
		if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&receipt); err != nil {
			return err
		}
		return CheckUrlProbeCompletionReceipt(completion, receipt)
	case http.StatusNotFound:
		return qualityprobe.ErrUrlProbeCompletionUnsupported
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return RejectedError(response.StatusCode, string(message))
	}
}
