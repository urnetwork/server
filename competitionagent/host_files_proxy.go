//go:build linux

// A finite local RPC connects the trusted non-root helper to the independent
// root file owner. Complete bounded input and a synced intent precede mutation.
package competitionagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// The worker UID was authenticated by the original Unix peer credentials in
// workerHandler. No peer is given a mount executable or a general root shell.
func (self *DockerProxy) hostFilesRequestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || self.hostFiles == nil {
		http.Error(w, "host-file role unavailable", http.StatusForbidden)
		return
	}
	release, err := self.admit(false)
	if err != nil {
		http.Error(w, "host-file admission closed", http.StatusServiceUnavailable)
		return
	}
	defer release()
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		http.Error(w, "host-file custody unavailable", http.StatusServiceUnavailable)
		return
	}
	id := hex.EncodeToString(idBytes)
	bodyPath := filepath.Join(self.bodyDirectory(), id+".request")
	deadline := self.settings.Now().Add(self.settings.UploadTimeout)
	if self.plan.DeadlineAt.Before(deadline) {
		deadline = self.plan.DeadlineAt
	}
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	reader := io.Reader(http.NoBody)
	if r.Body != nil {
		reader = r.Body
	}
	size, digest, err := self.sealBody(r.Context(), reader, bodyPath, self.settings.MaximumDocumentBytes)
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if err != nil || r.ContentLength >= 0 && size != r.ContentLength {
		http.Error(w, "host-file input incomplete", http.StatusBadRequest)
		return
	}
	content, err := readBounded(bodyPath, self.settings.MaximumDocumentBytes)
	request := HostFilesRequest{}
	if err != nil || validateDocumentKeys(content) != nil || decodeStrict(content, &request) != nil {
		http.Error(w, "host-file input rejected", http.StatusBadRequest)
		return
	}
	var validationErr error
	if request.Operation == "retire-build" {
		validationErr = self.validateBuildRetirementRequest(request)
	} else {
		_, validationErr = self.hostFiles.validateRequest(request)
	}
	if validationErr != nil {
		http.Error(w, "host-file scope rejected", http.StatusForbidden)
		return
	}
	if !self.settings.Now().Before(self.plan.DeadlineAt) {
		http.Error(w, "original host-file issue deadline elapsed", http.StatusRequestTimeout)
		return
	}
	operation := &dockerProxyOperation{Schema: Schema, Id: id, RunId: self.plan.RunId, Kind: "host-files", Method: http.MethodPost,
		Uri: "/_urnetwork/host-files", State: "issued", RequestSha256: digest, RequestBytes: size}
	if err := self.storeOperation(operation); err != nil {
		http.Error(w, "host-file intent unavailable", http.StatusServiceUnavailable)
		return
	}
	self.stateLock.Lock()
	self.operationsByIds[id] = operation
	self.stateLock.Unlock()
	if self.beforeIssue != nil {
		self.beforeIssue()
	}
	if !self.settings.Now().Before(self.plan.DeadlineAt) {
		operation.StatusCode = http.StatusRequestTimeout
		self.finishUnissuedOperation(operation)
		http.Error(w, "original host-file issue deadline elapsed after intent commit", http.StatusRequestTimeout)
		return
	}
	fileCtx, fileCancel := context.WithTimeout(self.ownerCtx, self.hostFiles.settings.OperationTimeout)
	defer fileCancel()
	ctx, cancel := context.WithDeadline(fileCtx, self.plan.DeadlineAt)
	defer cancel()
	result := HostFilesResult{RunId: self.plan.RunId, Operation: request.Operation, Acknowledged: true, ExitCode: 2, ErrorCode: "host_handoff_unavailable"}
	if request.Operation == "retire-build" {
		result = self.retireBuild(ctx)
	} else if request.Operation == "verify-id-map" || request.Operation == "verify-network" {
		result = self.hostFiles.Execute(ctx, request)
	} else if release, err := self.beginHostFileHandoff(ctx); err == nil {
		defer release()
		result = self.hostFiles.Execute(ctx, request)
	}
	response, marshalErr := json.Marshal(result)
	acknowledged := marshalErr == nil && int64(len(response)) <= self.settings.MaximumDocumentBytes
	if acknowledged {
		acknowledged = self.reserveSpool(int64(len(response))) == nil && exclusiveFile(filepath.Join(self.bodyDirectory(), id+".response"), response, 0600) == nil &&
			self.syncSpoolDirectory(self.bodyDirectory()) == nil
	}
	operation.StatusCode, operation.ResponseBytes, operation.ResponseSha256 = http.StatusOK, int64(len(response)), digestDockerBytes(response)
	self.finishOperation(operation, acknowledged && result.Acknowledged)
	if !acknowledged {
		http.Error(w, "host-file receipt could not be retained", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response)
}

