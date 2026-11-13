package handlers

// The capture the flag routes and the document of sn/api/operator.yml. Their
// conventions (one Access-Control-Allow-Origin, problem details, Link, ETag)
// are the controller's (controller/sn_operator_api.go).

import (
	"net/http"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/router"
)

// SnCtf backs `GET /sn/ctf`: the current state of capture the flag in every
// country. Public.
func SnCtf(w http.ResponseWriter, r *http.Request) {
	controller.SnCtf(w, r)
}

// SnCtfHere backs `GET /sn/ctf/here`: the caller's country from its address.
// Public; nothing about the request is stored.
func SnCtfHere(w http.ResponseWriter, r *http.Request) {
	controller.SnCtfHere(w, r)
}

// SnCtfCountry backs `GET /sn/ctf/country/{cc}`: one country's state and its
// history. Public.
func SnCtfCountry(w http.ResponseWriter, r *http.Request) {
	controller.SnCtfCountry(w, r, pathValue(r, 0))
}

// SnCtfTop backs `GET /sn/ctf/top`: the largest open pots. Public.
func SnCtfTop(w http.ResponseWriter, r *http.Request) {
	controller.SnCtfTop(w, r)
}

// SnCtfEpoch backs `GET /sn/ctf/epoch/{epoch}`: one closed epoch's census,
// draw, amounts, awards and pots. Public.
func SnCtfEpoch(w http.ResponseWriter, r *http.Request) {
	controller.SnCtfEpoch(w, r, pathValue(r, 0))
}

// SnCtfClient backs `GET /sn/ctf/client`: a client token's own client's status
// and wins, or the wins of every client of a network credential's network.
func SnCtfClient(w http.ResponseWriter, r *http.Request) {
	controller.SnCtfClient(w, r)
}

// SnOpenApi backs `GET /sn/openapi.yml`: sn/api/operator.yml, as this release
// was built with it. Public.
func SnOpenApi(w http.ResponseWriter, r *http.Request) {
	controller.SnOpenApi(w, r)
}

func pathValue(r *http.Request, i int) string {
	values := router.GetPathValues(r)
	if i < len(values) {
		return values[i]
	}
	return ""
}
