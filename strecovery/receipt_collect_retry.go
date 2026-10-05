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
	"github.com/urnetwork/server"
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
	return status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504
}

func receiptCollectorRetryable(err error) bool {
	inspection := server.InspectErrorCauses(err)
	if !inspection.Complete {
		return false
	}
	seen := false
	for _, node := range inspection.Nodes {
		if !node.Leaf {
			continue
		}
		cause := node.Err
		seen = true
		// Calling errors.Is here would allow a custom Is method to walk a
		// second unbounded cause tree. Missing receivers never reach a leaf.
		retry := false
		for _, transient := range []error{context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.ETIMEDOUT, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.EPIPE, syscall.EAGAIN, syscall.EINTR, syscall.EMFILE, syscall.ENFILE, syscall.ENOMEM, syscall.EIO} {
			if cause == transient {
				retry = true
				break
			}
		}
		if network, ok := cause.(net.Error); ok {
			retry = network.Timeout() || network.Temporary()
		}
		if !retry {
			return false
		}
	}
	return seen
}
