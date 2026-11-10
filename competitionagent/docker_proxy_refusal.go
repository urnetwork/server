//go:build linux

// Keep bounded policy diagnostics private without retaining request contents.
package competitionagent

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Refusal never issues an upstream mutation or gains retry authority. Operation
// admission already bounds receipt count; bytes use this owner's diagnostic cap.
func (self *DockerProxy) persistScopeRefusal(id string, request *http.Request, requestSha256 string, requestBytes int64, refusal error) error {
	reason := strings.ToValidUTF8(refusal.Error(), "\uFFFD")
	truncated := len(reason) > self.settings.MaximumRefusalReasonBytes
	if truncated {
		reason = reason[:self.settings.MaximumRefusalReasonBytes]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
	}
	content, err := json.Marshal(struct {
		Schema          int    `json:"schema"`
		Kind            string `json:"kind"`
		Id              string `json:"id"`
		RunId           string `json:"run_id"`
		Method          string `json:"method"`
		Path            string `json:"path"`
		RequestSha256   string `json:"request_sha256"`
		RequestBytes    int64  `json:"request_bytes"`
		Reason          string `json:"reason"`
		ReasonTruncated bool   `json:"reason_truncated"`
	}{Schema: Schema, Kind: "private-docker-scope-refusal/v1", Id: id, RunId: self.plan.RunId, Method: request.Method, Path: request.URL.Path,
		RequestSha256: requestSha256, RequestBytes: requestBytes, Reason: reason, ReasonTruncated: truncated})
	if err != nil {
		return err
	}
	return atomicFile(self.plan.StateDirectory, "scope-refusal-"+id+".json", content)
}
