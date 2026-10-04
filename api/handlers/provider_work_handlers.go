// Each API lifecycle owns finite public SDK transport admission. Signatures
// authenticate originals; receipt custody never grants roster completeness.
package handlers

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// One instance belongs to one route set. A refused caller never enters a
// waiting queue or retains its potentially large cut body in application memory.
type ProviderWorkHandlers struct{ slots chan struct{} }

// Construction creates no goroutines and shares no mutable process budget.
func NewProviderWorkHandlers() *ProviderWorkHandlers {
	return &ProviderWorkHandlers{slots: make(chan struct{}, 4)}
}

// Exact fixed-size public selectors reject alternative spellings and zero ids.
func providerWorkSelector(value string, output []byte) error {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != len(output) || hex.EncodeToString(raw) != value || bytes.Equal(raw, make([]byte, len(raw))) {
		return model.ErrProviderWorkInvalid
	}
	copy(output, raw)
	return nil
}

// One finite owner covers body interruption, signature verification and the
// actual database commit. Response deadlines also cover bodyless public reads.
func (self *ProviderWorkHandlers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	select {
	case self.slots <- struct{}{}:
		defer func() { <-self.slots }()
	default:
		http.Error(w, "Provider work transport busy.", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil {
		http.Error(w, "Provider work response owner unavailable.", http.StatusServiceUnavailable)
		return
	}
	domain, approver, err := controller.LoadProviderWorkPolicy()
	if err != nil {
		http.Error(w, "Provider work authority unavailable.", http.StatusServiceUnavailable)
		return
	}
	if r.URL.RawPath != "" && r.URL.RawPath != r.URL.Path {
		providerWorkHttpError(w, model.ErrProviderWorkInvalid)
		return
	}
	if r.URL.Path == "/provider-work/v1/owners" {
		self.serveOwner(ctx, w, r, domain)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/provider-work/v1/windows" {
		names := []string{"domain", "epoch", "artifact"}
		if _, exists := r.URL.Query()["authority"]; exists {
			names = append(names, "authority")
		}
		query, err := providerWorkQuery(r, names)
		if err != nil {
			providerWorkHttpError(w, err)
			return
		}
		var selectedDomain, artifactHash, authorityHash [32]byte
		if providerWorkSelector(query["domain"], selectedDomain[:]) != nil || providerWorkSelector(query["artifact"], artifactHash[:]) != nil || query["authority"] != "" && providerWorkSelector(query["authority"], authorityHash[:]) != nil {
			providerWorkHttpError(w, model.ErrProviderWorkInvalid)
			return
		}
		epoch, err := strconv.ParseUint(query["epoch"], 10, 64)
		if err != nil || strconv.FormatUint(epoch, 10) != query["epoch"] {
			providerWorkHttpError(w, model.ErrProviderWorkInvalid)
			return
		}
		raw, err := controller.ProviderWorkWindow(ctx, selectedDomain, epoch, artifactHash, authorityHash)
		if err != nil {
			providerWorkHttpError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/provider-work/v1/requests" {
		query, err := providerWorkQuery(r, []string{"domain", "client", "generation", "key"})
		if err != nil {
			providerWorkHttpError(w, err)
			return
		}
		var owner model.ProviderWorkOwner
		for _, field := range []struct {
			value  string
			output []byte
		}{{value: query["domain"], output: owner.DomainHash[:]}, {value: query["client"], output: owner.ClientId[:]}, {value: query["generation"], output: owner.Generation[:]}, {value: query["key"], output: owner.PublicKey[:]}} {
			if err := providerWorkSelector(field.value, field.output); err != nil {
				providerWorkHttpError(w, err)
				return
			}
		}
		if owner.DomainHash != domain {
			providerWorkHttpError(w, model.ErrProviderWorkInvalid)
			return
		}
		requests, err := model.ListProviderWorkRequests(ctx, owner, approver, time.Now())
		if err != nil {
			providerWorkHttpError(w, err)
			return
		}
		providerWorkJson(w, requests)
		return
	}
	if len(r.URL.RawQuery) != 0 {
		providerWorkHttpError(w, model.ErrProviderWorkInvalid)
		return
	}
	if r.Method == http.MethodGet {
		cut := strings.HasPrefix(r.URL.Path, "/provider-work/v1/cuts/")
		prefix := "/provider-work/v1/requests/"
		if cut {
			prefix = "/provider-work/v1/cuts/"
		}
		var digest [32]byte
		if !strings.HasPrefix(r.URL.Path, prefix) || providerWorkSelector(strings.TrimPrefix(r.URL.Path, prefix), digest[:]) != nil {
			providerWorkHttpError(w, model.ErrProviderWorkInvalid)
			return
		}
		raw, err := model.GetProviderWorkOriginal(ctx, digest, cut)
		if err != nil {
			providerWorkHttpError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"`+hex.EncodeToString(digest[:])+`"`)
		w.Header().Set("Cache-Control", "public, immutable, max-age=31536000")
		_, _ = w.Write(raw)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/provider-work/v1/requests" && r.URL.Path != "/provider-work/v1/cuts" && r.URL.Path != "/provider-work/v1/authorities" {
		http.Error(w, "Provider work route unavailable.", http.StatusMethodNotAllowed)
		return
	}
	maximum := int64(protocol.MaximumOriginalWorkRequestBytes)
	if r.URL.Path == "/provider-work/v1/cuts" {
		maximum = protocol.MaximumOriginalWorkSubmissionBytes
	}
	if r.URL.Path == "/provider-work/v1/authorities" {
		maximum = payoutartifact.MaxWholeWorkAuthorityBytes
	}
	if r.ContentLength > maximum {
		http.Error(w, "Provider work body exceeds capacity.", http.StatusRequestEntityTooLarge)
		return
	}
	if r.Body == nil || r.ContentLength <= 0 || r.Header.Get("Content-Type") != "application/json" || len(r.Header.Values("Content-Type")) != 1 || len(r.Header.Values("Content-Encoding")) != 0 || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 {
		providerWorkHttpError(w, model.ErrProviderWorkInvalid)
		return
	}
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		http.Error(w, "Provider work body owner unavailable.", http.StatusServiceUnavailable)
		return
	}
	body, err := ownSnAttemptUploadBody(ctx, w, r.Body)
	if err != nil {
		providerWorkHttpError(w, err)
		return
	}
	raw, err := readProviderWorkBody(ctx, body, r.ContentLength)
	if err != nil {
		providerWorkHttpError(w, err)
		return
	}
	if r.URL.Path == "/provider-work/v1/authorities" {
		domain, approver, expected, err := controller.LoadProviderWorkAuthorityPolicy()
		if err != nil {
			providerWorkHttpError(w, err)
			return
		}
		digest, err := model.RetainProviderWorkAuthority(ctx, raw, domain, approver, expected.AuthoritySigner)
		if err != nil {
			providerWorkHttpError(w, err)
			return
		}
		providerWorkJson(w, struct {
			AuthorityHash [32]byte `json:"authority_hash"`
		}{AuthorityHash: digest})
		return
	}
	if r.URL.Path == "/provider-work/v1/requests" {
		digest, err := model.RetainProviderWorkRequest(ctx, raw, approver, domain, time.Now())
		if err != nil {
			providerWorkHttpError(w, err)
			return
		}
		providerWorkJson(w, struct {
			RequestHash [32]byte `json:"request_hash"`
		}{RequestHash: digest})
		return
	}
	var submission protocol.OriginalWorkCutSubmission
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&submission); err != nil {
		providerWorkHttpError(w, model.ErrProviderWorkInvalid)
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		providerWorkHttpError(w, model.ErrProviderWorkInvalid)
		return
	}
	canonical, err := json.Marshal(submission)
	if err != nil || !bytes.Equal(canonical, raw) {
		providerWorkHttpError(w, model.ErrProviderWorkInvalid)
		return
	}
	receipt, err := model.RetainProviderWorkCut(ctx, submission, approver, domain)
	if err != nil {
		providerWorkHttpError(w, err)
		return
	}
	providerWorkJson(w, receipt)
}

// Duplicate, missing and unknown selectors must not select another operation.
func providerWorkQuery(r *http.Request, names []string) (map[string]string, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, model.ErrProviderWorkInvalid
	}
	if len(query) != len(names) {
		return nil, model.ErrProviderWorkInvalid
	}
	result := make(map[string]string, len(names))
	for _, name := range names {
		values := query[name]
		if len(values) != 1 || values[0] == "" {
			return nil, model.ErrProviderWorkInvalid
		}
		result[name] = values[0]
	}
	return result, err
}

// Cancellation interrupts native I/O and joins Close before bytes reach SQL.
func readProviderWorkBody(ctx context.Context, body io.ReadCloser, size int64) (raw []byte, resultErr error) {
	if ctx == nil || body == nil || size <= 0 || size > protocol.MaximumOriginalWorkSubmissionBytes {
		return nil, model.ErrProviderWorkInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, body.Close())
	}
	closed := make(chan struct{})
	var closeErr error
	stop := context.AfterFunc(ctx, func() { defer close(closed); closeErr = body.Close() })
	defer func() {
		if stop() {
			closeErr = body.Close()
		} else {
			<-closed
		}
		resultErr = errors.Join(resultErr, closeErr, ctx.Err())
		if resultErr != nil {
			raw = nil
		}
	}()
	raw, resultErr = io.ReadAll(io.LimitReader(body, size+1))
	if resultErr == nil && int64(len(raw)) != size {
		resultErr = model.ErrProviderWorkInvalid
	}
	return
}

// An unavailable observation is retryable HTTP state; only returned signature,
// identity, framing or immutable-byte contradictions receive permanent refusal.
func providerWorkHttpError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, model.ErrProviderWorkInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, model.ErrProviderWorkConflict), errors.Is(err, payoutartifact.ErrClosedWorkIntegrity):
		status = http.StatusConflict
	case errors.Is(err, model.ErrProviderWorkMissing), errors.Is(err, payoutartifact.ErrClosedWorkUnavailable):
		status = http.StatusNotFound
	case errors.Is(err, model.ErrProviderWorkCapacity), errors.Is(err, payoutartifact.ErrClosedWorkCapacity):
		status = http.StatusTooManyRequests
	}
	http.Error(w, "Provider work operation unavailable or refused.", status)
}

// Encoding precedes response commitment so a partial object is never acknowledged.
func providerWorkJson(w http.ResponseWriter, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		providerWorkHttpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}
