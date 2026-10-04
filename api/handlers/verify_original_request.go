// The exact-request read path uses original signed request bytes and remains
// bounded independently of the optional timestamp-based public index.
package handlers

import (
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
	"net/http"
)

// A missing original remains a null value; it never grants zero exposure.
func GetVerifyOriginalRequest(w http.ResponseWriter, r *http.Request) {
	if !requireVerifyEnabled(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	router.WrapWithInputNoAuth(controller.GetVerifyOriginalRequest, w, r)
}
