package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// SnUploadHeadSummary backs POST /sn/head-summary: the SN25 validator's signed
// head summary of one native tempo (sn docs/spec/head-summary-v1.md). The
// client session is required (401) but authorizes nothing by itself; the
// hotkey signature in the body is the summary's authority
// (controller/sn_head_summary.go). Every accepted summary answers 204.
func SnUploadHeadSummary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	router.WrapRequireClient(router.PrivateErrors(func(clientSession *session.ClientSession) (string, error) {
		ctx, cancel := context.WithTimeout(clientSession.Ctx, 25*time.Second)
		defer cancel()
		if controller.SnHeadSummaryMaxBytes < r.ContentLength {
			controller.ObserveSnHeadSummaryTooLarge()
			return "", errors.New("413 Head summary is over 16 MiB.")
		}
		if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" || len(r.Header.Values("Content-Encoding")) != 0 {
			return "", errors.New("400 Head summary must be an unencoded application/json body.")
		}
		admitted, err := controller.SnHeadSummaryClientAdmission(ctx, clientSession)
		if err != nil {
			return "", router.PrivateCause(http.StatusServiceUnavailable, "Head summary admission unavailable.", err)
		}
		if !admitted {
			w.Header().Set("Retry-After", "60")
			return "", errors.New("429 Head summary uploads are rate limited.")
		}
		if r.Body == nil {
			return "", errors.New("400 Head summary body is empty.")
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, controller.SnHeadSummaryMaxBytes+1))
		if err != nil {
			return "", router.PrivateCause(http.StatusBadRequest, "Incomplete head summary.", err)
		}
		if controller.SnHeadSummaryMaxBytes < len(body) {
			controller.ObserveSnHeadSummaryTooLarge()
			return "", errors.New("413 Head summary is over 16 MiB.")
		}
		result, err := controller.SnUploadHeadSummary(ctx, clientSession, body)
		switch {
		case err == nil:
			return result, nil
		case errors.Is(err, controller.ErrSnHeadSummaryInvalid):
			return "", router.PrivateCause(http.StatusBadRequest, "Head summary is malformed, not canonical, inconsistent or wrongly signed.", err)
		case errors.Is(err, controller.ErrSnHeadSummaryNotAccepted):
			return "", router.PrivateCause(http.StatusForbidden, "The validator hotkey is not accepted for this subnet.", err)
		case errors.Is(err, controller.ErrSnHeadSummaryRateLimited):
			w.Header().Set("Retry-After", "60")
			return "", errors.New("429 Head summary uploads are rate limited.")
		default:
			return "", router.PrivateCause(http.StatusServiceUnavailable, "Head summary storage unavailable.", fmt.Errorf("head summary: %w", err))
		}
	}), w, r, func(_ *session.ClientSession, _ string) bool {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNoContent)
		return true
	})
}
