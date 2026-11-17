package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The lag.dev drift test (lag.dev/DRIFTTEST.md P, E1-E3, K3, K5, W8). An
// Embed customer's backend allocates a token for a visitor's address and
// reads the verdict with its network credential (P2, P4). The widget, a
// lag.dev frame on the customer's page, starts each run with the token alone,
// measures it against the extenders the server chose (E2), finishes it, and
// reads what it shows (P5). The verdict comes from the extenders' signed
// reports (A7, E5), never from figures the browser supplies.
//
// A token is 32 random bytes and is stored only as its SHA-256 (K5). The
// visitor's address is stored only as the peppered hash of its /29 (/56),
// until no run can start (K3, R1); each start's address is compared with it
// in memory and goes no further than its track's client fields (A5).

// A token's text: the prefix and the base64url of 32 random bytes (P2).
const (
	lagDriftTokenPrefix = "dt1_"
	lagDriftTokenBytes  = 32
)

// The token anyone may start a run with, which stores nothing and is never
// verified (W8).
const LagDriftDemoToken = "demo"

// Where the widget is framed from (W1).
const lagDriftEmbedUrlPrefix = "https://lag.dev/?t="

// An allocation's bounds (P2).
const (
	lagDriftMaxReference     = 128
	lagDriftDefaultTtl       = 300 * time.Second
	lagDriftMinTtl           = 60 * time.Second
	lagDriftMaxTtl           = 3600 * time.Second
	lagDriftMaxParentOrigins = 16
	lagDriftMaxOriginLength  = 256
)

// The drift routes' refusals beyond the lag API's limit and unavailable
// (lag_session_controller.go).
const (
	// the network is not Embed-enabled (decision 3)
	LagDriftErrorCodeEmbed = "embed"
	// no drift test of the caller's network has that id
	LagDriftErrorCodeNotFound = "not_found"
	// the token is unknown, or its first run did not start in time
	LagDriftErrorCodeExpired = model.LagDriftRefusalExpired
	// the token is out of runs, past its retest window, or failed
	LagDriftErrorCodeClosed = model.LagDriftRefusalClosed
	// another run of the token is still measuring
	LagDriftErrorCodeBusy = model.LagDriftRefusalBusy
	// the frame's parent origin is not allowed (W5, P3)
	LagDriftErrorCodeOrigin = "origin"
	// an operator switched the network's drift tests off (P3)
	LagDriftErrorCodeDisabled = "disabled"
	// the request is invalid: a 400
	LagDriftErrorCodeInvalid = "invalid"
	// a client token on a route that takes the network's credential: a 403,
	// which the route's class answers first (route_authz.go)
	LagDriftErrorCodeClientToken = "client_token"
)

// The rate limits' names (lag_rate_limit.go): allocations per network, runs
// and demo runs per /24 (/48) (P7, W8).
const (
	lagDriftAllocateRateLimitName = "drift_allocate"
	lagDriftStartRateLimitName    = "drift_start"
	lagDriftDemoRateLimitName     = "drift_demo"
)

// A drift test's states (P4).
const (
	LagDriftStateCreated = "created"
	LagDriftStateActive  = "active"
	LagDriftStateClosed  = "closed"
	LagDriftStateExpired = "expired"
	LagDriftStateFailed  = "failed"
)

// The browser's reasons to end a run (V10).
const (
	LagDriftStopConverged   = "converged"
	LagDriftStopClear       = "clear"
	LagDriftStopInterrupted = "interrupted"
	LagDriftStopCap         = "cap"
)

var lagDriftStops = []string{
	LagDriftStopConverged,
	LagDriftStopClear,
	LagDriftStopInterrupted,
	LagDriftStopCap,
}

func lagDriftRefusal(code string, message string) *LagError {
	return &LagError{Code: code, Message: message}
}

// A refusal of a network route (P5): an http status, 403 for a network
// without Embed, 404 for an unknown drift test and 429 for the allocation
// limit, whose body is the refusal as the widget routes answer theirs,
// `{"error": {"code", "message", "retry_after_ms"}}`, with Retry-After for a
// limit (router.RaiseHttpError).
type lagDriftStatusError struct {
	status   int
	lagError *LagError
}

func (self *lagDriftStatusError) Error() string {
	return fmt.Sprintf("%d %s", self.status, self.lagError.Message)
}

func (self *lagDriftStatusError) HttpErrorResultBody() any {
	return &struct {
		Error *LagError `json:"error"`
	}{Error: self.lagError}
}

func (self *lagDriftStatusError) RetryAfterSeconds() int {
	return int(math.Ceil(float64(self.lagError.RetryAfterMs) / 1000))
}

// An invalid request: 400, with the refusal's body.
func lagDriftInvalid(format string, args ...any) error {
	return &lagDriftStatusError{
		status:   http.StatusBadRequest,
		lagError: lagDriftRefusal(LagDriftErrorCodeInvalid, fmt.Sprintf(format, args...)),
	}
}

func lagDriftLimited(limit model.LagRateLimitResult, message string) *LagError {
	return &LagError{
		Code:         LagErrorCodeLimit,
		Message:      message,
		RetryAfterMs: limit.RetryAfter.Milliseconds(),
	}
}

// The checks every network route makes first: an authenticated network, the
// Embed gate before any other work, as for the data-cap and ACL-group routes
// (decision 3, P1; model/network_embed_model.go), then the network credential
// itself, the root token or an API key, neither of which carries a client.
// The route's class refuses a client token before the handler runs
// (route_authz.go); this guards a caller that reaches the controller another
// way.
func lagDriftNetworkRefusal(clientSession *session.ClientSession) error {
	if clientSession.ByJwt == nil || clientSession.ByJwt.NetworkId == (server.Id{}) {
		return fmt.Errorf("401 A drift test requires the network's root token or an API key.")
	}
	if !model.NetworkEmbedEnabled(clientSession.Ctx, clientSession.ByJwt.NetworkId) {
		return &lagDriftStatusError{
			status:   http.StatusForbidden,
			lagError: lagDriftRefusal(LagDriftErrorCodeEmbed, model.NetworkEmbedNotEnabledMessage),
		}
	}
	if clientSession.ByJwt.ClientId != nil {
		return &lagDriftStatusError{
			status:   http.StatusForbidden,
			lagError: lagDriftRefusal(LagDriftErrorCodeClientToken, "A drift test requires the network's root token or an API key."),
		}
	}
	return nil
}

