// Native proof and finality reads distinguish a returned contradiction from
// an endpoint that has supplied no usable evidence. Both retain one deadline.
package strecovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/urfoundation/sn/v2026/protocol"
)

var errReceiptCollectorUnavailable = errors.New("native RPC evidence is unavailable")
var errReceiptCollectorCapability = errors.New("native RPC read capability or parameters are refused")

// A gateway object, null identity, missing result or RPC service error is not
// proof about the selected chain. Concrete wrong identities and ambiguous
// complete envelopes still refuse before any transient transport tail.
func decodeNativeReceiptCollectorReply(raw []byte, id int, reply *receiptCollectorReply) (bool, error) {
	if err := protocol.ValidateUniqueJsonKeys(raw); err != nil {
		return false, errors.New("native RPC reply is ambiguous or malformed")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return true, nil
	}
	version, hasVersion := fields["jsonrpc"]
	if !hasVersion {
		return true, nil
	}
	var versionString string
	if json.Unmarshal(version, &versionString) != nil || versionString != "2.0" {
		return false, errors.New("native RPC reply protocol identity differs")
	}
	result, hasResult := fields["result"]
	errorRaw, hasError := fields["error"]
	nonNull := func(value json.RawMessage) bool {
		return len(value) != 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
	}
	hasError = hasError && nonNull(errorRaw)
	if hasResult && nonNull(result) && hasError {
		return false, errors.New("native RPC reply contains both result and error")
	}
	identity, hasIdentity := fields["id"]
	if !hasIdentity || !nonNull(identity) {
		return true, nil
	}
	var observedId int
	if err := json.Unmarshal(identity, &observedId); err != nil || observedId != id {
		return false, errors.New("native RPC reply request identity differs")
	}
	if hasError {
		var failure struct {
			Code *int `json:"code"`
		}
		if json.Unmarshal(errorRaw, &failure) == nil && failure.Code != nil && (*failure.Code == -32601 || *failure.Code == -32602) {
			return false, fmt.Errorf("%w: code %d", errReceiptCollectorCapability, *failure.Code)
		}
		return true, nil
	}
	if !hasResult {
		return true, nil
	}
	*reply = receiptCollectorReply{Version: "2.0", Id: &observedId, Result: result}
	return false, nil
}

// Retry hints never extend the caller's original deadline. The public archive
// can return its seconds hint in a non-JSON-RPC service response.
func receiptCollectorRetryDelay(header http.Header, raw []byte, now, deadline time.Time) time.Duration {
	maximum := max(time.Duration(0), deadline.Sub(now))
	delay := min(time.Second, maximum)
	seconds := func(value string) time.Duration {
		count, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return 0
		}
		if count > uint64(maximum/time.Second) {
			return maximum
		}
		return min(time.Duration(count)*time.Second, maximum)
	}
	for _, value := range header.Values("Retry-After") {
		value = strings.TrimSpace(value)
		delay = max(delay, seconds(value))
		if at, err := http.ParseTime(value); err == nil {
			delay = max(delay, min(at.Sub(now), maximum))
		}
	}
	if protocol.ValidateUniqueJsonKeys(raw) == nil {
		var hint struct {
			Seconds json.Number `json:"retry_after_seconds"`
		}
		if json.Unmarshal(raw, &hint) == nil {
			delay = max(delay, seconds(hint.Seconds.String()))
		}
	}
	return delay
}
