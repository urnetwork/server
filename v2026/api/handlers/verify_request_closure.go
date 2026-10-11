// Each API lifecycle owns bounded original request-closure intake. A signed
// response leaves the handler only after the durable execution fence commits.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

// Separate route-set owners do not contend on a mutable global admission pool.
type VerifyRequestClosureHandlers struct{ slots chan struct{} }

// Construction owns no background worker and admits at most four operations.
func NewVerifyRequestClosureHandlers() *VerifyRequestClosureHandlers {
	return &VerifyRequestClosureHandlers{slots: make(chan struct{}, 4)}
}

// The original owner has 300 seconds total, including a 60-second native body
// read. Unsupported native deadlines refuse before accepting a closure body.
func (self *VerifyRequestClosureHandlers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !requireVerifyEnabled(w) {
		return
	}
	select {
	case self.slots <- struct{}{}:
		defer func() { <-self.slots }()
	default:
		http.Error(w, "Verification closure busy.", http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.ContentLength > 8192 {
		http.Error(w, "Verification closure exceeds capacity.", http.StatusRequestEntityTooLarge)
		return
	}
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || r.Body == nil || r.ContentLength <= 0 || r.Header.Get("Content-Type") != "application/json" || len(r.Header.Values("Content-Type")) != 1 || len(r.Header.Values("Content-Encoding")) != 0 || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 {
		http.Error(w, "Invalid verification closure framing.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	control := http.NewResponseController(w)
	if err := control.SetWriteDeadline(deadline); err != nil {
		http.Error(w, "Verification closure response unavailable.", http.StatusServiceUnavailable)
		return
	}
	if err := control.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		http.Error(w, "Verification closure read unavailable.", http.StatusServiceUnavailable)
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
	var closure protocol.ProviderAttemptRequestClosure
	if err := json.Unmarshal(raw, &closure); err != nil {
		http.Error(w, "Invalid verification closure encoding.", http.StatusBadRequest)
		return
	}
	canonical, err := json.Marshal(closure)
	if err != nil || !bytes.Equal(canonical, raw) {
		http.Error(w, "Noncanonical verification closure.", http.StatusBadRequest)
		return
	}
	router.WrapNoAuth(func(owner *session.ClientSession) (*model.VerifyRequestClosureResult, error) {
		return controller.CloseVerifyOriginalRequest(&closure, owner)
	}, w, r.WithContext(ctx))
}