// A new token's text (P2): the prefix and the base64url, unpadded, of 32
// random bytes, 47 characters in all.
func newLagDriftToken() string {
	tokenBytes := make([]byte, lagDriftTokenBytes)
	if _, err := rand.Read(tokenBytes); err != nil {
		panic(err)
	}
	return lagDriftTokenPrefix + base64.RawURLEncoding.EncodeToString(tokenBytes)
}

// The SHA-256 a token is stored as (K5), or false for a text that is not a
// token's form.
func lagDriftTokenHash(token string) ([]byte, bool) {
	encoded, ok := strings.CutPrefix(token, lagDriftTokenPrefix)
	if !ok {
		return nil, false
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(tokenBytes) != lagDriftTokenBytes {
		return nil, false
	}
	digest := sha256.Sum256([]byte(token))
	return digest[:], true
}

// The hosts a developer's local page is served from, which may frame the
// widget over http (W5, W6).
var lagDriftLocalHosts = []string{"localhost", "127.0.0.1"}

// A parent origin as compared (W5, P3): scheme and host in lower case, the
// default port dropped. "" and `null` are an unknown origin, answered as ""
// with ok. A drift test is framed from https origins, and for development
// from http://localhost and http://127.0.0.1 on any port. Strictly, the text
// must be an origin; otherwise, as a frame's referrer can be, a page's url,
// whose origin is taken.
func lagDriftParseOrigin(text string, strict bool) (origin string, ok bool) {
	text = strings.TrimSpace(text)
	if text == "" || text == "null" {
		return "", true
	}
	if lagDriftMaxOriginLength < len(text) && strict {
		return "", false
	}
	parsed, err := url.Parse(text)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return "", false
	}
	if strict && (parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery) {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	port := parsed.Port()
	switch {
	case scheme == "https":
		if port == "443" {
			port = ""
		}
	case scheme == "http" && slices.Contains(lagDriftLocalHosts, host):
		if port == "80" {
			port = ""
		}
	default:
		return "", false
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	origin = scheme + "://" + host
	if port != "" {
		origin += ":" + port
	}
	if lagDriftMaxOriginLength < len(origin) {
		return "", false
	}
	return origin, true
}

// Normalizes a list of origins strictly, without repeats, for an allocation
// or a network's list (P3), or explains what is wrong with it.
func lagDriftParseOrigins(texts []string) ([]string, error) {
	origins := []string{}
	for _, text := range texts {
		origin, ok := lagDriftParseOrigin(text, true)
		if !ok || origin == "" {
			return nil, fmt.Errorf("%q is not an https origin, such as https://example.com, or a local http one", text)
		}
		if !slices.Contains(origins, origin) {
			origins = append(origins, origin)
		}
	}
	if lagDriftMaxParentOrigins < len(origins) {
		return nil, fmt.Errorf("at most %d origins", lagDriftMaxParentOrigins)
	}
	return origins, nil
}

// ParseLagDriftOrigins normalizes a network's parent origins as the api
// compares them (P3), for `bringyourctl network drift-test`.
func ParseLagDriftOrigins(texts []string) ([]string, error) {
	return lagDriftParseOrigins(texts)
}

// Whether a frame whose parent is `origin`, "" when unknown, may start a run
// (W5, P3): it must be in the token's list when the token narrowed one, and
// in the network's when the network set one. Without either list, any https
// origin, a local http one and an unknown one are accepted (Q5).
func lagDriftOriginAllowed(origin string, tokenOrigins []string, networkOrigins []string) bool {
	for _, origins := range [][]string{tokenOrigins, networkOrigins} {
		if origins != nil && !slices.Contains(origins, origin) {
			return false
		}
	}
	return true
}

// A time as the drift routes answer it: RFC 3339, whole seconds, utc.
func lagDriftTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func lagDriftOptionalTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	text := lagDriftTime(*t)
	return &text
}

// Allocates a drift test (P2).
type LagDriftTestArgs struct {
	// the visitor's address as the customer's server sees it (K3)
	ClientIp string `json:"client_ip"`
	// narrows the network's allowed parent origins (P3)
	ParentOrigins []string `json:"parent_origins,omitempty"`
	// 0.95, 0.99 or 0.999 (V6)
	Confidence *float64 `json:"confidence,omitempty"`
	// at most 128 characters, returned as given
	Reference string `json:"reference,omitempty"`
	// until the first run must start: 60 to 3600, default 300
	TtlSeconds *int `json:"ttl_seconds,omitempty"`
}

// A new drift test. A refusal is an http status with the refusal as its body
// (lagDriftStatusError).
type LagDriftTestResult struct {
	DriftTestId server.Id `json:"drift_test_id"`
	Token       string    `json:"token"`
	// the widget's frame source (W1)
	EmbedUrl string `json:"embed_url"`
	// RFC 3339: the first run must start before this
	ExpiresAt string `json:"expires_at"`
}

// Answers `POST /lag/drift-test` (P2).
func LagDriftTestAllocate(args *LagDriftTestArgs, clientSession *session.ClientSession) (*LagDriftTestResult, error) {
	result, err := lagDriftTestAllocate(args, clientSession, server.NowUtc(), GetLagSettings())
	countLagDriftAllocate(result, err)
	return result, err
}

func lagDriftTestAllocate(
	args *LagDriftTestArgs,
	clientSession *session.ClientSession,
	now time.Time,
	settings *LagSettings,
) (*LagDriftTestResult, error) {
	if err := lagDriftNetworkRefusal(clientSession); err != nil {
		return nil, err
	}
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	drift := &settings.Drift
	if !drift.On() {
		// the operator's kill switch (lag.yml drift.enabled)
		return nil, &lagDriftStatusError{
			status:   http.StatusServiceUnavailable,
			lagError: lagDriftRefusal(LagErrorCodeUnavailable, "Drift tests are not available now."),
		}
	}
	network := model.GetLagDriftNetwork(ctx, networkId)
	if network.Disabled {
		return nil, &lagDriftStatusError{
			status:   http.StatusForbidden,
			lagError: lagDriftRefusal(LagDriftErrorCodeDisabled, "Drift tests are switched off for this network."),
		}
	}

	limit, err := model.CheckLagDriftNetworkRateLimit(
		ctx,
		lagDriftAllocateRateLimitName,
		networkId,
		drift.AllocateLimit,
		drift.AllocateLimitWindow,
	)
	if err != nil {
		return nil, err
	}
	if !limit.Allowed {
		return nil, &lagDriftStatusError{
			status:   http.StatusTooManyRequests,
			lagError: lagDriftLimited(limit, "Too many drift tests allocated by this network. Try again shortly."),
		}
	}

	if args == nil {
		return nil, lagDriftInvalid("client_ip is required.")
	}
	clientIp, err := netip.ParseAddr(strings.TrimSpace(args.ClientIp))
	if err != nil {
		// the parse error would quote the address, so it goes no further
		// (amendment 9)
		return nil, lagDriftInvalid("client_ip is required: the visitor's address as your server sees it.")
	}
	clientIp = clientIp.Unmap().WithZone("")
	if clientIp.IsUnspecified() || clientIp.IsMulticast() {
		return nil, lagDriftInvalid("client_ip is not a visitor's address.")
	}

	var parentOrigins []string
	if args.ParentOrigins != nil {
		parentOrigins, err = lagDriftParseOrigins(args.ParentOrigins)
		if err != nil {
			return nil, lagDriftInvalid("parent_origins: %s.", err)
		}
		// a token narrows the network's list, never widens it (P3)
		if networkOrigins := network.ParentOrigins; networkOrigins != nil {
			for _, origin := range parentOrigins {
				if !slices.Contains(networkOrigins, origin) {
					return nil, lagDriftInvalid("parent_origins: %s is not among the network's drift test origins.", origin)
				}
			}
		}
	}

	confidence := drift.ConfidenceDefault
	if args.Confidence != nil {
		confidence = *args.Confidence
		if !lagDriftConfidenceAllowed(confidence) {
			return nil, lagDriftInvalid("confidence is 0.95, 0.99 or 0.999.")
		}
	}
	if lagDriftMaxReference < utf8.RuneCountInString(args.Reference) || !utf8.ValidString(args.Reference) {
		return nil, lagDriftInvalid("reference is at most %d characters.", lagDriftMaxReference)
	}
	ttl := lagDriftDefaultTtl
	if args.TtlSeconds != nil {
		ttl = time.Duration(*args.TtlSeconds) * time.Second
		if ttl < lagDriftMinTtl || lagDriftMaxTtl < ttl {
			return nil, lagDriftInvalid("ttl_seconds is %d to %d.", int(lagDriftMinTtl.Seconds()), int(lagDriftMaxTtl.Seconds()))
		}
	}

	// the country every run is judged against: client_ip's own, never a
	// start address's (K3, review F-10)
	clientIpPlace := lagDriftStartPlace(clientIp)
	token := newLagDriftToken()
	tokenHash, _ := lagDriftTokenHash(token)
	// the binding is to the visitor's network, not the exact address: the
	// peppered hash of its /29 (IPv4) or /56 (IPv6), as ClientIpHashForAddr
	// hashes it, since addresses churn within a block through CGNAT pools and
	// IPv6 privacy addresses (K3)
	clientIpHash := server.ClientIpHashForAddr(clientIp)
	// whole seconds, so the expiry stored is the one answered
	expiresAt := now.Add(ttl).Truncate(time.Second)
	driftTestId := server.NewId()
	model.CreateLagDriftTest(ctx, &model.LagDriftTest{
		DriftTestId:         driftTestId,
		NetworkId:           networkId,
		TokenHash:           tokenHash,
		ClientIpHash:        clientIpHash[:],
		ClientIpFamily:      server.IpVersionForAddr(clientIp),
		ClientIpCountryCode: clientIpPlace.CountryCode,
		ClientIpCountryName: clientIpPlace.CountryName,
		ParentOrigins:       parentOrigins,
		Confidence:          confidence,
		Reference:           args.Reference,
		CreatedAt:           now,
		ExpiresAt:           expiresAt,
	})
	return &LagDriftTestResult{
		DriftTestId: driftTestId,
		Token:       token,
		EmbedUrl:    lagDriftEmbedUrlPrefix + token,
		ExpiresAt:   lagDriftTime(expiresAt),
	}, nil
}

// Reads a drift test (P4).
type LagDriftTestGetArgs struct {
	DriftTestId string `json:"drift_test_id"`
}

// One run as the customer reads it (P4).
type LagDriftRunView struct {
	Run         int     `json:"run"`
	State       string  `json:"state"`
	StartedAt   string  `json:"started_at"`
	CompletedAt *string `json:"completed_at,omitempty"`
	// from the sessions' opening to their close, as the reports show it
	MeasuredMs *int `json:"measured_ms,omitempty"`
	// the browser's reason to end the run (V10)
	Stop *string `json:"stop,omitempty"`
	// null when the run started from client_ip's other family (K3)
	ClientIpMatches *bool `json:"client_ip_matches"`
	// the extenders the run was ticketed with, spares apart
	Extenders int `json:"extenders"`
	// the extenders that answered, once the run is complete
	Answered *int `json:"answered,omitempty"`
	// whether the run is in the current verdict's window (V7)
	InVerdict bool `json:"in_verdict"`
	// once complete: counted, or why the run stays out of the window
	Outcome string `json:"outcome,omitempty"`
}

// A drift test as the customer reads it (P4).
type LagDriftTestView struct {
	DriftTestId    server.Id          `json:"drift_test_id"`
	Reference      string             `json:"reference,omitempty"`
	State          string             `json:"state"`
	Confidence     float64            `json:"confidence"`
	CreatedAt      string             `json:"created_at"`
	ExpiresAt      string             `json:"expires_at"`
	FirstStartedAt *string            `json:"first_started_at"`
	RetestUntil    *string            `json:"retest_until"`
	UpdatedAt      string             `json:"updated_at"`
	Runs           []*LagDriftRunView `json:"runs"`
	// null until a run has completed; each completed run replaces it
	Result *LagDriftResult `json:"result"`
}

// Answers `GET /lag/drift-test?drift_test_id=…` (P4). A refusal is an http
// status with the refusal as its body (lagDriftStatusError).
func LagDriftTestGet(args *LagDriftTestGetArgs, clientSession *session.ClientSession) (*LagDriftTestView, error) {
	return lagDriftTestGet(args, clientSession, server.NowUtc(), GetLagSettings())
}

func lagDriftTestGet(
	args *LagDriftTestGetArgs,
	clientSession *session.ClientSession,
	now time.Time,
	settings *LagSettings,
) (*LagDriftTestView, error) {
	if err := lagDriftNetworkRefusal(clientSession); err != nil {
		return nil, err
	}
	ctx := clientSession.Ctx
	driftTestId, err := server.ParseId(strings.TrimSpace(args.DriftTestId))
	if err != nil {
		return nil, lagDriftInvalid("drift_test_id is required.")
	}
	test := model.GetLagDriftTest(ctx, driftTestId)
	if test == nil || test.NetworkId != clientSession.ByJwt.NetworkId {
		return nil, &lagDriftStatusError{
			status:   http.StatusNotFound,
			lagError: lagDriftRefusal(LagDriftErrorCodeNotFound, "No drift test of this network has that id."),
		}
	}
	test, runs := advanceLagDriftTest(ctx, test, now, settings)
	return newLagDriftTestView(test, runs, now, &settings.Drift), nil
}

// A drift test's state at `now` (P4): created until its first run, expired if
// that never came in time or the token was revoked first, active while a run
// can start or is still being processed, closed once neither holds, and
// failed after an internal error.
func lagDriftTestState(test *model.LagDriftTest, runs []*model.LagDriftRun, now time.Time, drift *LagDriftSettings) string {
	switch {
	case test.FailedAt != nil:
		return LagDriftStateFailed
	case test.RunCount == 0 && test.RevokeTime == nil && now.Before(test.ExpiresAt):
		return LagDriftStateCreated
	case test.RunCount == 0:
		return LagDriftStateExpired
	}
	for _, run := range runs {
		if !run.Complete() {
			return LagDriftStateActive
		}
	}
	if lagDriftRunsLeft(test, now, drift) == 0 {
		return LagDriftStateClosed
	}
	return LagDriftStateActive
}

// The runs a token can still start at `now`: none once it has failed or was
// revoked, is out of runs, is past its retest window, or never started before
// its expiry.
func lagDriftRunsLeft(test *model.LagDriftTest, now time.Time, drift *LagDriftSettings) int {
	switch {
	case test.FailedAt != nil || test.RevokeTime != nil:
		return 0
	case test.RunCount == 0 && !now.Before(test.ExpiresAt):
		return 0
	case test.RetestUntil != nil && !now.Before(*test.RetestUntil):
		return 0
	}
	return max(drift.MaxRuns-test.RunCount, 0)
}

// The newest run whose verdict is not in yet, or nil (P4).
func lagDriftPendingRun(runs []*model.LagDriftRun) *model.LagDriftRun {
	for i := len(runs) - 1; 0 <= i; i -= 1 {
		if !runs[i].Complete() {
			return runs[i]
		}
	}
	return nil
}

// The main extenders a run was ticketed with, its spares apart (E2).
func lagDriftRunMainExtenders(run *model.LagDriftRun, drift *LagDriftSettings) int {
	count := 0
	for _, extender := range run.Extenders {
		if extender.Position < drift.Extenders {
			count += 1
		}
	}
	return count
}

// The stored verdict of a test, nil before a run has completed.
func lagDriftStoredResult(test *model.LagDriftTest) *LagDriftResult {
	if len(test.ResultJson) == 0 {
		return nil
	}
	result := &LagDriftResult{}
	if err := json.Unmarshal(test.ResultJson, result); err != nil {
		glog.Errorf("[lag]drift test %s has an unreadable result: %s\n", test.DriftTestId, err)
		return nil
	}
	// a slice stored as null, as Go writes a nil one, reads as empty: P4's
	// arrays are never null
	if result.RunsInVerdict == nil {
		result.RunsInVerdict = []int{}
	}
	if result.Drift != nil && result.Drift.LastMileMs == nil {
		result.Drift.LastMileMs = []float64{}
	}
	return result
}

func newLagDriftTestView(test *model.LagDriftTest, runs []*model.LagDriftRun, now time.Time, drift *LagDriftSettings) *LagDriftTestView {
	result := lagDriftStoredResult(test)
	inVerdict := map[int]bool{}
	if result != nil {
		for _, run := range result.RunsInVerdict {
			inVerdict[run] = true
		}
		if pending := lagDriftPendingRun(runs); pending != nil {
			result.PendingRun = &pending.Run
		}
	}
	view := &LagDriftTestView{
		DriftTestId:    test.DriftTestId,
		Reference:      test.Reference,
		State:          lagDriftTestState(test, runs, now, drift),
		Confidence:     test.Confidence,
		CreatedAt:      lagDriftTime(test.CreatedAt),
		ExpiresAt:      lagDriftTime(test.ExpiresAt),
		FirstStartedAt: lagDriftOptionalTime(test.FirstStartedAt),
		RetestUntil:    lagDriftOptionalTime(test.RetestUntil),
		UpdatedAt:      lagDriftTime(test.UpdatedAt),
		Runs:           []*LagDriftRunView{},
		Result:         result,
	}
	for _, run := range runs {
		runView := &LagDriftRunView{
			Run:             run.Run,
			State:           run.State,
			StartedAt:       lagDriftTime(run.StartedAt),
			CompletedAt:     lagDriftOptionalTime(run.CompletedAt),
			MeasuredMs:      run.MeasuredMs,
			ClientIpMatches: run.ClientIpMatches,
			Extenders:       lagDriftRunMainExtenders(run, drift),
			Answered:        run.Answered,
			InVerdict:       inVerdict[run.Run],
			Outcome:         run.Outcome,
		}
		if run.Stop != "" {
			stop := run.Stop
			runView.Stop = &stop
		}
		view.Runs = append(view.Runs, runView)
	}
	return view
}

// Starts a run (P5).
type LagDriftStartArgs struct {
	Token string `json:"token"`
	// the frame's parent, from its ancestor origins or referrer, "" when
	// unknown (W5)
	ParentOrigin string `json:"parent_origin,omitempty"`
	// the frame couldn't reach the pinned origin, so the run goes ahead from
	// client_ip's other family (K3, K6)
	Unpinned bool `json:"unpinned,omitempty"`
}

// The place the radar centers on (P5): /my-ip-info's fields for the start's
// address.
type LagDriftIpPlace struct {
	CountryCode string   `json:"country_code,omitempty"`
	CountryName string   `json:"country_name,omitempty"`
	Region      string   `json:"region,omitempty"`
	City        string   `json:"city,omitempty"`
	Latitude    *float64 `json:"latitude,omitempty"`
	Longitude   *float64 `json:"longitude,omitempty"`
}

// One extender's ticket (E1-E3): its host and place, its position, 0-11 for
// the run's extenders and 12-15 for the spares, and the ticket text.
type LagDriftTicket struct {
	ExtenderId server.Id            `json:"extender_id"`
	Host       string               `json:"host"`
	Position   int                  `json:"position"`
	Ticket     string               `json:"ticket"`
	Location   *LagExtenderLocation `json:"location"`
}

type LagDriftStartResult struct {
	// absent for the demo token, which stores nothing (W8)
	DriftTestId *server.Id `json:"drift_test_id,omitempty"`
	Demo        bool       `json:"demo,omitempty"`
	TrackId     *server.Id `json:"track_id,omitempty"`
	// from 1; absent for the demo token
	Run int `json:"run,omitempty"`
	// the runs the token can start after this one
	RunsLeft int `json:"runs_left"`
	// the cap from the sessions' opening (V10)
	MeasureMs int `json:"measure_ms,omitempty"`
	// the token's confidence, at which the radar draws the region (V8)
	Confidence float64           `json:"confidence,omitempty"`
	Ip         *LagDriftIpPlace  `json:"ip,omitempty"`
	Tickets    []*LagDriftTicket `json:"tickets,omitempty"`
	// RFC 3339: every session must open before this
	ExpiresAt string    `json:"expires_at,omitempty"`
	Error     *LagError `json:"error,omitempty"`
	// the API's origin in client_ip's family, for a start that arrived over
	// the other (K6): the frame sends the same request there
	Pin string `json:"pin,omitempty"`
}

// A pin answer is the pin and nothing else (P5).
func (self *LagDriftStartResult) MarshalJSON() ([]byte, error) {
	if self.Pin != "" {
		return json.Marshal(struct {
			Pin string `json:"pin"`
		}{self.Pin})
	}
	type plain LagDriftStartResult
	return json.Marshal((*plain)(self))
}

// Answers `POST /lag/drift-test/start` (P5, W8).
func LagDriftTestStart(args *LagDriftStartArgs, clientSession *session.ClientSession) (*LagDriftStartResult, error) {
	result, err := lagDriftTestStart(args, clientSession, server.NowUtc(), GetLagSettings())
	countLagDriftStart(result, err)
	return result, err
}

func lagDriftTestStart(
	args *LagDriftStartArgs,
	clientSession *session.ClientSession,
	now time.Time,
	settings *LagSettings,
) (*LagDriftStartResult, error) {
	if args == nil || strings.TrimSpace(args.Token) == "" {
		return nil, lagDriftInvalid("token is required.")
	}
	ctx := clientSession.Ctx
	drift := &settings.Drift
	addr, _, err := clientSession.ParseClientIpPort()
	if err != nil {
		// the parse error would quote the address, so it goes no further
		// (amendment 9)
		return nil, errors.New("could not read the caller address")
	}
	addr = addr.Unmap().WithZone("")
	parentOrigin, ok := lagDriftParseOrigin(args.ParentOrigin, false)
	if !ok {
		return &LagDriftStartResult{
			Error: lagDriftRefusal(LagDriftErrorCodeOrigin, "This page may not run the location check."),
		}, nil
	}
	if !drift.On() {
		// the operator's kill switch stops the demo too (lag.yml drift.enabled)
		return &LagDriftStartResult{
			Error: lagDriftRefusal(LagErrorCodeUnavailable, "The location check is not available now."),
		}, nil
	}
	if args.Token == LagDriftDemoToken {
		return lagDriftDemoStart(ctx, addr, now, settings)
	}

	var test *model.LagDriftTest
	tokenHash, ok := lagDriftTokenHash(args.Token)
	if ok {
		test = model.GetLagDriftTestByTokenHash(ctx, tokenHash)
	}
	// pinned to client_ip's family (K6, P5): a start over the other family
	// is pointed at the API's name in client_ip's family, and uses no run and
	// no count of the start limit. One the frame sends unpinned, having
	// failed to reach that name, goes ahead from the other family (K3).
	if test != nil && !args.Unpinned && server.IpVersionForAddr(addr) != test.ClientIpFamily {
		return &LagDriftStartResult{Pin: drift.PinOrigin(test.ClientIpFamily)}, nil
	}

	limit, err := model.CheckLagPrefixRateLimit(ctx, lagDriftStartRateLimitName, addr, drift.StartLimit, drift.StartLimitWindow)
	if err != nil {
		return nil, err
	}
	if !limit.Allowed {
		return &LagDriftStartResult{
			Error: lagDriftLimited(
				limit,
				"Too many location checks from your network. The limit is shared by everyone on it, so this may not be your own activity.",
			),
		}, nil
	}

	expired := &LagDriftStartResult{
		Error: lagDriftRefusal(LagDriftErrorCodeExpired, "This check has expired."),
	}
	if test == nil {
		return expired, nil
	}
	// the network's Embed and its drift switch are checked again at every
	// start, so a network switched off stops the tokens it already holds,
	// within the Embed cache's 30 s (decision 3, P3)
	network := model.GetLagDriftNetwork(ctx, test.NetworkId)
	if network.Disabled || !model.NetworkEmbedEnabled(ctx, test.NetworkId) {
		return expired, nil
	}
	if !lagDriftOriginAllowed(parentOrigin, test.ParentOrigins, network.ParentOrigins) {
		return &LagDriftStartResult{
			Error: lagDriftRefusal(LagDriftErrorCodeOrigin, "This page may not run the location check."),
		}, nil
	}
	if refusal := model.LagDriftStartRefusal(test, model.GetLagDriftRuns(ctx, test.DriftTestId), now, drift.MaxRuns, drift.ProcessingAfter); refusal != "" {
		return &LagDriftStartResult{Error: lagDriftStartRefusalError(refusal)}, nil
	}
	// a verdict needs the borders, so without them no run starts (G1)
	if lagDriftBorders() == nil {
		return &LagDriftStartResult{
			Error: lagDriftRefusal(LagErrorCodeUnavailable, "The location check is not available now."),
		}, nil
	}

	signer, signerRefusal := newLagTicketSigner()
	if signerRefusal != nil {
		return &LagDriftStartResult{Error: signerRefusal.Error}, nil
	}
	place := lagDriftStartPlace(addr)
	// a retest prefers paths the window has not measured (E2, V7)
	used := lagDriftWindowExtenderIds(model.GetLagDriftRuns(ctx, test.DriftTestId), drift)
	chosen := lagDriftChooseExtenders(ctx, now, settings, place, used)
	if lagDriftMainCount(chosen, drift) < drift.MinExtenders {
		return &LagDriftStartResult{
			Error: lagDriftRefusal(LagErrorCodeUnavailable, "Too few extenders can be measured now. Try again shortly."),
		}, nil
	}

	trackId := server.NewId()
	tickets, ticketsResult, expiresAt, err := signer.signDrift(test.DriftTestId, test.NetworkId, trackId, chosen, false, now, settings)
	if err != nil {
		return nil, err
	}
	// the address goes no further than these lookups (amendment 9); the run
	// is bound to client_ip by the peppered hash of the /29 (/56), compared
	// in memory (K3)
	client := newLagTrackClient(addr)
	startIpHash := server.ClientIpHashForAddr(addr)
	added := model.AddLagDriftRun(ctx, &model.AddLagDriftRunArgs{
		DriftTestId:   test.DriftTestId,
		StartIpHash:   startIpHash,
		StartIpFamily: server.IpVersionForAddr(addr),
		Track: &model.LagTrack{
			TrackId:         trackId,
			NetworkId:       test.NetworkId,
			ClientId:        test.DriftTestId,
			StartedAt:       now,
			Client:          client,
			AutomatedReason: lagTrackAutomatedReason(http.Header(clientSession.Header), client.Asn),
			Source:          model.LagSourceWeb,
		},
		Tickets:         tickets,
		IpCountryCode:   place.CountryCode,
		IpCountryName:   place.CountryName,
		Extenders:       lagDriftRunExtenders(chosen),
		Now:             now,
		MaxRuns:         drift.MaxRuns,
		RetestWindow:    drift.RetestWindow,
		ProcessingAfter: drift.ProcessingAfter,
	})
	if added.Refusal != "" {
		return &LagDriftStartResult{Error: lagDriftStartRefusalError(added.Refusal)}, nil
	}
	countLagDriftRun(test.NetworkId)

	return &LagDriftStartResult{
		DriftTestId: &test.DriftTestId,
		TrackId:     &trackId,
		Run:         added.Run,
		RunsLeft:    added.RunsLeft,
		MeasureMs:   drift.MeasureMs,
		Confidence:  test.Confidence,
		Ip:          place.view(),
		Tickets:     ticketsResult,
		ExpiresAt:   lagDriftTime(expiresAt),
	}, nil
}

// The demo token's run (W8): tickets to extenders chosen as for any run, with
// NoReport, so no extender reports them. Nothing is written but the per-/24
// (/48) counter that limits it; no drift test, run, track or ticket is
// stored, and there is no drift test id.
func lagDriftDemoStart(ctx context.Context, addr netip.Addr, now time.Time, settings *LagSettings) (*LagDriftStartResult, error) {
	drift := &settings.Drift
	limit, err := model.CheckLagPrefixRateLimit(ctx, lagDriftDemoRateLimitName, addr, drift.DemoStartsPerHour, time.Hour)
	if err != nil {
		return nil, err
	}
	if !limit.Allowed {
		return &LagDriftStartResult{
			Error: lagDriftLimited(
				limit,
				"Too many demo checks from your network. The limit is shared by everyone on it, so this may not be your own activity.",
			),
		}, nil
	}
	signer, signerRefusal := newLagTicketSigner()
	if signerRefusal != nil {
		return &LagDriftStartResult{Error: signerRefusal.Error}, nil
	}
	place := lagDriftStartPlace(addr)
	chosen := lagDriftChooseExtenders(ctx, now, settings, place, nil)
	if lagDriftMainCount(chosen, drift) < drift.MinExtenders {
		return &LagDriftStartResult{
			Error: lagDriftRefusal(LagErrorCodeUnavailable, "Too few extenders can be measured now. Try again shortly."),
		}, nil
	}
	// a client and a track that name nothing stored: the extenders never
	// report these sessions (E4), and an older one's report finds no ticket
	// (A7)
	_, ticketsResult, expiresAt, err := signer.signDrift(server.NewId(), server.Id{}, server.NewId(), chosen, true, now, settings)
	if err != nil {
		return nil, err
	}
	return &LagDriftStartResult{
		Demo:       true,
		RunsLeft:   drift.MaxRuns,
		MeasureMs:  drift.MeasureMs,
		Confidence: drift.ConfidenceDefault,
		Ip:         place.view(),
		Tickets:    ticketsResult,
		ExpiresAt:  lagDriftTime(expiresAt),
	}, nil
}

// The refusal of a start for the token's own state (P5).
func lagDriftStartRefusalError(refusal string) *LagError {
	switch refusal {
	case model.LagDriftRefusalBusy:
		return lagDriftRefusal(LagDriftErrorCodeBusy, "A check is still running.")
	case model.LagDriftRefusalClosed:
		return lagDriftRefusal(LagDriftErrorCodeClosed, "This check has no more runs.")
	default:
		return lagDriftRefusal(LagDriftErrorCodeExpired, "This check has expired.")
	}
}

// The load caps of a drift ticket (lag.dev/DRIFTTEST.md E3): 1 each, the
// least an extender of any version honors. An extender before connect
// 76acc8a1 reads a zero cap as its default, 5 s each way over 4 streams up to
// 2 GiB, so a ticket of zeros would admit a full load there; the server can't
// tell an extender's version. With 1s, every extender clamps the phases to its
// 250 ms floor, one stream and one byte, and its hello names 250 ms, 250 ms
// and 1 stream, which the SDK's own clamp of the ticket allows. The SDK never
// asks for a load.
const (
	lagDriftTicketPhaseMs  = 1
	lagDriftTicketStreams  = 1
	lagDriftTicketMaxBytes = 1
)

// A demo ticket's track, derived from its ticket (W8, E4): an extender before
// NoReport still reports a demo session, and with this the report path knows
// such a report for what it is, with nothing stored (lagDriftDemoReport).
func lagDriftDemoTrackId(ticketId server.Id) server.Id {
	digest := sha256.Sum256(append([]byte("lag.dev drift demo track v1:"), ticketId.Bytes()...))
	return server.RequireIdFromBytes(digest[:16])
}

// Whether a report is of a demo session (W8): its track is the one derived
// from its ticket. Such a report is dropped before anything is stored, and
// counted.
func lagDriftDemoReport(ticketId server.Id, trackId server.Id) bool {
	if lagDriftDemoTrackId(ticketId) != trackId {
		return false
	}
	countLagDriftDemoReport()
	return true
}

// Signs one ticket per chosen extender (E1-E3, M0): pings only, with load
// caps that bound any load to nothing (E3), and a prompt report (E4), or no
// report at all for the demo token, whose tickets each have their own derived
// track (W8). The ticket's client is the drift test (E1). Answers the tickets
// to store, the tickets to answer, and when they expire.
func (self *lagTicketSigner) signDrift(
	clientId server.Id,
	networkId server.Id,
	trackId server.Id,
	chosen []*lagDriftExtender,
	demo bool,
	now time.Time,
	settings *LagSettings,
) ([]*model.LagTicket, []*LagDriftTicket, time.Time, error) {
	// whole seconds, so the expiry answered is the one signed
	expiresAt := now.Add(settings.Drift.TicketTtl).Truncate(time.Second)
	tickets := []*model.LagTicket{}
	ticketsResult := []*LagDriftTicket{}
	for _, choice := range chosen {
		ticketId := server.NewId()
		ticketTrackId := trackId
		if demo {
			ticketTrackId = lagDriftDemoTrackId(ticketId)
		}
		ticket, err := connect.SignLagTicket(&protocol.LagTicketBody{
			TicketId:          ticketId.Bytes(),
			ClientId:          clientId.Bytes(),
			ExtenderPublicKey: choice.extender.PublicKey,
			NetworkHost:       self.networkHost,
			IssueTimeMs:       uint64(now.UnixMilli()),
			ExpireTimeMs:      uint64(expiresAt.UnixMilli()),
			TrackId:           ticketTrackId.Bytes(),
			TrackPosition:     uint32(choice.position),
			PingIntervalMs:    uint32(settings.PingIntervalMs),
			// any load bounded to nothing, on every extender version (E3)
			DownloadMs:       lagDriftTicketPhaseMs,
			UploadMs:         lagDriftTicketPhaseMs,
			MaxStreams:       lagDriftTicketStreams,
			MaxDownloadBytes: lagDriftTicketMaxBytes,
			MaxUploadBytes:   lagDriftTicketMaxBytes,
			ReportPromptly:   !demo,
			NoReport:         demo,
		}, self.rootPrivateKey)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		ticketText, err := connect.EncodeLagTicket(ticket)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		tickets = append(tickets, &model.LagTicket{
			TicketId:   ticketId,
			TrackId:    ticketTrackId,
			Position:   choice.position,
			ClientId:   clientId,
			NetworkId:  networkId,
			ExtenderId: choice.extender.ExtenderId,
			IssuedAt:   now,
			ExpiresAt:  expiresAt,
		})
		place := choice.extender.Place
		ticketsResult = append(ticketsResult, &LagDriftTicket{
			ExtenderId: choice.extender.ExtenderId,
			Host:       choice.extender.Name,
			Position:   choice.position,
			Ticket:     ticketText,
			Location: &LagExtenderLocation{
				City:        place.City,
				Region:      place.Region,
				CountryCode: place.CountryCode,
				CountryName: place.CountryName,
				Latitude:    place.Latitude,
				Longitude:   place.Longitude,
			},
		})
	}
	return tickets, ticketsResult, expiresAt, nil
}

// Ends a run (P5).
type LagDriftFinishArgs struct {
	Token string `json:"token"`
	Run   int    `json:"run"`
	// the browser's reason: converged, clear, interrupted or cap (V10)
	Stop string `json:"stop"`
}

type LagDriftFinishResult struct {
	Run int `json:"run,omitempty"`
	// processing, or complete once its reports are in
	RunState string    `json:"run_state,omitempty"`
	Error    *LagError `json:"error,omitempty"`
}

// Answers `POST /lag/drift-test/finish` (P5): the frame closed the run's
// sessions. Idempotent: a finished run keeps its first stop.
func LagDriftTestFinish(args *LagDriftFinishArgs, clientSession *session.ClientSession) (*LagDriftFinishResult, error) {
	return lagDriftTestFinish(args, clientSession, server.NowUtc(), GetLagSettings())
}

func lagDriftTestFinish(
	args *LagDriftFinishArgs,
	clientSession *session.ClientSession,
	now time.Time,
	settings *LagSettings,
) (*LagDriftFinishResult, error) {
	if args == nil || strings.TrimSpace(args.Token) == "" {
		return nil, lagDriftInvalid("token is required.")
	}
	if args.Token == LagDriftDemoToken {
		return nil, lagDriftInvalid("The demo token has no runs to finish.")
	}
	if args.Run < 1 {
		return nil, lagDriftInvalid("run is required.")
	}
	if !slices.Contains(lagDriftStops, args.Stop) {
		return nil, lagDriftInvalid("stop is converged, clear, interrupted or cap.")
	}
	ctx := clientSession.Ctx
	tokenHash, ok := lagDriftTokenHash(args.Token)
	if !ok {
		return &LagDriftFinishResult{Error: lagDriftStartRefusalError(model.LagDriftRefusalExpired)}, nil
	}
	test := model.GetLagDriftTestByTokenHash(ctx, tokenHash)
	if test == nil {
		return &LagDriftFinishResult{Error: lagDriftStartRefusalError(model.LagDriftRefusalExpired)}, nil
	}
	run := model.FinishLagDriftRun(ctx, test.DriftTestId, args.Run, args.Stop, now)
	if run == nil {
		return nil, lagDriftInvalid("The check has no run %d.", args.Run)
	}
	// with every report in already, the run completes now (E5)
	_, runs := advanceLagDriftTest(ctx, test, now, settings)
	for _, advanced := range runs {
		if advanced.Run == run.Run {
			run = advanced
		}
	}
	return &LagDriftFinishResult{
		Run:      run.Run,
		RunState: run.State,
	}, nil
}

// Reads what the widget shows (P5).
type LagDriftResultArgs struct {
	Token string `json:"token"`
}

// Only what the widget shows (P5, decision 7): never the drift country, a
// distance or the fit. The token holder is the visitor, who may be the one
// being checked.
type LagDriftWidgetResult struct {
	State string `json:"state,omitempty"`
	// the newest run, 0 before the first
	Run      int    `json:"run"`
	RunState string `json:"run_state,omitempty"`
	// agree, disagree or inconclusive; null before a run has completed
	Verdict       *string `json:"verdict"`
	IpCountryName string  `json:"ip_country_name,omitempty"`
	// the runs the verdict pools, which the frame keeps on its radar (V9)
	RunsInVerdict []int     `json:"runs_in_verdict"`
	RetestAllowed bool      `json:"retest_allowed"`
	RunsLeft      int       `json:"runs_left"`
	Error         *LagError `json:"error,omitempty"`
}

// Answers `POST /lag/drift-test/result` (P5).
func LagDriftTestResultRead(args *LagDriftResultArgs, clientSession *session.ClientSession) (*LagDriftWidgetResult, error) {
	return lagDriftTestResult(args, clientSession, server.NowUtc(), GetLagSettings())
}

func lagDriftTestResult(
	args *LagDriftResultArgs,
	clientSession *session.ClientSession,
	now time.Time,
	settings *LagSettings,
) (*LagDriftWidgetResult, error) {
	if args == nil || strings.TrimSpace(args.Token) == "" {
		return nil, lagDriftInvalid("token is required.")
	}
	if args.Token == LagDriftDemoToken {
		return nil, lagDriftInvalid("The demo token's verdict is the frame's own.")
	}
	ctx := clientSession.Ctx
	tokenHash, ok := lagDriftTokenHash(args.Token)
	if !ok {
		return &LagDriftWidgetResult{RunsInVerdict: []int{}, Error: lagDriftStartRefusalError(model.LagDriftRefusalExpired)}, nil
	}
	test := model.GetLagDriftTestByTokenHash(ctx, tokenHash)
	if test == nil {
		return &LagDriftWidgetResult{RunsInVerdict: []int{}, Error: lagDriftStartRefusalError(model.LagDriftRefusalExpired)}, nil
	}
	test, runs := advanceLagDriftTest(ctx, test, now, settings)
	return newLagDriftWidgetResult(test, runs, now, &settings.Drift), nil
}

func newLagDriftWidgetResult(test *model.LagDriftTest, runs []*model.LagDriftRun, now time.Time, drift *LagDriftSettings) *LagDriftWidgetResult {
	runsLeft := lagDriftRunsLeft(test, now, drift)
	widget := &LagDriftWidgetResult{
		State:         lagDriftTestState(test, runs, now, drift),
		RunsInVerdict: []int{},
		RetestAllowed: 0 < runsLeft && 0 < test.RunCount,
		RunsLeft:      runsLeft,
	}
	// the country judged: client_ip's (K3)
	widget.IpCountryName = test.ClientIpCountryName
	if 0 < len(runs) {
		newest := runs[len(runs)-1]
		widget.Run = newest.Run
		widget.RunState = newest.State
	}
	if result := lagDriftStoredResult(test); result != nil {
		verdict := result.Verdict
		widget.Verdict = &verdict
		widget.RunsInVerdict = result.RunsInVerdict
	}
	return widget
}

// The window runs' extenders, which a retest prefers not to repeat (E2).
func lagDriftWindowExtenderIds(runs []*model.LagDriftRun, drift *LagDriftSettings) map[server.Id]bool {
	used := map[server.Id]bool{}
	for _, run := range lagDriftWindowRuns(runs, drift) {
		for _, extender := range run.Extenders {
			used[extender.ExtenderId] = true
		}
	}
	return used
}

// The runs that count (V7): complete, from client_ip or its other family,
// and counted by processing.
func lagDriftCounts(run *model.LagDriftRun) bool {
	return run.Complete() && run.Outcome == LagDriftRunOutcomeCounted && (run.ClientIpMatches == nil || *run.ClientIpMatches)
}

// The last window_runs runs that count, by run number (V7), of one family:
// the family of the newest run that counts. Runs from the two families can
// end at different places, so they are never pooled (K3).
func lagDriftWindowRuns(runs []*model.LagDriftRun, drift *LagDriftSettings) []*model.LagDriftRun {
	window := []*model.LagDriftRun{}
	family := 0
	for i := len(runs) - 1; 0 <= i && len(window) < drift.WindowRuns; i -= 1 {
		run := runs[i]
		if !lagDriftCounts(run) {
			continue
		}
		if family == 0 {
			family = run.IpFamily
		}
		if run.IpFamily == family {
			window = append(window, run)
		}
	}
	slices.Reverse(window)
	return window
}

// A figure as the result answers it, to a tenth.
func lagDriftRound(value float64) float64 {
	return math.Round(value*10) / 10
}
