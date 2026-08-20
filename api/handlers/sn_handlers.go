package handlers

import (
	"encoding/json"
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
	key, err := startifact.ContentKey(store, hash)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reader, err := store.Get(r.Context(), key)
	if err != nil {
		http.Error(w, "Artifact not found.", http.StatusNotFound)
		return
	}
	defer reader.Close()
	b, err := io.ReadAll(io.LimitReader(reader, 32<<20))
	if err != nil {
		http.Error(w, "Artifact read failed.", http.StatusBadGateway)
		return
	}
	var artifact startifact.Artifact
	if err := json.Unmarshal(b, &artifact); err != nil || startifact.Verify(&artifact) != nil || !strings.EqualFold(artifact.ContentHash, hash) {
		http.Error(w, "Artifact integrity failure.", http.StatusBadGateway)
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
	deployment := cleanArtifactSegment(r.URL.Query().Get("deployment_id"))
	netuid := cleanArtifactSegment(r.URL.Query().Get("netuid"))
	if deployment == "" || netuid == "" {
		http.Error(w, "deployment_id and netuid are required.", http.StatusBadRequest)
		return
	}
	prefix := filepath.ToSlash(filepath.Join(store.Prefix(), "st", "v1", "history", deployment, netuid)) + "/"
	objects, err := store.List(r.Context(), prefix)
	if err != nil {
		http.Error(w, "Artifact history unavailable.", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": "urnetwork-payout-artifact-history-v1", "objects": objects})
}

// SnEvidence accepts and serves the generic signed release history envelope.
// POST authorization is the configured operator artifact signature, checked
// together with chain/deployment/netuid by the controller.
func SnEvidence(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil || len(body) == 0 {
			http.Error(w, "Evidence body is required.", http.StatusBadRequest)
			return
		}
		published, err := controller.StPublishEvidence(r.Context(), body)
		if err != nil {
			http.Error(w, "Evidence rejected.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(published)
		return
	}
	store, ok := server.LoadBlobStore()
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
	b, err := io.ReadAll(io.LimitReader(reader, 64<<20))
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
	store, ok := server.LoadBlobStore()
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
