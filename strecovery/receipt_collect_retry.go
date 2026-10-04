// Retry admission traverses every retained cause. One permanent cause wins
// over an adjacent timeout, and owner cancellation is checked by the caller
// after complete returned evidence has been validated.
package strecovery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"syscall"

	"github.com/urfoundation/sn/protocol"
)

var errReceiptCollectorReplyBound = errors.New("receipt collection response exceeds its byte budget")

type receiptCollectorReply struct {
	Version string          `json:"jsonrpc"`
	Id      *int            `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func decodeReceiptCollectorReply(raw []byte, id int, reply *receiptCollectorReply) error {
	if err := protocol.ValidateUniqueJsonKeys(raw); err != nil {
		return errors.New("receipt collection reply is ambiguous or malformed")
	}
	if err := json.Unmarshal(raw, reply); err != nil || reply.Version != "2.0" || reply.Id == nil || *reply.Id != id || (len(reply.Result) == 0) == (len(reply.Error) == 0) {
		return errors.New("receipt collection reply identity or result/error envelope differs")
	}
	return nil
}

func receiptCollectorStatusRetry(status int) bool {
	return status == 408 || status == 429 || status == 502 || status == 503 || status == 504
}

func receiptCollectorRetryable(err error) bool {
	remaining := 128
	var visit func(error, int) bool
	visit = func(cause error, depth int) bool {
		if cause == nil || depth > 32 || remaining == 0 {
			return false
		}
		remaining--
		if many, ok := cause.(interface{ Unwrap() []error }); ok {
			causes := many.Unwrap()
			if len(causes) == 0 || len(causes) > remaining {
				return false
			}
			seen := false
			for _, next := range causes {
				if next == nil {
					continue
				}
				seen = true
				if !visit(next, depth+1) {
					return false
				}
			}
			return seen
		}
		if one, ok := cause.(interface{ Unwrap() error }); ok {
			return visit(one.Unwrap(), depth+1)
		}
		// Calling errors.Is here would allow a custom Is method to walk a
		// second unbounded cause tree. Only the observed leaf grants retry.
		for _, transient := range []error{context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.EPIPE, syscall.EAGAIN, syscall.EINTR, syscall.EMFILE, syscall.ENFILE, syscall.ENOMEM, syscall.EIO} {
			if cause == transient {
				return true
			}
		}
		if network, ok := cause.(net.Error); ok {
			return network.Timeout() || network.Temporary()
		}
		return false
	}
	return visit(err, 0)
}
