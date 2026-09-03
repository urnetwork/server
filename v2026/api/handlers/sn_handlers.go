package handlers

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/startifact"
)

const maximumSnEvidenceBytes = 64 * 1024 * 1024

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
	prefix, err := payoutArtifactHistoryPrefix(
		store.Prefix(),
		r.URL.Query().Get("deployment_id"),
		r.URL.Query().Get("netuid"),
		r.URL.Query().Get("epoch"),
		r.URL.Query().Get("no_id"),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	objects, err := store.List(r.Context(), prefix)
	if err != nil {
		http.Error(w, "Artifact history unavailable.", http.StatusBadGateway)
		return
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	type historyObject struct {
		Key         string `json:"key"`
		Size        int64  `json:"size"`
		ContentHash string `json:"content_hash"`
	}
	publicObjects := make([]historyObject, len(objects))
	for i, object := range objects {
		if object.Size < 0 || !strings.HasPrefix(object.Key, prefix) {
			http.Error(w, "Artifact history integrity failure.", http.StatusBadGateway)
			return
		}
		hash := strings.TrimSuffix(filepath.Base(object.Key), filepath.Ext(object.Key))
		decoded, decodeErr := hex.DecodeString(hash)
		if decodeErr != nil || len(decoded) != 32 || filepath.Ext(object.Key) != ".json" {
			http.Error(w, "Artifact history integrity failure.", http.StatusBadGateway)
			return
		}
		publicObjects[i] = historyObject{Key: object.Key, Size: object.Size, ContentHash: "sha256:" + strings.ToLower(hash)}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": "urnetwork-payout-artifact-history-v1", "objects": publicObjects})
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
		body, err := io.ReadAll(io.LimitReader(r.Body, maximumSnEvidenceBytes))
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
	b, err := io.ReadAll(io.LimitReader(reader, maximumSnEvidenceBytes))
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

func SnEvidenceHistory(w http.ResponseWriter, r *http.Request) {
	store, ok := loadSnEvidenceBlobStore()
	if !ok {
		http.Error(w, "Artifact store unavailable.", http.StatusServiceUnavailable)
		return
	}
	deployment := cleanArtifactSegment(r.URL.Query().Get("deployment_id"))
	netuidValue, err := strconv.ParseUint(r.URL.Query().Get("netuid"), 10, 16)
	if err != nil || netuidValue == 0 {
		http.Error(w, "deployment_id and numeric netuid are required.", http.StatusBadRequest)
		return
	}
	kind := ""
	if raw := r.URL.Query().Get("kind"); raw != "" {
		kind = cleanArtifactSegment(raw)
		if kind == "" {
			http.Error(w, "Bad evidence kind.", http.StatusBadRequest)
			return
		}
	}
	prefix, err := startifact.EvidenceHistoryPrefix(store, deployment, uint16(netuidValue), kind)
	if err != nil {
		http.Error(w, "Bad evidence history identity.", http.StatusBadRequest)
		return
	}
	objects, err := store.List(r.Context(), prefix)
	if err != nil {
		http.Error(w, "Evidence history unavailable.", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": "urnetwork-release-evidence-history-v1", "objects": objects})
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