// No late create/start may race a filesystem handoff after an empty census.
// This temporary barrier joins every previously admitted upload/mutation;
// root build-session drain remains allowed, and stop cleanup never takes it.
func (self *DockerProxy) beginHostFileHandoff(ctx context.Context) (func(), error) {
	return self.beginHostFileHandoffExceptRetirementWaiters(ctx, false)
}

// Only retirement can exempt its already-admitted duplicate observers. Those
// requests still count in ordinary handoffs and final owner quiescence, so a
// receipt-write/disconnect cannot make the owner forget a live request.
func (self *DockerProxy) beginHostFileHandoffExceptRetirementWaiters(ctx context.Context, retirement bool) (func(), error) {
	self.stateLock.Lock()
	if self.fileHandoff || !self.accepting || self.unknown != 0 {
		self.stateLock.Unlock()
		return nil, errors.New("host-file handoff has unresolved daemon custody")
	}
	self.fileHandoff = true
	self.notifyWithLock()
	self.stateLock.Unlock()
	release := func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.fileHandoff = false
		self.notifyWithLock()
	}
	if self.beforeHandoffWait != nil {
		self.beforeHandoffWait()
	}
	for {
		self.stateLock.Lock()
		changed, inflight, unknown, accepting := self.changed, self.inflight, self.unknown, self.accepting
		if retirement {
			inflight -= self.retirementWaiters
		}
		self.stateLock.Unlock()
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		if !accepting || unknown != 0 || self.ownerCtx.Err() != nil {
			release()
			return nil, errors.New("host-file handoff cannot pass uncertain or stopped daemon work")
		}
		// The host-files request itself owns the remaining admission.
		if inflight == 1 {
			return release, nil
		}
		select {
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// Filesystem handoff/unmount never races an already-running owned container.
// The trusted helper joins its entire cohort before requesting reclaim.
func (self *DockerProxy) requireNoRunContainers(ctx context.Context, _ string) error {
	filters, err := json.Marshal(map[string][]string{"label": {cleanupJobLabel + "=" + self.plan.JobId, cleanupRoundLabel + "=" + self.plan.RoundId, cleanupRunLabel + "=" + self.plan.RunId}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil)
	if err != nil {
		return err
	}
	response, err := self.transport.RoundTrip(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, self.settings.MaximumDocumentBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || int64(len(content)) > self.settings.MaximumDocumentBytes || validateDockerMutationJson(content, self.settings.MaximumDocumentDepth) != nil {
		return errors.New("owned container census is unavailable")
	}
	var containers []json.RawMessage
	if json.Unmarshal(content, &containers) != nil || containers == nil || len(containers) != 0 {
		return errors.New("host-file handoff requires all owned containers to have joined and been removed")
	}
	return nil
}

// The public non-root client can wait for only a typed finite local operation.
// A transport failure is never an instruction to replay a native mutation.
func requestHostFiles(ctx context.Context, socket string, request HostFilesRequest, settings DockerProxySettings) (HostFilesResult, error) {
	content, err := json.Marshal(request)
	if err != nil || int64(len(content)) > settings.MaximumDocumentBytes {
		return HostFilesResult{}, errors.New("host-file request exceeds its bound")
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://owner/_urnetwork/host-files", strings.NewReader(string(content)))
	if err != nil {
		return HostFilesResult{}, err
	}
	response, err := transport.RoundTrip(httpRequest)
	if err != nil {
		return HostFilesResult{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, settings.MaximumDocumentBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || int64(len(body)) > settings.MaximumDocumentBytes || validateDocumentKeys(body) != nil {
		return HostFilesResult{}, errors.New("host-file response remains unknown")
	}
	result := HostFilesResult{}
	if decodeStrict(body, &result) != nil || result.RunId != request.RunId || result.Operation != request.Operation || result.ExitCode < 0 || result.ExitCode > 255 || !result.Acknowledged {
		return HostFilesResult{}, ErrDockerProxyUnknown
	}
	return result, nil
}
