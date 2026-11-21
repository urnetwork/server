package server

// §5.15's internal lookup of sidecars' current clients, by which a host that keeps Postgres off §6.8's hint path turns
// the sidecar ids its hints name into the client ids hints land on (plan-KEYS-hint-addressing, QD-146; §10.14's
// Tables, §19.4's gkm row). It sits under the internal gate's prefix, which the lb refuses from outside (owner
// decision 340; the path reading is question keys-late-delivery-identity-clients-path), and takes only the gkm and
// mxedge classes' bearers. A request names at most SidecarIdMaxCount sidecars; the answer gives each one api holds an
// active identity of its newest binding's client and generation, and omits the rest, unknown and revoked alike. api
// logs no per-target record (§2.8's api-5).
//
// SidecarIdentityClientsResolver is the hosts' end: pushservice.PushClientResolver for their hint writers, each
// process keeping what it learned for CacheTimeout, at most CacheMaxCount sidecars, a miss batching one call per
// SidecarIdMaxCount ids. A call that fails is an error the hint writer counts as unresolved and passes over, since
// §6.8's hourly resync covers a lost hint. Nothing here logs a bearer, a sidecar id or a client id.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The route, under the internal gate's prefix.
const SidecarIdentityClientsGatePath = InternalGatePathPrefix + "sidecar-identity-clients"

// The route's format, which both ends hold alike (QD-702): the protocol's limits as fields.
type SidecarIdentityClientsGateFormatSettings struct {
	// The most sidecar ids one request names: 1,000 (§5.15).
	SidecarIdMaxCount int
}

func DefaultSidecarIdentityClientsGateFormatSettings() *SidecarIdentityClientsGateFormatSettings {
	return &SidecarIdentityClientsGateFormatSettings{
		SidecarIdMaxCount: 1000,
	}
}

// A request: the sidecars whose current clients the caller wants.
type SidecarIdentityClientsGateArgs struct {
	SidecarIds []Id `json:"sidecar_ids"`
}

// The format's refusal of a request; the text names the part, never an id.
var ErrSidecarIdentityClientsGateInvalid = errors.New("sidecar identity clients: invalid request")

// Whether the request is the format's: one to SidecarIdMaxCount ids, none zero.
func (self *SidecarIdentityClientsGateArgs) Validate(format *SidecarIdentityClientsGateFormatSettings) error {
	if format == nil {
		format = DefaultSidecarIdentityClientsGateFormatSettings()
	}
	switch {
	case len(self.SidecarIds) == 0:
		return fmt.Errorf("%w: no sidecar id", ErrSidecarIdentityClientsGateInvalid)
	case format.SidecarIdMaxCount < len(self.SidecarIds):
		return fmt.Errorf("%w: more than %d sidecar ids", ErrSidecarIdentityClientsGateInvalid, format.SidecarIdMaxCount)
	}
	for _, sidecarId := range self.SidecarIds {
		if sidecarId == (Id{}) {
			return fmt.Errorf("%w: a zero sidecar id", ErrSidecarIdentityClientsGateInvalid)
		}
	}
	return nil
}

// One sidecar's current client.
type SidecarIdentityClientsGateClient struct {
	SidecarId  Id    `json:"sidecar_id"`
	ClientId   Id    `json:"client_id"`
	Generation int64 `json:"generation"`
}

// The answer: the asked sidecars api holds an active identity of, in the request's order.
type SidecarIdentityClientsGateResult struct {
	Clients []*SidecarIdentityClientsGateClient `json:"clients"`
}

// The resolver's tunables.
type SidecarIdentityClientsResolverSettings struct {
	// How long one call may take: 10 s. The hint sender's own HintTimeout bounds it too.
	RequestTimeout time.Duration
	// The most bytes an answer may hold: 256 KiB, an answer of the format's 1,000 clients twice over.
	AnswerMaxByteCount int64
	// How long a sidecar's client, or its absence, is kept: 5 min (§5.15). A client generation that changes within it
	// costs the hints until it ends, which the hourly resync covers.
	CacheTimeout time.Duration
	// The most sidecars kept: 65,536 (§5.15). Past it the oldest kept go first.
	CacheMaxCount int
	// The route's format. nil: DefaultSidecarIdentityClientsGateFormatSettings.
	Format *SidecarIdentityClientsGateFormatSettings
}

