package handlers

import (
	"net/http"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// ServicesContactSales is the public Services contact form (EMBED1.md): no
// authentication, rate limited with 429.
func ServicesContactSales(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(model.ServicesContactSales, w, r)
}
