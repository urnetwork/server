package controller

// sn_ctf_api.go — the capture the flag routes of sn/api/operator.yml (sn
// CAPTURETHEFLAG.md §9.1): GET /sn/ctf, /sn/ctf/here, /sn/ctf/country/{cc},
// /sn/ctf/top, /sn/ctf/epoch/{epoch} and /sn/ctf/client. The answers are
// derived in sn_ctf_api_state.go from the view sn_ctf_api_source.go reads.
//
// The routes answer only where configured, in the api block of the vault
// resource ctf.yml (absent: off); until then every route but /sn/ctf/here
// answers 503 unavailable, which the page shows as unavailable, never zero:
//
//	api:
//	  enabled: false                  # the /sn/ctf routes answer
//	  page_url: https://ur.xyz/ctf    # links.page; a country's page is page_url/<cc>
//
// The same switch, with st.yml's ctf block on, has the ledger route and the
// claim relay read the operator's capture the flag pool (stCtfPoolOn); while
// either is off, they read no capture the flag pool at all.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The Cache-Control of the routes' answers.
const (
	snCtfCacheControl          = "public, max-age=60"
	snCtfFinalizedCacheControl = "public, max-age=31536000, immutable"
	snCtfHereCacheControl      = "private, no-store"
	snCtfClientCacheControl    = "private, no-cache"
)

// The limits of the routes' parameters.
const (
	snCtfTopDefaultLimit    = 10
	snCtfClientDefaultLimit = 8
	snCtfClientMaximumLimit = 52
)

// How long a view is answered from before it is read again. The answers are a
// function of the view, so between the events that change it (an epoch start,
// a census or root commitment, a root finalization) they are the same bytes.
const snCtfViewTtl = 30 * time.Second

// How long ctf.yml's api block is kept before it is read again.
const snCtfApiConfigTtl = time.Minute

const snCtfDefaultPageUrl = "https://ur.xyz/ctf"

var errSnCtfNotConfigured = errors.New("capture the flag is not configured on this server")

// snCtfApiConfig is ctf.yml's api block.
type snCtfApiConfig struct {
	Enabled bool   `yaml:"enabled"`
	PageUrl string `yaml:"page_url"`
}

type snCtfApiConfigFile struct {
	Api snCtfApiConfig `yaml:"api"`
}

var snCtfApiConfigCache struct {
	lock     sync.Mutex
	config   *snCtfApiConfig
	readTime time.Time
}

// snCtfApiConfigOf is ctf.yml's api block; off when the resource is absent or
// does not parse. Tests replace it.
var snCtfApiConfigOf = func() *snCtfApiConfig {
	cache := &snCtfApiConfigCache
	cache.lock.Lock()
	defer cache.lock.Unlock()
	now := time.Now()
	if cache.config != nil && now.Sub(cache.readTime) < snCtfApiConfigTtl {
		return cache.config
	}
	config := &snCtfApiConfig{}
	server.HandleError(func() {
		resource, err := server.Vault.SimpleResource("ctf.yml")
		if err != nil {
			return
		}
		var file snCtfApiConfigFile
		if err := resource.UnmarshalYamlE(&file); err != nil {
			glog.Errorf("[sn]ctf.yml does not parse; the capture the flag routes are off: %s\n", err)
			return
		}
		config = &file.Api
	})
	if config.PageUrl == "" {
		config.PageUrl = snCtfDefaultPageUrl
	}
	cache.config, cache.readTime = config, now
	return config
}

// snCtfSource reads what the routes answer from.
type snCtfSource interface {
	// View is the pool, this epoch, the committed roots, every covered epoch
	// and the pending draw, read at one time.
	View(ctx context.Context) (*snCtfView, error)
	// EpochDetail is a covered epoch's census counts, draw and pending awards.
	EpochDetail(ctx context.Context, view *snCtfView, epoch *snCtfEpoch) (*snCtfEpochDetail, error)
	// ClientEpochs are a client's census members and awards in the epochs.
	ClientEpochs(ctx context.Context, view *snCtfView, clientId server.Id, epochs []*snCtfEpoch) (map[uint64]*snCtfClientEpochFacts, error)
	// Wins are the scope's awards in the view's covered epochs: its client's,
	// or every client's its network has had.
	Wins(ctx context.Context, view *snCtfView, scope snCtfClientScope) ([]*snCtfWin, error)
	// AccountClaims reads the claim state of each coldkey's account in the
	// CTF pool at the roots it needs, while ctx lasts, and returns when it
	// ends, with nothing it started still running. A coldkey whose state
	// cannot be read in that time is left out, so its wins answer claimed
	// null.
	AccountClaims(ctx context.Context, view *snCtfView, needs []*snCtfClaimNeed) map[[32]byte]*snCtfAccountClaims
}