func DefaultSidecarIdentityClientsResolverSettings() *SidecarIdentityClientsResolverSettings {
	return &SidecarIdentityClientsResolverSettings{
		RequestTimeout:     10 * time.Second,
		AnswerMaxByteCount: 256 * 1024,
		CacheTimeout:       5 * time.Minute,
		CacheMaxCount:      65536,
		Format:             DefaultSidecarIdentityClientsGateFormatSettings(),
	}
}

// What the resolver is made from.
type SidecarIdentityClientsResolverArgs struct {
	// api's base URL, http or https, with no final slash.
	ApiUrl string
	// The caller's internal gate bearer, LoadInternalGateCredential of its class. A secret: never logged.
	Bearer string
	// nil: DefaultSidecarIdentityClientsResolverSettings.
	Settings *SidecarIdentityClientsResolverSettings
	// nil: a client of its own, which follows no redirect.
	HttpClient *http.Client
	// nil: real time.
	Clock sidecar.Clock
}

// A call api did not answer as the route's: away, refusing, or an answer that does not read. The text names the
// part, never the bearer or an id.
var ErrSidecarIdentityClientsGate = errors.New("sidecar identity clients gate")

// One sidecar as the resolver keeps it: its client, or none, until expires.
type sidecarIdentityClientsEntry struct {
	clientId connect.Id
	found    bool
	expires  time.Time
	// the insertion that wrote it, so an eviction takes only its own
	seq uint64
}

// One insertion, in order, for the oldest-first eviction.
type sidecarIdentityClientsInsertion struct {
	sidecarId connect.Id
	seq       uint64
}

// The hosts' end of the route (the file header). Safe for concurrent use.
type SidecarIdentityClientsResolver struct {
	routeUrl   string
	bearer     string
	settings   *SidecarIdentityClientsResolverSettings
	format     *SidecarIdentityClientsGateFormatSettings
	httpClient *http.Client
	clock      sidecar.Clock

	stateLock  sync.Mutex
	entries    map[connect.Id]*sidecarIdentityClientsEntry
	insertions []sidecarIdentityClientsInsertion
	seq        uint64
}

// A resolver of args. An error names the part that cannot be used, never the bearer.
func NewSidecarIdentityClientsResolver(args *SidecarIdentityClientsResolverArgs) (*SidecarIdentityClientsResolver, error) {
	if args == nil {
		return nil, fmt.Errorf("%w: no args", ErrSidecarIdentityClientsGate)
	}
	settings := args.Settings
	if settings == nil {
		settings = DefaultSidecarIdentityClientsResolverSettings()
	}
	format := settings.Format
	if format == nil {
		format = DefaultSidecarIdentityClientsGateFormatSettings()
	}
	switch {
	case settings.RequestTimeout <= 0 || settings.AnswerMaxByteCount <= 0 || settings.CacheMaxCount <= 0 || format.SidecarIdMaxCount <= 0:
		return nil, fmt.Errorf("%w: settings", ErrSidecarIdentityClientsGate)
	case args.Bearer == "":
		return nil, fmt.Errorf("%w: no bearer", ErrSidecarIdentityClientsGate)
	}
	apiUrl, err := url.Parse(args.ApiUrl)
	if err != nil || (apiUrl.Scheme != "http" && apiUrl.Scheme != "https") || apiUrl.Host == "" || strings.HasSuffix(args.ApiUrl, "/") {
		return nil, fmt.Errorf("%w: api url", ErrSidecarIdentityClientsGate)
	}
	httpClient := args.HttpClient
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{},
			// a redirect is answered as the refusal it is, so the bearer goes to api's own url alone
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	clock := args.Clock
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	return &SidecarIdentityClientsResolver{
		routeUrl:   args.ApiUrl + SidecarIdentityClientsGatePath,
		bearer:     args.Bearer,
		settings:   settings,
		format:     format,
		httpClient: httpClient,
		clock:      clock,
		entries:    map[connect.Id]*sidecarIdentityClientsEntry{},
	}, nil
}

