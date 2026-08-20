package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Verify backs `POST /verify` (sn/VALIDATOR.md §4). No JWT by design
// (PLAN.md D-7): the protocol is self-authenticating — the body carries the
// validator's client_id and an Ed25519 signature under that client's
// registered key — and unknown callers are answered with indistinguishable
// poisoned trails (§9).
func Verify(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(controller.Verify, w, r)
}

// GetVerifyKeys backs `GET /verify/keys`. Unauthenticated by design: the
// values are the published server Ed25519 public keys, by server_key_id, so
// any third party can verify published trail proofs (VALIDATOR.md §3.5).
func GetVerifyKeys(w http.ResponseWriter, r *http.Request) {
	router.WrapNoAuth(controller.GetVerifyKeys, w, r)
}

func verifyEvidenceArgs(r *http.Request) (*controller.GetVerifyEvidenceArgs, error) {
	args := &controller.GetVerifyEvidenceArgs{}
	var err error
	if raw := r.URL.Query().Get("from"); raw != "" {
		args.From, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, err
		}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		args.To, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, err
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		args.Limit, err = strconv.Atoi(raw)
		if err != nil {
			return nil, err
		}
	}
	return args, nil
}

// GetVerifyStats and GetVerifyProofs are public reproducibility indexes. They
// expose no secret egress hash key and do not participate in validator Q_n.
func GetVerifyStats(w http.ResponseWriter, r *http.Request) {
	args, err := verifyEvidenceArgs(r)
	if err != nil {
		http.Error(w, "Bad evidence query.", http.StatusBadRequest)
		return
	}
	router.WrapNoAuth(func(s *session.ClientSession) (*controller.GetVerifyStatsResult, error) {
		return controller.GetVerifyStats(args, s)
	}, w, r)
}

func GetVerifyProofs(w http.ResponseWriter, r *http.Request) {
	args, err := verifyEvidenceArgs(r)
	if err != nil {
		http.Error(w, "Bad evidence query.", http.StatusBadRequest)
		return
	}
	router.WrapNoAuth(func(s *session.ClientSession) (*controller.GetVerifyProofsResult, error) {
		return controller.GetVerifyProofs(args, s)
	}, w, r)
}
