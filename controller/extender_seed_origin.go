package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The api's answer at ExtenderSeedPath: the origin of the public seed bundle
// (connect/EXTENDER-UPDATE2.md §2.3).
//
// The api serves the bundle the publisher stored, exactly as stored, to anyone:
// it is public by design, identical on every mirror, and holds only signed
// records. The operations-managed mirrors copy this answer, so they need no key
// and no storage credential, and a client may fetch it directly, over DNS or
// over a configured address of a services edge with the api host as the server
// name.
//
// Each process reads the stored bundle at most once every
// extenderSeedOriginCacheTimeout and serves its copy in between. A copy is
// served only once it verifies under the configured root keys and hosts as of
// its own issue time; its freshness is the client's to judge and the mirror
// check's to report, so an expired bundle is still served, which lets the
// monitor tell a stalled publisher from an unreachable origin. A store that
// cannot be read keeps the last good copy for up to ExtenderSeedRefreshTimeout,
// and a stored object that does not verify is never served.

// How long one process serves a copy before reading the store again.
const extenderSeedOriginCacheTimeout = time.Minute

// The answers, the fixed label set of the counter.
const (
	extenderSeedOriginServed      = "served"
	extenderSeedOriginNotModified = "not_modified"
	extenderSeedOriginDisabled    = "disabled"
	extenderSeedOriginAbsent      = "absent"
	extenderSeedOriginUnavailable = "unavailable"
)

var extenderSeedOriginResponsesCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "extender_seed",
	Name:      "origin_responses_total",
	Help:      "Answers at the public seed bundle path by outcome",
}, []string{"outcome"})

func init() {
	prometheus.MustRegister(extenderSeedOriginResponsesCounter)
}

// One verified copy of the stored bundle.
type extenderSeedOriginCopy struct {
	artifact []byte
	etag     string
	readAt   time.Time
}

type extenderSeedOriginCache struct {
	stateLock sync.Mutex
	copy      *extenderSeedOriginCopy
	// when the store was last read, whatever it answered
	checkedAt time.Time
	// the store answered that it holds no bundle
	absent bool
}

var extenderSeedOrigin = &extenderSeedOriginCache{}

// Testing_ResetExtenderSeedOrigin drops every process copy of the bundle.
func Testing_ResetExtenderSeedOrigin() {
	extenderSeedOrigin.stateLock.Lock()
	defer extenderSeedOrigin.stateLock.Unlock()
	extenderSeedOrigin.copy = nil
	extenderSeedOrigin.checkedAt = time.Time{}
	extenderSeedOrigin.absent = false
}

// The copy to serve at now and the answer's outcome when there is none.
func (self *extenderSeedOriginCache) get(ctx context.Context, config *ExtenderConfig, now time.Time) (*extenderSeedOriginCopy, string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.checkedAt.IsZero() || extenderSeedOriginCacheTimeout <= now.Sub(self.checkedAt) {
		self.checkedAt = now
		artifact, absent, err := readVerifiedExtenderSeed(ctx, config)
		switch {
		case err != nil:
			glog.V(1).Infof("[extenderseed]origin read: %s\n", err)
		case absent:
			self.copy, self.absent = nil, true
		default:
			digest := sha256.Sum256(artifact)
			self.copy = &extenderSeedOriginCopy{
				artifact: artifact,
				etag:     `"` + hex.EncodeToString(digest[:]) + `"`,
				readAt:   now,
			}
			self.absent = false
		}
	}
	if self.copy != nil && now.Sub(self.copy.readAt) < ExtenderSeedRefreshTimeout {
		return self.copy, ""
	}
	if self.absent {
		return nil, extenderSeedOriginAbsent
	}
	return nil, extenderSeedOriginUnavailable
}

// The stored bundle once it verifies, or absent when none is stored.
func readVerifiedExtenderSeed(ctx context.Context, config *ExtenderConfig) (artifact []byte, absent bool, returnErr error) {
	host, err := NormalizeExtenderSeedNetworkHost(config.NetworkHost)
	if err != nil {
		return nil, false, err
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		return nil, false, errExtenderSeedOriginNoStore
	}
	artifact, err = ReadExtenderSeedLatest(ctx, store, host)
	if err != nil {
		return nil, false, err
	}
	if artifact == nil {
		return nil, true, nil
	}
	envelope, err := connect.ParseExtenderSeedEnvelope(artifact)
	if err != nil {
		return nil, false, err
	}
	body := &protocol.ExtenderSeedBody{}
	if err := proto.Unmarshal(envelope.Body, body); err != nil {
		return nil, false, err
	}
	rootKeys, err := connect.NewExtenderRootKeySetFromHex(config.RootPublicKeys()...)
	if err != nil {
		return nil, false, err
	}
	networkHosts := append([]string{config.NetworkHost}, config.NetworkHosts...)
	if _, err := connect.VerifyExtenderSeed(envelope, rootKeys, networkHosts, time.UnixMilli(int64(body.IssuedAtUnixMilli))); err != nil {
		return nil, false, err
	}
	return artifact, false, nil
}

type extenderSeedOriginError string

func (self extenderSeedOriginError) Error() string { return string(self) }

const errExtenderSeedOriginNoStore = extenderSeedOriginError("no blob store is configured")

// ServeExtenderSeed answers `GET ExtenderSeedPath`: the stored bundle as raw
// envelope bytes, 404 while the seed is off or nothing is stored, 503 while no
// verified copy is at hand.
func ServeExtenderSeed(w http.ResponseWriter, r *http.Request) {
	outcome := serveExtenderSeed(w, r, server.NowUtc())
	extenderSeedOriginResponsesCounter.WithLabelValues(outcome).Inc()
}

func serveExtenderSeed(w http.ResponseWriter, r *http.Request, now time.Time) string {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	config, err := EnvExtenderConfig()
	if err != nil || !config.Seed.Enabled {
		http.NotFound(w, r)
		return extenderSeedOriginDisabled
	}
	copy, outcome := extenderSeedOrigin.get(r.Context(), config, now)
	switch outcome {
	case extenderSeedOriginAbsent:
		http.NotFound(w, r)
		return outcome
	case extenderSeedOriginUnavailable:
		w.Header().Set("Retry-After", strconv.Itoa(int(extenderSeedOriginCacheTimeout/time.Second)))
		http.Error(w, "The seed bundle is unavailable.", http.StatusServiceUnavailable)
		return outcome
	}
	w.Header().Set("Content-Type", ExtenderSeedContentType)
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(ExtenderSeedCacheMaxAge/time.Second)))
	w.Header().Set("ETag", copy.etag)
	if extenderSeedEtagMatches(r.Header.Get("If-None-Match"), copy.etag) {
		w.WriteHeader(http.StatusNotModified)
		return extenderSeedOriginNotModified
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(copy.artifact)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(copy.artifact)
	}
	return extenderSeedOriginServed
}

// Whether an If-None-Match header names the etag (a list, or "*").
func extenderSeedEtagMatches(header string, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}
