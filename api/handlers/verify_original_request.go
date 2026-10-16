// The exact-request read path uses original signed request bytes and remains
// bounded independently of the optional timestamp-based public index.
package handlers

import (
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"net/http"
)

// A missing original remains a null value; it never grants zero exposure. A
// request its owner closed is refused 409, and a storage failure is a 503
// without its text (router.PrivateErrorsWithInput).
func GetVerifyOriginalRequest(w http.ResponseWriter, r *http.Request) {
	if !requireVerifyEnabled(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	router.WrapWithInputNoAuth(router.PrivateErrorsWithInput(controller.GetVerifyOriginalRequest), w, r)
}
