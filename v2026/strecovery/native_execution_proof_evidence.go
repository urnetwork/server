// One refused proof call retains its last physical attempt for attribution.
// Diagnostic bytes are never a proof candidate or independent RPC authority.
package strecovery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const MaximumNativeExecutionProofFailureReplyBytes = 64 * 1024

type NativeExecutionProofReadFailure struct {
	Schema           string `json:"schema"`
	Endpoint         string `json:"endpoint"`
	Method           string `json:"method"`
	RequestId        int    `json:"request_id"`
	Status           int    `json:"http_status"`
	Request          []byte `json:"request_bytes"`
	RequestSha256    string `json:"request_sha256"`
	ReplyPrefix      []byte `json:"reply_prefix_bytes"`
	ReplySha256      string `json:"reply_sha256"`
	ReplyBytes       int    `json:"reply_bytes"`
	ReplyTruncated   bool   `json:"reply_truncated"`
	BodyReadComplete bool   `json:"body_read_complete"`
	Failure          string `json:"failure"`
	Cause            error  `json:"-"`
}

func (self *NativeExecutionProofReadFailure) Error() string {
	return fmt.Sprintf("native proof read method=%s id=%d status=%d reply_bytes=%d reply_sha256=%s: %s", self.Method, self.RequestId, self.Status, self.ReplyBytes, self.ReplySha256, self.Failure)
}

func (self *NativeExecutionProofReadFailure) Unwrap() error { return self.Cause }

// The digest covers all observed bytes. A failed body read does not claim that
// those bytes are the endpoint's complete response, even without prefix loss.
func nativeExecutionProofReadFailure(endpoint, method string, id, status int, request, raw []byte, bodyReadComplete bool, failure string, cause error) error {
	requestHash, replyHash := sha256.Sum256(request), sha256.Sum256(raw)
	// The stage reason is owned text. Formatting an arbitrary transport cause
	// could dispatch a missing receiver or bypass bounded cause traversal.
	if len(failure) > 4096 {
		failure = failure[:4096] + " [truncated]"
	}
	return &NativeExecutionProofReadFailure{
		Schema:   "urnetwork-native-proof-read-failure-v1",
		Endpoint: endpoint, Method: method, RequestId: id, Status: status,
		Request: append([]byte(nil), request...), RequestSha256: hex.EncodeToString(requestHash[:]),
		ReplyPrefix: append([]byte(nil), raw[:min(len(raw), MaximumNativeExecutionProofFailureReplyBytes)]...),
		ReplySha256: hex.EncodeToString(replyHash[:]), ReplyBytes: len(raw),
		ReplyTruncated: len(raw) > MaximumNativeExecutionProofFailureReplyBytes, Cause: cause,
		BodyReadComplete: bodyReadComplete, Failure: failure,
	}
}