// pushservice.PushClientResolver's: each kept sidecar's client from memory, and the rest from api, in calls of at most
// SidecarIdMaxCount ids, whose answers are kept; a sidecar api omits is absent here too, and kept as absent. A call
// that fails answers its error, and keeps nothing it did not learn.
func (self *SidecarIdentityClientsResolver) PushClientIds(ctx context.Context, sidecarIds []connect.Id) (map[connect.Id]connect.Id, error) {
	clientIds := map[connect.Id]connect.Id{}
	missed := []connect.Id{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		now := self.clock.Now()
		asked := map[connect.Id]bool{}
		for _, sidecarId := range sidecarIds {
			if asked[sidecarId] {
				continue
			}
			asked[sidecarId] = true
			entry, ok := self.entries[sidecarId]
			switch {
			case ok && now.Before(entry.expires):
				if entry.found {
					clientIds[sidecarId] = entry.clientId
				}
			default:
				missed = append(missed, sidecarId)
			}
		}
	}()
	for first := 0; first < len(missed); first += self.format.SidecarIdMaxCount {
		batch := missed[first:min(first+self.format.SidecarIdMaxCount, len(missed))]
		answered, err := self.call(ctx, batch)
		if err != nil {
			return nil, err
		}
		self.keep(batch, answered)
		for sidecarId, clientId := range answered {
			clientIds[sidecarId] = clientId
		}
	}
	return clientIds, nil
}

// Keeps a call's answer: each asked sidecar's client, or its absence, until CacheTimeout from now, the oldest kept
// going first past CacheMaxCount.
func (self *SidecarIdentityClientsResolver) keep(asked []connect.Id, answered map[connect.Id]connect.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	expires := self.clock.Now().Add(self.settings.CacheTimeout)
	for _, sidecarId := range asked {
		self.seq += 1
		clientId, found := answered[sidecarId]
		self.entries[sidecarId] = &sidecarIdentityClientsEntry{clientId: clientId, found: found, expires: expires, seq: self.seq}
		self.insertions = append(self.insertions, sidecarIdentityClientsInsertion{sidecarId: sidecarId, seq: self.seq})
	}
	for self.settings.CacheMaxCount < len(self.entries) && 0 < len(self.insertions) {
		oldest := self.insertions[0]
		self.insertions = self.insertions[1:]
		if entry, ok := self.entries[oldest.sidecarId]; ok && entry.seq == oldest.seq {
			delete(self.entries, oldest.sidecarId)
		}
	}
	// a sidecar asked again leaves its earlier insertion behind: drop those once they outnumber the kept entries
	if 2*max(len(self.entries), 1) < len(self.insertions) {
		live := make([]sidecarIdentityClientsInsertion, 0, len(self.entries))
		for _, insertion := range self.insertions {
			if entry, ok := self.entries[insertion.sidecarId]; ok && entry.seq == insertion.seq {
				live = append(live, insertion)
			}
		}
		self.insertions = live
	}
}

// How many sidecars the resolver keeps now, expired ones not yet evicted included: what a test reads.
func (self *SidecarIdentityClientsResolver) KeptCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.entries)
}

// One call for sidecarIds, at most the format's count, answering each one api resolved.
func (self *SidecarIdentityClientsResolver) call(ctx context.Context, sidecarIds []connect.Id) (map[connect.Id]connect.Id, error) {
	args := &SidecarIdentityClientsGateArgs{SidecarIds: make([]Id, len(sidecarIds))}
	for index, sidecarId := range sidecarIds {
		args.SidecarIds[index] = Id(sidecarId)
	}
	if err := args.Validate(self.format); err != nil {
		return nil, err
	}
	body, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, self.routeUrl, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: the request could not be made", ErrSidecarIdentityClientsGate)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+self.bearer)
	response, err := self.httpClient.Do(request)
	if err != nil {
		// a transport error can quote the url, which holds no secret, but nothing of the request's headers or body
		return nil, fmt.Errorf("%w: api did not answer", ErrSidecarIdentityClientsGate)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: api answered %d", ErrSidecarIdentityClientsGate, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, self.settings.AnswerMaxByteCount+1))
	if err != nil || self.settings.AnswerMaxByteCount < int64(len(data)) {
		return nil, fmt.Errorf("%w: api's answer could not be read", ErrSidecarIdentityClientsGate)
	}
	result := &SidecarIdentityClientsGateResult{}
	if err := json.Unmarshal(data, result); err != nil {
		return nil, fmt.Errorf("%w: api's answer is not the route's", ErrSidecarIdentityClientsGate)
	}
	asked := map[connect.Id]bool{}
	for _, sidecarId := range sidecarIds {
		asked[sidecarId] = true
	}
	clientIds := map[connect.Id]connect.Id{}
	for _, client := range result.Clients {
		// a client of a sidecar this call did not ask about is no answer to it
		if client == nil || !asked[connect.Id(client.SidecarId)] || client.ClientId == (Id{}) {
			return nil, fmt.Errorf("%w: api answered a sidecar the call did not name", ErrSidecarIdentityClientsGate)
		}
		clientIds[connect.Id(client.SidecarId)] = connect.Id(client.ClientId)
	}
	return clientIds, nil
}