// snCtfSourceOf is the source of the routes, errSnCtfNotConfigured where
// capture the flag is not configured. Tests replace it.
var snCtfSourceOf = func() (snCtfSource, error) {
	if !snCtfApiConfigOf().Enabled {
		return nil, errSnCtfNotConfigured
	}
	return newSnCtfCoreSource()
}

// How long one read of the view may take, and how old a view may be served
// while a newer one is read.
const (
	snCtfViewReadTimeout = 30 * time.Second
	snCtfViewMaxStale    = 5 * time.Minute
)

var snCtfViewCache struct {
	lock     sync.Mutex
	view     *snCtfView
	readTime time.Time
	// closed when the read in progress ends; nil when none is
	reading chan struct{}
	// the last read's error
	err error
}

// snCtfLoadView is the view, read at most once at a time on a context of its
// own, so that no request waits on another's read or fails with another's
// cancellation. While a view is fresh it is answered; while a newer one is
// read, the previous one is answered if it is not too old; otherwise the
// request waits for the read.
func snCtfLoadView(ctx context.Context, source snCtfSource) (*snCtfView, error) {
	cache := &snCtfViewCache
	cache.lock.Lock()
	now := time.Now()
	if cache.view != nil && now.Sub(cache.readTime) < snCtfViewTtl {
		view := cache.view
		cache.lock.Unlock()
		return view, nil
	}
	if cache.reading == nil {
		reading := make(chan struct{})
		cache.reading = reading
		go server.HandleError(func() {
			defer close(reading)
			readCtx, cancel := context.WithTimeout(context.Background(), snCtfViewReadTimeout)
			defer cancel()
			view, err := source.View(readCtx)
			cache.lock.Lock()
			defer cache.lock.Unlock()
			if err == nil {
				cache.view, cache.readTime = view, time.Now()
			}
			cache.err, cache.reading = err, nil
		}, func() {
			cache.lock.Lock()
			defer cache.lock.Unlock()
			cache.err, cache.reading = errors.New("the capture the flag view read panicked"), nil
		})
	}
	reading := cache.reading
	if cache.view != nil && now.Sub(cache.readTime) < snCtfViewMaxStale {
		view := cache.view
		cache.lock.Unlock()
		return view, nil
	}
	cache.lock.Unlock()
	if reading != nil {
		select {
		case <-reading:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	cache.lock.Lock()
	defer cache.lock.Unlock()
	if cache.view != nil && time.Since(cache.readTime) < snCtfViewMaxStale {
		return cache.view, nil
	}
	if cache.err != nil {
		return nil, cache.err
	}
	return nil, errors.New("the capture the flag view is not read yet")
}

// snCtfReadView reads the view. On a failure it has answered 503 and returns
// nil.
func snCtfReadView(w http.ResponseWriter, r *http.Request) (snCtfSource, *snCtfView) {
	source, err := snCtfSourceOf()
	if err != nil {
		if !errors.Is(err, errSnCtfNotConfigured) {
			glog.Infof("[sn]ctf source: %s\n", err)
		}
		WriteSnProblem(w, http.StatusServiceUnavailable, SnProblemUnavailable, "The capture the flag state is unavailable. Try again shortly.")
		return nil, nil
	}
	cached, err := snCtfLoadView(r.Context(), source)
	if err != nil {
		glog.Infof("[sn]ctf view: %s\n", err)
		WriteSnProblem(w, http.StatusServiceUnavailable, SnProblemUnavailable, "The capture the flag state is unavailable. Try again shortly.")
		return nil, nil
	}
	view := *cached
	view.PageUrl = snCtfApiConfigOf().PageUrl
	return source, &view
}

func snCtfResetViewCache() {
	cache := &snCtfViewCache
	cache.lock.Lock()
	reading := cache.reading
	cache.lock.Unlock()
	if reading != nil {
		<-reading
	}
	cache.lock.Lock()
	defer cache.lock.Unlock()
	cache.view, cache.err = nil, nil
}

// SnCtf serves GET /sn/ctf.
func SnCtf(w http.ResponseWriter, r *http.Request) {
	SnOperatorCors(w, r)
	_, view := snCtfReadView(w, r)
	if view == nil {
		return
	}
	writeSnCacheableJson(w, r, nil, snCtfCacheControl, view.AsOf, snCtfResultOf(view))
}

// SnCtfHere serves GET /sn/ctf/here: the caller's country from GeoLite2 on the
// request's address, or null. Nothing about the request is stored or logged.
func SnCtfHere(w http.ResponseWriter, r *http.Request) {
	SnOperatorCors(w, r)
	result := &SnCtfHereResult{}
	server.HandleError(func() {
		address, err := session.ResolveClientAddress(r)
		if err != nil {
			return
		}
		ip, _, err := server.SplitClientAddress(address)
		if err != nil {
			return
		}
		info, err := server.GetIpInfoFromString(ip)
		if err != nil {
			return
		}
		if country, ok := snCtfParseCountry(info.CountryCode); ok {
			code := country.Code
			result.Country = &code
		}
	})
	body, err := json.Marshal(result)
	if err != nil {
		WriteSnProblem(w, http.StatusServiceUnavailable, SnProblemUnavailable, "The answer could not be encoded.")
		return
	}
	header := w.Header()
	header.Set("Cache-Control", snCtfHereCacheControl)
	header.Set("Link", SnOperatorLink)
	header.Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

var snCtfCountryCodePattern = regexp.MustCompile(`^[A-Za-z]{2}$`)

// SnCtfCountry serves GET /sn/ctf/country/{cc}, the code in either case.
func SnCtfCountry(w http.ResponseWriter, r *http.Request, code string) {
	SnOperatorCors(w, r)
	if !snCtfCountryCodePattern.MatchString(code) {
		WriteSnProblem(w, http.StatusBadRequest, SnProblemBadRequest, "cc must be a two-letter ISO 3166-1 alpha-2 code.")
		return
	}
	country, ok := snCtfParseCountry(code)
	if !ok {
		WriteSnProblem(w, http.StatusNotFound, SnProblemUnknownCountry, fmt.Sprintf("%s is not one of the 249 ISO 3166-1 alpha-2 codes.", string([]byte{code[0] | 0x20, code[1] | 0x20})))
		return
	}
	_, view := snCtfReadView(w, r)
	if view == nil {
		return
	}
	writeSnCacheableJson(w, r, nil, snCtfCacheControl, view.AsOf, snCtfCountryResultOf(view, country))
}

// SnCtfTop serves GET /sn/ctf/top?role=&limit=&include_uninhabited=.
func SnCtfTop(w http.ResponseWriter, r *http.Request) {
	SnOperatorCors(w, r)
	query := r.URL.Query()
	role := snCtfTopRoleAny
	if value := query.Get("role"); value != "" {
		switch value {
		case snCtfTopRoleProvider, snCtfTopRoleExtender, snCtfTopRoleAny:
			role = value
		default:
			WriteSnProblem(w, http.StatusBadRequest, SnProblemBadRequest, "role must be provider, extender or any.")
			return
		}
	}
	limit, ok := snCtfLimit(query.Get("limit"), snCtfTopDefaultLimit, snCtfCountryCount)
	if !ok {
		WriteSnProblem(w, http.StatusBadRequest, SnProblemBadRequest, fmt.Sprintf("limit must be an integer from 1 to %d.", snCtfCountryCount))
		return
	}
	includeUninhabited := false
	if value := query.Get("include_uninhabited"); value != "" {
		switch value {
		case "true":
			includeUninhabited = true
		case "false":
		default:
			WriteSnProblem(w, http.StatusBadRequest, SnProblemBadRequest, "include_uninhabited must be true or false.")
			return
		}
	}
	_, view := snCtfReadView(w, r)
	if view == nil {
		return
	}
	writeSnCacheableJson(w, r, nil, snCtfCacheControl, view.AsOf, snCtfTopOf(view, role, limit, includeUninhabited))
}

// A limit parameter: the default when absent, else an integer from 1 to most.
func snCtfLimit(value string, fallback int, most int) (int, bool) {
	if value == "" {
		return fallback, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || most < limit {
		return 0, false
	}
	return limit, true
}

var snCtfEpochPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,18})$`)

// SnCtfEpoch serves GET /sn/ctf/epoch/{epoch}: immutable once its root is
// finalized.
func SnCtfEpoch(w http.ResponseWriter, r *http.Request, value string) {
	SnOperatorCors(w, r)
	if !snCtfEpochPattern.MatchString(value) {
		WriteSnProblem(w, http.StatusBadRequest, SnProblemBadRequest, "epoch must be a non-negative integer.")
		return
	}
	number, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		WriteSnProblem(w, http.StatusBadRequest, SnProblemBadRequest, "epoch must be a non-negative integer.")
		return
	}
	source, view := snCtfReadView(w, r)
	if view == nil {
		return
	}
	if number < view.Pool.FirstEpoch {
		WriteSnProblem(w, http.StatusNotFound, SnProblemUnknownEpoch, fmt.Sprintf("Epoch %d is before the pool's first epoch, %d.", number, view.Pool.FirstEpoch))
		return
	}
	epoch := view.epoch(number)
	if epoch == nil {
		WriteSnProblem(w, http.StatusNotFound, SnProblemUnknownEpoch, fmt.Sprintf("No committed root covers epoch %d yet.", number))
		return
	}
	detail, err := source.EpochDetail(r.Context(), view, epoch)
	if err != nil {
		glog.Infof("[sn]ctf epoch %d: %s\n", number, err)
		WriteSnProblem(w, http.StatusServiceUnavailable, SnProblemUnavailable, "The capture the flag state is unavailable. Try again shortly.")
		return
	}
	cacheControl := snCtfCacheControl
	if epoch.Root.Finalized {
		cacheControl = snCtfFinalizedCacheControl
	}
	writeSnCacheableJson(w, r, nil, cacheControl, epoch.Root.changeTime(), snCtfEpochResultOf(view, epoch, detail))
}

// snCtfClientScopeOf is whom a credential's answer covers: the client a
// client token names, or, for the network credential (the network's token or
// an API key), every client of its network.
func snCtfClientScopeOf(byJwt *session.ByJwt) snCtfClientScope {
	scope := snCtfClientScope{NetworkId: byJwt.NetworkId}
	if byJwt.ClientId != nil {
		clientId := *byJwt.ClientId
		scope.ClientId = &clientId
	}
	return scope
}

// SnCtfClient serves GET /sn/ctf/client?limit=: for a client token, its
// client's status and wins; for the network credential (a network token or an
// API key), the wins of every client of its network. Only a request without a
// credential that authenticates is answered 401, never a network's.
func SnCtfClient(w http.ResponseWriter, r *http.Request) {
	SnOperatorCors(w, r)
	unauthorized := func() {
		w.Header().Set("WWW-Authenticate", "Bearer")
		WriteSnProblem(w, http.StatusUnauthorized, SnProblemUnauthorized, "Send a client's or a network's bearer token.")
	}
	clientSession, err := session.NewClientSessionFromRequest(r)
	if err != nil {
		unauthorized()
		return
	}
	defer clientSession.Cancel()
	if err := clientSession.Auth(r); err != nil {
		if errors.Is(err, session.ErrAuthUnavailable) {
			WriteSnProblem(w, http.StatusServiceUnavailable, SnProblemUnavailable, "Authentication is unavailable. Try again shortly.")
			return
		}
		unauthorized()
		return
	}
	if clientSession.ByJwt == nil {
		unauthorized()
		return
	}
	scope := snCtfClientScopeOf(clientSession.ByJwt)
	limit, ok := snCtfLimit(r.URL.Query().Get("limit"), snCtfClientDefaultLimit, snCtfClientMaximumLimit)
	if !ok {
		WriteSnProblem(w, http.StatusBadRequest, SnProblemBadRequest, fmt.Sprintf("limit must be an integer from 1 to %d.", snCtfClientMaximumLimit))
		return
	}
	source, view := snCtfReadView(w, r)
	if view == nil {
		return
	}
	result, err := snCtfClientAnswer(clientSession.Ctx, source, view, scope, limit)
	if err != nil {
		glog.Infof("[sn]ctf client: %s\n", err)
		WriteSnProblem(w, http.StatusServiceUnavailable, SnProblemUnavailable, "The capture the flag state is unavailable. Try again shortly.")
		return
	}
	writeSnCacheableJson(w, r, clientSession, snCtfClientCacheControl, view.AsOf, result)
}

// How long an answer of GET /sn/ctf/client may spend reading the claim states
// of its wins: each coldkey's account in the CTF pool, and vault2's claimed at
// the finalized head. A state not read by then answers claimed null, so a
// claim state that a chain down or slow cannot give never fails the answer,
// nor holds it past this. Tests shorten it.
var snCtfClaimsTimeout = 5 * time.Second

// snCtfClientAnswer reads what GET /sn/ctf/client answers for a scope: a
// client's census members and awards in the most recent limit covered epochs,
// the scope's wins, and the claim states of their coldkeys' accounts. A claim
// state that cannot be read within snCtfClaimsTimeout never fails the answer;
// its wins say null.
func snCtfClientAnswer(ctx context.Context, source snCtfSource, view *snCtfView, scope snCtfClientScope, limit int) (*SnCtfClientResult, error) {
	facts := map[uint64]*snCtfClientEpochFacts{}
	if scope.ClientId != nil {
		epochs := []*snCtfEpoch{}
		for i := len(view.Epochs) - 1; 0 <= i && len(epochs) < limit; i -= 1 {
			epochs = append(epochs, view.Epochs[i])
		}
		var err error
		if facts, err = source.ClientEpochs(ctx, view, *scope.ClientId, epochs); err != nil {
			return nil, fmt.Errorf("epochs: %w", err)
		}
	}
	wins, err := source.Wins(ctx, view, scope)
	if err != nil {
		return nil, fmt.Errorf("wins: %w", err)
	}
	claimsCtx, cancel := context.WithTimeout(ctx, snCtfClaimsTimeout)
	defer cancel()
	claims := source.AccountClaims(claimsCtx, view, snCtfClaimNeeds(view, wins))
	return snCtfClientResultOf(view, scope, facts, wins, claims, limit), nil
}
