package handlers

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/startifact"
)

const maximumSnEvidenceBytes = 64 * 1024 * 1024

const defaultSnHistoryPageObjects = 256

// Swappable boundaries let handler tests force exact storage interleavings
// while production uses the configured controller and blob store.
var (
	publishSnEvidence       = controller.StPublishEvidence
	loadSnEvidenceBlobStore = server.LoadBlobStore
)

// SnSetWallet backs `POST /sn/wallet` (sn/PLAN.md §5): sets the caller
// network's subnet claim coldkey. Network JWT auth; the ss58 format is
// validated in the controller.
func SnSetWallet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SnSetWallet, w, r)
}

// SnPoolClaim backs `GET /sn/pool/claim?epoch=N` (sn/PLAN.md §5): the
// caller network's merkle pool-payout claim. `epoch` is optional and
// defaults to the latest finalized epoch (epoch 0 is a real epoch, so only
// absence defaults). Network JWT auth — a provider fetches its own claim.
func SnPoolClaim(w http.ResponseWriter, r *http.Request) {
	poolClaim := &controller.SnPoolClaimArgs{}
	if epochStr := r.URL.Query().Get("epoch"); epochStr != "" {
		epoch, err := strconv.ParseUint(epochStr, 10, 64)
		if err != nil {
			http.Error(w, "Bad epoch.", http.StatusBadRequest)
			return
		}
		poolClaim.Epoch = &epoch
	}
	impl := func(clientSession *session.ClientSession) (*controller.SnPoolClaimResult, error) {
		return controller.SnPoolClaim(poolClaim, clientSession)
	}
	router.WrapRequireAuth(impl, w, r)
}

// SnEpoch backs `GET /sn/epoch`: the contract epoch clock mirrored from
// chain so clients do not need their own RPC. No auth by design (matching
// the connect binding) — the state is global chain-clock state with nothing
// per-caller in it.
func SnEpoch(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.SnEpoch, w, r)
}

