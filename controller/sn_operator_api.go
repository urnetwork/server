package controller

// sn_operator_api.go — the conventions of the routes sn/api/operator.yml
// documents (GET /sn/ctf…, GET /sn/ledger/account and GET /sn/openapi.yml):
//   - exactly one Access-Control-Allow-Origin on every answer;
//   - RFC 9457 problem details for every error the api answers itself;
//   - the Link to the document on every 200 but the document's own;
//   - a strong ETag of the exact body, 304 on If-None-Match, and
//     Last-Modified from the answer's as_of.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	snapi "github.com/urfoundation/sn/api"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// SnOperatorLink is the Link of every 200 answer of the document's routes but
// the document itself.
const SnOperatorLink = `</sn/openapi.yml>; rel="service-desc"; type="application/yaml"`

// The problem codes of sn/api/operator.yml (Problem.code).
const (
	SnProblemBadRequest        = "bad_request"
	SnProblemUnauthorized      = "unauthorized"
	SnProblemUnknownCountry    = "unknown_country"
	SnProblemUnknownEpoch      = "unknown_epoch"
	SnProblemUnavailable       = "unavailable"
	SnProblemLedgerUnavailable = "ledger_unavailable"
)

// SnProblem is an RFC 9457 problem details object with the document's code.
type SnProblem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// WriteSnProblem answers an error as application/problem+json. The caller has
// set the Access-Control-Allow-Origin.
func WriteSnProblem(w http.ResponseWriter, status int, code string, detail string) {
	body, err := json.Marshal(&SnProblem{Type: "about:blank", Title: http.StatusText(status), Status: status, Code: code, Detail: detail})
	if err != nil {
		http.Error(w, router.UnexpectedErrorMessage, http.StatusInternalServerError)
		return
	}
	header := w.Header()
	header.Set("Content-Type", "application/problem+json")
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// The service whose cors_origins the load balancer in front of these routes
// answers for, when the process does not name its own.
const snOperatorDefaultService = "api"

// How long the load balancer's origin allowlist is kept before it is read
// again from the services configuration.
const snOperatorCorsOriginsTtl = 10 * time.Minute

// The services configuration, as far as the allowlist goes: the load balancer
// renders each service's cors_origins (or the cors_origins of the service its
// cors_origins_from names) from the active version, versions[0].
type snOperatorServicesYaml struct {
	Versions []struct {
		Services map[string]struct {
			CorsOrigins     []string `yaml:"cors_origins"`
			CorsOriginsFrom string   `yaml:"cors_origins_from"`
		} `yaml:"services"`
	} `yaml:"versions"`
}

// snOperatorCorsOriginsOf is the allowlist of a service in a services
// configuration, following cors_origins_from.
func snOperatorCorsOriginsOf(services *snOperatorServicesYaml, service string) []string {
	if len(services.Versions) == 0 {
		return nil
	}
	configured := services.Versions[0].Services
	seen := map[string]bool{}
	for !seen[service] {
		seen[service] = true
		entry, ok := configured[service]
		if !ok {
			return nil
		}
		if entry.CorsOriginsFrom == "" {
			return entry.CorsOrigins
		}
		service = entry.CorsOriginsFrom
	}
	return nil
}

var snOperatorCorsOriginsCache struct {
	lock     sync.Mutex
	origins  []string
	known    bool
	readTime time.Time
}

// snOperatorCorsOrigins is the load balancer's origin allowlist for this
// process's service: the origins whose requests it answers with their own
// origin in Access-Control-Allow-Origin. It is read from the same services
// configuration the load balancer is rendered from, so the two never
// disagree, and a read that fails keeps the last good list. known is false
// until a read succeeds. Tests replace it.
var snOperatorCorsOrigins = func() (origins []string, known bool) {
	cache := &snOperatorCorsOriginsCache
	cache.lock.Lock()
	defer cache.lock.Unlock()
	now := time.Now()
	if !cache.readTime.IsZero() && now.Sub(cache.readTime) < snOperatorCorsOriginsTtl {
		return cache.origins, cache.known
	}
	cache.readTime = now
	service, err := server.Service()
	if err != nil {
		service = snOperatorDefaultService
	}
	server.HandleError(func() {
		resource, err := server.Vault.SimpleResource("services.yml")
		if err != nil {
			glog.Infof("[sn]no services configuration, so the load balancer's origins are unknown: %s\n", err)
			return
		}
		var services snOperatorServicesYaml
		if err := resource.UnmarshalYamlE(&services); err != nil {
			glog.Errorf("[sn]services configuration does not parse, so the load balancer's origins are unknown: %s\n", err)
			return
		}
		cache.origins, cache.known = snOperatorCorsOriginsOf(&services, service), true
	})
	return cache.origins, cache.known
}

// SnOperatorCors sets the answer's one Access-Control-Allow-Origin: "*",
// unless the load balancer answers for the request's origin itself, which it
// does for the origins on its allowlist (and for every origin when the list
// holds "*"). Two values would make a browser refuse the answer, so while the
// allowlist is unknown the api sends none: the operator's own sites still read
// the answer through the load balancer's.
func SnOperatorCors(w http.ResponseWriter, r *http.Request) {
	origins, known := snOperatorCorsOrigins()
	if !known || snOperatorOriginAnsweredByLb(r.Header.Get("Origin"), origins) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

func snOperatorOriginAnsweredByLb(origin string, allowlist []string) bool {
	if slices.Contains(allowlist, "*") {
		return true
	}
	return origin != "" && slices.Contains(allowlist, origin)
}

// The format of an answer's times: RFC 3339 in UTC, to the second.
const snOperatorTimeFormat = "2006-01-02T15:04:05Z"

func snOperatorTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(snOperatorTimeFormat)
}

// writeSnCacheableJson writes a JSON answer with its Link, its Cache-Control,
// a strong ETag of the exact body and, when asOf is set, Last-Modified; a
// request whose If-None-Match names the tag is answered 304 with the same
// headers and no body. clientSession scopes the tag to the caller's network,
// nil for a public answer. The caller has set the Access-Control-Allow-Origin.
func writeSnCacheableJson(w http.ResponseWriter, r *http.Request, clientSession *session.ClientSession, cacheControl string, asOf time.Time, result any) {
	body, err := json.Marshal(result)
	if err != nil {
		glog.Infof("[sn]operator answer encoding error (%T): %s\n", result, err)
		WriteSnProblem(w, http.StatusServiceUnavailable, SnProblemUnavailable, "The answer could not be encoded.")
		return
	}
	header := w.Header()
	etag := router.JsonEntityTag(clientSession, r, r.URL.RawQuery, body)
	header.Set("ETag", etag)
	header.Set("Cache-Control", cacheControl)
	if !asOf.IsZero() {
		header.Set("Last-Modified", asOf.UTC().Format(http.TimeFormat))
	}
	if router.IfNoneMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	header.Set("Link", SnOperatorLink)
	header.Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// The Cache-Control of the document.
const snOpenApiCacheControl = "public, max-age=3600"

// The document's strong ETag, a digest of its bytes, which never change
// within a release.
var snOpenApiEntityTag = sync.OnceValue(func() string {
	sum := sha256.Sum256(snapi.OperatorYml)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
})

// SnOpenApi serves GET /sn/openapi.yml: sn/api/operator.yml, byte for byte as
// this release was built with it, so it describes the routes this server
// serves.
func SnOpenApi(w http.ResponseWriter, r *http.Request) {
	SnOperatorCors(w, r)
	header := w.Header()
	etag := snOpenApiEntityTag()
	header.Set("ETag", etag)
	header.Set("Cache-Control", snOpenApiCacheControl)
	if router.IfNoneMatch(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	header.Set("Content-Type", "application/yaml")
	_, _ = w.Write(snapi.OperatorYml)
}
