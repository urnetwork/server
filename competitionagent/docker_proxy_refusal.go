//go:build linux

// Keep bounded policy diagnostics private without retaining request contents.
package competitionagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Native proof details remain in the root owner's bounded receipt. The host
// RPC still returns only its generic refusal code, never tools or proc output.
func (self *DockerProxy) proveHostNetworkWithDiagnostics(ctx context.Context, networkId string) (HostNetworkBoundary, error) {
	proof, err := self.proveHostNetwork(ctx, networkId)
	if err != nil {
		err = errors.Join(err, self.persistHostProofRefusal("verify-network", networkId, err))
	}
	return proof, err
}

// Namespace proofs use the same private diagnostic boundary as network proofs.
func (self *DockerProxy) proveHostNamespaceWithDiagnostics(ctx context.Context, containerId string) (HostNamespaceMap, error) {
	proof, err := self.proveHostNamespace(ctx, containerId)
	if err != nil {
		err = errors.Join(err, self.persistHostProofRefusal("verify-id-map", containerId, err))
	}
	return proof, err
}

// The operation name is fixed by the owner. No request body, environment,
// header, query or plaintext object identity enters the retained diagnostic.
func (self *DockerProxy) persistHostProofRefusal(operation, objectId string, refusal error) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, "http://owner/_urnetwork/host-files/"+operation, nil)
	if err != nil {
		return err
	}
	return self.persistScopeRefusal(hex.EncodeToString(nonce[:]), request, digestDockerBytes([]byte(objectId)), int64(len(objectId)), refusal)
}

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