// SnArtifact serves immutable canonical bytes by their sha256 content hash.
// The hash is rechecked before any bytes are returned so a corrupt object
// store cannot silently provide a different payout tree.
func SnArtifact(w http.ResponseWriter, r *http.Request) {
	store, ok := server.LoadBlobStore()
	if !ok {
		http.Error(w, "Artifact store unavailable.", http.StatusServiceUnavailable)
		return
	}
	hash := r.URL.Query().Get("hash")
	if _, err := startifact.ContentKey(store, hash); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	artifact, b, err := startifact.Read(r.Context(), store, hash)
	if err != nil {
		http.Error(w, "Artifact unavailable or failed integrity.", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", `"`+artifact.ContentHash+`"`)
	w.Header().Set("Cache-Control", "public, immutable, max-age=31536000")
	_, _ = w.Write(b)
}

// SnArtifactHistory lists immutable artifact keys under a deployment/netuid
// prefix. It intentionally returns public object identifiers, never signer or
// vault material.
func SnArtifactHistory(w http.ResponseWriter, r *http.Request) {
	store, ok := server.LoadBlobStore()
	if !ok {
		http.Error(w, "Artifact store unavailable.", http.StatusServiceUnavailable)
		return
	}
	epochRaw := r.URL.Query().Get("epoch")
	noIDRaw := r.URL.Query().Get("no_id")
	prefix, err := payoutArtifactHistoryPrefix(
		store.Prefix(),
		r.URL.Query().Get("deployment_id"),
		r.URL.Query().Get("netuid"),
		epochRaw,
		noIDRaw,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, startAfter, err := snHistoryPageQuery(r, prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pagedStore, ok := store.(server.PagedBlobStore)
	if !ok {
		http.Error(w, "Artifact history unavailable.", http.StatusServiceUnavailable)
		return
	}
	objects, more, err := pagedStore.ListPage(r.Context(), prefix, startAfter, limit)
	if err != nil {
		http.Error(w, "Artifact history unavailable.", http.StatusBadGateway)
		return
	}
	if err := validateSnArtifactHistoryPage(objects, more, prefix, startAfter, limit, epochRaw, noIDRaw); err != nil {
		http.Error(w, "Artifact history integrity failure.", http.StatusBadGateway)
		return
	}
	type historyObject struct {
		Key         string `json:"key"`
		Size        int64  `json:"size"`
		ContentHash string `json:"content_hash"`
	}
	publicObjects := make([]historyObject, len(objects))
	for i, object := range objects {
		hash := strings.TrimSuffix(filepath.Base(object.Key), filepath.Ext(object.Key))
		decoded, decodeErr := hex.DecodeString(hash)
		if decodeErr != nil || len(decoded) != 32 || hash != strings.ToLower(hash) || filepath.Ext(object.Key) != ".json" {
			http.Error(w, "Artifact history integrity failure.", http.StatusBadGateway)
			return
		}
		publicObjects[i] = historyObject{Key: object.Key, Size: object.Size, ContentHash: "sha256:" + strings.ToLower(hash)}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	nextAfter := ""
	if more && 0 < len(objects) {
		nextAfter = objects[len(objects)-1].Key
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema":     "urnetwork-payout-artifact-history-v1",
		"objects":    publicObjects,
		"more":       more,
		"next_after": nextAfter,
	})
}

func payoutArtifactHistoryPrefix(blobPrefix, deploymentRaw, netuidRaw, epochRaw, noIDRaw string) (string, error) {
	deployment := cleanArtifactSegment(deploymentRaw)
	if deployment == "" {
		return "", fmt.Errorf("deployment_id is required and must be one safe segment")
	}
	netuid, err := strconv.ParseUint(strings.TrimSpace(netuidRaw), 10, 16)
	if err != nil || netuid == 0 {
		return "", fmt.Errorf("netuid must be a nonzero uint16")
	}
	parts := []string{blobPrefix, "st", "v1", "history", deployment, strconv.FormatUint(netuid, 10)}
	if strings.TrimSpace(epochRaw) == "" {
		if strings.TrimSpace(noIDRaw) != "" {
			return "", fmt.Errorf("no_id requires epoch")
		}
		return filepath.ToSlash(filepath.Join(parts...)) + "/", nil
	}
	epoch, err := strconv.ParseUint(strings.TrimSpace(epochRaw), 10, 64)
	if err != nil {
		return "", fmt.Errorf("epoch must be a uint64")
	}
	parts = append(parts, strconv.FormatUint(epoch, 10))
	if strings.TrimSpace(noIDRaw) != "" {
		noID, noIDErr := strconv.ParseUint(strings.TrimSpace(noIDRaw), 10, 64)
		if noIDErr != nil || noID == 0 {
			return "", fmt.Errorf("no_id must be a nonzero uint64")
		}
		parts = append(parts, strconv.FormatUint(noID, 10))
	}
	return filepath.ToSlash(filepath.Join(parts...)) + "/", nil
}

// SnEvidence accepts and serves the generic signed release history envelope.
// POST authorization is the configured operator artifact signature, checked
// together with chain/deployment/netuid by the controller.
func SnEvidence(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, err := readSnEvidenceBytes(r.Body, maximumSnEvidenceBytes)
		if err != nil || len(body) == 0 {
			http.Error(w, "Evidence body is required.", http.StatusBadRequest)
			return
		}
		published, err := publishSnEvidence(r.Context(), body)
		if err != nil {
			http.Error(w, "Evidence rejected.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(published)
		return
	}
	store, ok := loadSnEvidenceBlobStore()
	if !ok {
		http.Error(w, "Artifact store unavailable.", http.StatusServiceUnavailable)
		return
	}
	hash := r.URL.Query().Get("hash")
	key, err := startifact.EvidenceContentKey(store, hash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reader, err := store.Get(r.Context(), key)
	if err != nil {
		http.Error(w, "Evidence not found.", http.StatusNotFound)
		return
	}
	defer reader.Close()
	b, err := readSnEvidenceBytes(reader, maximumSnEvidenceBytes)
	if err != nil {
		http.Error(w, "Evidence read failed.", http.StatusBadGateway)
		return
	}
	var envelope startifact.EvidenceEnvelope
	if err := json.Unmarshal(b, &envelope); err != nil || startifact.VerifyEvidence(&envelope) != nil || !strings.EqualFold(envelope.ContentHash, hash) {
		http.Error(w, "Evidence integrity failure.", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", `"`+envelope.ContentHash+`"`)
	w.Header().Set("Cache-Control", "public, immutable, max-age=31536000")
	_, _ = w.Write(b)
}

// readSnEvidenceBytes reads one byte beyond the accepted boundary so neither
// an HTTP request nor a corrupt object can be silently accepted after truncation.
func readSnEvidenceBytes(reader io.Reader, maximumBytes int64) ([]byte, error) {
	if maximumBytes <= 0 {
		return nil, errors.New("evidence byte limit must be positive")
	}
	b, err := io.ReadAll(io.LimitReader(reader, maximumBytes+1))
	if err != nil {
		return nil, err
	}
	if maximumBytes < int64(len(b)) {
		return nil, errors.New("evidence exceeds byte limit")
	}
	return b, nil
}

// SnEvidenceHistory exposes one signed run's bounded, lexically paged history.
// Both kind and run id are mandatory so public callers cannot enumerate other
// campaigns or turn a small verifier query into an unbounded deployment scan.
func SnEvidenceHistory(w http.ResponseWriter, r *http.Request) {
	store, ok := loadSnEvidenceBlobStore()
	if !ok {
		http.Error(w, "Artifact store unavailable.", http.StatusServiceUnavailable)
		return
	}
	deployment := r.URL.Query().Get("deployment_id")
	netuidValue, err := strconv.ParseUint(r.URL.Query().Get("netuid"), 10, 16)
	if err != nil || netuidValue == 0 {
		http.Error(w, "deployment_id and numeric netuid are required.", http.StatusBadRequest)
		return
	}
	kind := r.URL.Query().Get("kind")
	runID := r.URL.Query().Get("run_id")
	if kind == "" || runID == "" {
		http.Error(w, "kind and run_id are required.", http.StatusBadRequest)
		return
	}
	prefix, err := startifact.EvidenceHistoryRunPrefix(store, deployment, uint16(netuidValue), kind, runID)
	if err != nil {
		http.Error(w, "Bad evidence history identity.", http.StatusBadRequest)
		return
	}
	limit, startAfter, err := snHistoryPageQuery(r, prefix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var objects []server.BlobObject
	more := false
	contentHash := r.URL.Query().Get("hash")
	if runID == startifact.EvidenceLegacyDeploymentHistoryRunID && contentHash == "" {
		http.Error(w, "legacy deployment history requires an exact evidence hash.", http.StatusBadRequest)
		return
	}
	if contentHash != "" {
		if startAfter != "" {
			http.Error(w, "after is not valid with an exact evidence hash.", http.StatusBadRequest)
			return
		}
		historyKey, keyErr := startifact.EvidenceHistoryKey(store, deployment, uint16(netuidValue), kind, runID, contentHash)
		if keyErr != nil {
			http.Error(w, "Bad evidence content hash.", http.StatusBadRequest)
			return
		}
		reader, getErr := store.Get(r.Context(), historyKey)
		if getErr != nil {
			http.Error(w, "Evidence history object not found.", http.StatusNotFound)
			return
		}
		b, readErr := readSnEvidenceBytes(reader, maximumSnEvidenceBytes)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			http.Error(w, "Evidence history object unavailable.", http.StatusBadGateway)
			return
		}
		var envelope startifact.EvidenceEnvelope
		expectedContentHash := "sha256:" + strings.TrimSuffix(filepath.Base(historyKey), ".json")
		if json.Unmarshal(b, &envelope) != nil || startifact.VerifyEvidence(&envelope) != nil ||
			envelope.DeploymentID != deployment || envelope.Netuid != uint16(netuidValue) || envelope.Kind != kind ||
			envelope.ContentHash != expectedContentHash || !evidenceHistoryRunMatches(envelope.RunID, runID) {
			http.Error(w, "Evidence history object failed integrity.", http.StatusBadGateway)
			return
		}
		objects = []server.BlobObject{{Key: historyKey, Size: int64(len(b))}}
	} else {
		pagedStore, paged := store.(server.PagedBlobStore)
		if !paged {
			http.Error(w, "Evidence history unavailable.", http.StatusServiceUnavailable)
			return
		}
		objects, more, err = pagedStore.ListPage(r.Context(), prefix, startAfter, limit)
		if err != nil {
			http.Error(w, "Evidence history unavailable.", http.StatusBadGateway)
			return
		}
		if err := validateSnEvidenceHistoryPage(objects, more, prefix, startAfter, limit); err != nil {
			http.Error(w, "Evidence history integrity failure.", http.StatusBadGateway)
			return
		}
	}
	nextAfter := ""
	if more && 0 < len(objects) {
		nextAfter = objects[len(objects)-1].Key
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema":     "urnetwork-release-evidence-history-v1",
		"objects":    objects,
		"more":       more,
		"next_after": nextAfter,
	})
}

// evidenceHistoryRunMatches maps the collision-free empty-run namespace back
// to the signed envelope. The legacy word remains readable only by exact hash;
// new empty-run publications use the underscore sentinel.
func evidenceHistoryRunMatches(envelopeRunID string, historyRunID string) bool {
	if historyRunID == startifact.EvidenceDeploymentHistoryRunID {
		return envelopeRunID == ""
	}
	return envelopeRunID == historyRunID || (historyRunID == startifact.EvidenceLegacyDeploymentHistoryRunID && envelopeRunID == "")
}

// validateSnHistoryPage treats the paged store as an integrity boundary for
// ordering and scope. Namespace-specific validators then enforce key grammar.
func validateSnHistoryPage(objects []server.BlobObject, more bool, prefix string, startAfter string, limit int) error {
	if limit < len(objects) || (more && len(objects) != limit) {
		return errors.New("invalid history page cardinality")
	}
	previousKey := startAfter
	for _, object := range objects {
		if object.Size < 0 || object.Key <= previousKey || !strings.HasPrefix(object.Key, prefix) ||
			filepath.ToSlash(filepath.Clean(object.Key)) != object.Key || strings.ContainsAny(object.Key, "\\\r\n\x00") {
			return errors.New("invalid history page object")
		}
		previousKey = object.Key
	}
	return nil
}

// validateSnEvidenceHistoryPage permits exactly one canonical hash filename
// below the already exact deployment/netuid/kind/run prefix.
func validateSnEvidenceHistoryPage(objects []server.BlobObject, more bool, prefix string, startAfter string, limit int) error {
	if err := validateSnHistoryPage(objects, more, prefix, startAfter, limit); err != nil {
		return err
	}
	for _, object := range objects {
		if err := validateSnHistoryHashFilename(strings.TrimPrefix(object.Key, prefix)); err != nil {
			return err
		}
	}
	return nil
}

// validateSnArtifactHistoryPage permits the exact numeric suffix implied by
// optional epoch/no-id selectors before the canonical content hash filename.
func validateSnArtifactHistoryPage(objects []server.BlobObject, more bool, prefix string, startAfter string, limit int, epochRaw string, noIDRaw string) error {
	if err := validateSnHistoryPage(objects, more, prefix, startAfter, limit); err != nil {
		return err
	}
	numericSegmentCount := 2
	if strings.TrimSpace(epochRaw) != "" {
		numericSegmentCount--
	}
	if strings.TrimSpace(noIDRaw) != "" {
		numericSegmentCount--
	}
	for _, object := range objects {
		segments := strings.Split(strings.TrimPrefix(object.Key, prefix), "/")
		if len(segments) != numericSegmentCount+1 {
			return errors.New("invalid artifact history path depth")
		}
		for i := 0; i < numericSegmentCount; i++ {
			value, err := strconv.ParseUint(segments[i], 10, 64)
			if err != nil || segments[i] != strconv.FormatUint(value, 10) || (i == numericSegmentCount-1 && value == 0) {
				return errors.New("invalid artifact history numeric segment")
			}
		}
		if err := validateSnHistoryHashFilename(segments[len(segments)-1]); err != nil {
			return err
		}
	}
	return nil
}

// validateSnHistoryHashFilename accepts the one canonical filename emitted by
// immutable content-addressed evidence and payout artifact publishers.
func validateSnHistoryHashFilename(filename string) error {
	hash := strings.TrimSuffix(filename, ".json")
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != 32 || hash != strings.ToLower(hash) || filename != hash+".json" {
		return errors.New("invalid history page content key")
	}
	return nil
}

// snHistoryPageQuery validates the common bounded-list cursor before either
// public history handler reaches its backing store.
func snHistoryPageQuery(r *http.Request, prefix string) (int, string, error) {
	limit := defaultSnHistoryPageObjects
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || server.MaximumBlobListPageObjects < parsed {
			return 0, "", errors.New("bad artifact history limit")
		}
		limit = parsed
	}
	startAfter := r.URL.Query().Get("after")
	if startAfter != "" && (!strings.HasPrefix(startAfter, prefix) || filepath.ToSlash(filepath.Clean(startAfter)) != startAfter || strings.ContainsAny(startAfter, "\\\r\n\x00")) {
		return 0, "", errors.New("bad artifact history cursor")
	}
	return limit, startAfter, nil
}

func cleanArtifactSegment(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.ContainsAny(v, "/\\.") {
		return ""
	}
	return v
}

// SnGetWallet backs `GET /sn/wallet`: every coldkey attached inside the
// caller's network (network-level and per provider client) and the
// session's effective wallet.
func SnGetWallet(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.SnGetWallet, w, r)
}

// SnValidateWallet backs `POST /sn/wallet/validate`: syntax, chain existence
// and ban-list checks for an address the user is about to attach. No auth —
// the connect flow runs it before the account has a wallet, and the answer
// carries nothing per-account.
func SnValidateWallet(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.SnValidateWallet, w, r)
}

// SnHead backs `GET /sn/head`: the caller network's Top 200 head-tier
// standing (server estimate + binding status).
func SnHead(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.SnHead, w, r)
}

// SnHeadBinding backs `POST /sn/head/binding`: stores a device's fleet
// binding consent and returns the calldata the operator submits themselves.
func SnHeadBinding(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(controller.SnHeadBinding, w, r)
}

// GetAccountEpochs backs `GET /account/epochs?limit=N`: the caller network's
// finalized epochs, newest first, with points and pool share.
func GetAccountEpochs(w http.ResponseWriter, r *http.Request) {
	args := &controller.AccountEpochsArgs{}
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		limit, err := strconv.Atoi(limitStr)
		if err != nil || limit < 0 {
			http.Error(w, "Bad limit.", http.StatusBadRequest)
			return
		}
		args.Limit = limit
	}
	impl := func(clientSession *session.ClientSession) (*controller.AccountEpochsResult, error) {
		return controller.AccountEpochs(args, clientSession)
	}
	router.WrapRequireAuth(impl, w, r)
}
