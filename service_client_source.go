package server

// An edge block's platform clients (TLS-M5-11; §6.10.1, §13.6, §19.4; QD-812): how tlsedge, mxedge's router and
// smtpedge, which mount no vault and hold no network-level token, come to hold client jwts in urnetwork-edge.
//
//   - The block mounts one credential, the `edge` class's internal gate bearer (internal_gate.go:
//     internal-gate-edge.yml among its secret_files, 256 random bits, api holding only its SHA-256).
//   - POST EdgeClientMintPath on api, which takes only that class, answers one client's short-lived jwt for
//     (block, host, index): the client id is derived, never chosen by the caller, so a restart asks for the ids it
//     held before and no state lives on the edge, and a stolen bearer can mint only the environment's fixed edge
//     clients (model.MintEdgeClient).
//   - api admits only what config/<env>/edge.yml names: the block, a host in the block's list and an index below
//     its K (EdgeClientsConfig). The file's name and layout are this draft's reading, as the decision names what
//     api checks, not where the list lives. A missing or broken file admits nothing.
//
// The derivation, fixed for the life of the environment (DeriveEdgeClientId): the first 16 bytes of
//
//	SHA-256("urnetwork-edge-client-v1" 0x00 <environment> 0x00 <block> 0x00 <host> 0x00 <index, decimal>)
//
// and the client's device the same under "urnetwork-edge-device-v1". "block" is the edge block's name as edge.yml
// lists it, tlsedge, mxedge or smtpedge: one such block runs on a host (§13.6), so no warp block name enters.
//
// PlatformClientSource is the block's end, sidecar.ServiceClientSource for sidecar.EdgeClientPool: it asks for
// each of its K indexes at start, keeps each answer until its jwt is refreshed, and refreshes each before it
// lapses, at RefreshLifeFraction of its life. A jwt and the bearer are secrets: neither is logged (I-7), and log
// lines name a client by its index alone (I-13).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
)

// The route that mints an edge client's jwt, under the internal gate's prefix.
const EdgeClientMintPath = InternalGatePathPrefix + "edge-client"

// The file under config/<env> that names each edge block's hosts and K.
const edgeClientsConfigResourceName = "edge.yml"

// The derivation's labels: a change to either is a new set of clients.
const (
	edgeClientIdLabel = "urnetwork-edge-client-v1"
	edgeDeviceIdLabel = "urnetwork-edge-device-v1"
)

// What an edge block asks of EdgeClientMintPath.
type EdgeClientMintArgs struct {
	// The edge block, as edge.yml lists it: tlsedge, mxedge or smtpedge.
	Block string `json:"block"`
	// The host the block runs on, as edge.yml lists it.
	Host string `json:"host"`
	// Which of the block's K clients, from 0.
	Index int `json:"index"`
}

// What EdgeClientMintPath answers.
type EdgeClientMintResult struct {
	ClientId string `json:"client_id"`
	// A secret: never logged.
	ByClientJwt string `json:"by_client_jwt"`
	// When the jwt ends, with no grace.
	ExpireTimeMillis int64 `json:"expire_time_millis"`
	// Where the client connects, ws or wss.
	PlatformUrl string `json:"platform_url"`
}

// An edge client that edge.yml does not admit, or a file that admits nothing.
var ErrEdgeClientRefused = errors.New("edge client refused")

// The source could not be made or could not answer.
var ErrPlatformClientSource = errors.New("platform client source")

// The limits of an edge client's key, each the format's value (QD-702).
type EdgeClientKeySettings struct {
	// The longest host name: RFC 1035's 253.
	HostMaxByteCount int
	// The longest block name.
	BlockMaxByteCount int
	// The most clients one block may list as its K.
	ClientMaxCount int
}

func DefaultEdgeClientKeySettings() *EdgeClientKeySettings {
	return &EdgeClientKeySettings{
		HostMaxByteCount:  253,
		BlockMaxByteCount: 32,
		ClientMaxCount:    64,
	}
}

// Whether name is lowercase letters, digits, dots and hyphens, at most maxByteCount: what a block or a host is
// spelled in, so no separator of the derivation can appear in one.
func edgeClientNameValid(name string, maxByteCount int) bool {
	if name == "" || maxByteCount < len(name) {
		return false
	}
	for i := 0; i < len(name); i += 1 {
		c := name[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

func deriveEdgeId(label string, environment string, block string, host string, index int) Id {
	hash := sha256.New()
	for _, part := range []string{label, environment, block, host} {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	hash.Write([]byte(strconv.Itoa(index)))
	var id Id
	copy(id[:], hash.Sum(nil))
	return id
}

// The client id of an edge block's client: see the file header. The same for the life of the environment.
func DeriveEdgeClientId(environment string, block string, host string, index int) Id {
	return deriveEdgeId(edgeClientIdLabel, environment, block, host, index)
}

// The device the client hangs from, derived the same way under its own label.
func DeriveEdgeDeviceId(environment string, block string, host string, index int) Id {
	return deriveEdgeId(edgeDeviceIdLabel, environment, block, host, index)
}

// One edge block as edge.yml lists it.
type edgeClientsBlockYaml struct {
	Clients int      `yaml:"clients"`
	Hosts   []string `yaml:"hosts"`
}

// edge.yml:
//
//	platform_url: wss://connect.<domain>
//	blocks:
//	  tlsedge: {clients: 4, hosts: [<host>, ...]}
type edgeClientsConfigYaml struct {
	PlatformUrl string                           `yaml:"platform_url"`
	Blocks      map[string]*edgeClientsBlockYaml `yaml:"blocks"`
}

// One read of edge.yml. Immutable once read.
type EdgeClientsConfig struct {
	platformUrl string
	// block -> K
	blockClientCounts map[string]int
	// block -> its hosts
	blockHosts map[string][]string
}

// Reads edge.yml. A key it does not know, a block or host that is not a plain name, a K outside
// [1, ClientMaxCount] or a platform url that is not ws or wss refuses the whole file, so a misspelled entry
// admits nothing rather than something else.
func ParseEdgeClientsConfig(data []byte, settings *EdgeClientKeySettings) (*EdgeClientsConfig, error) {
	if settings == nil {
		settings = DefaultEdgeClientKeySettings()
	}
	refuse := func(format string, arguments ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrEdgeClientRefused, edgeClientsConfigResourceName, fmt.Sprintf(format, arguments...))
	}
	file := &edgeClientsConfigYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(file); err != nil {
		return nil, refuse("%v", err)
	}
	platformUrl, err := url.Parse(file.PlatformUrl)
	if err != nil || (platformUrl.Scheme != "ws" && platformUrl.Scheme != "wss") || platformUrl.Host == "" {
		return nil, refuse("platform_url is not a ws or wss url")
	}
	self := &EdgeClientsConfig{
		platformUrl:       file.PlatformUrl,
		blockClientCounts: map[string]int{},
		blockHosts:        map[string][]string{},
	}
	for block, entry := range file.Blocks {
		if !edgeClientNameValid(block, settings.BlockMaxByteCount) || entry == nil {
			return nil, refuse("a block that is not a plain name with an entry")
		}
		if entry.Clients < 1 || settings.ClientMaxCount < entry.Clients {
			return nil, refuse("%s: clients outside [1, %d]", block, settings.ClientMaxCount)
		}
		for _, host := range entry.Hosts {
			if !edgeClientNameValid(host, settings.HostMaxByteCount) {
				return nil, refuse("%s: a host that is not a plain name", block)
			}
		}
		self.blockClientCounts[block] = entry.Clients
		self.blockHosts[block] = slices.Clone(entry.Hosts)
	}
	return self, nil
}

// edge.yml from config. An error names the file and the part, and its caller then admits nothing.
func LoadEdgeClientsConfig() (*EdgeClientsConfig, error) {
	resource, err := Config.SimpleResource(edgeClientsConfigResourceName)
	if err != nil {
		return nil, fmt.Errorf("%w: no %s: %v", ErrEdgeClientRefused, edgeClientsConfigResourceName, err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrEdgeClientRefused, edgeClientsConfigResourceName, err)
	}
	return ParseEdgeClientsConfig(data, nil)
}

// Where an edge client connects.
func (self *EdgeClientsConfig) PlatformUrl() string {
	return self.platformUrl
}

// The block's K, 0 for a block the file does not list.
func (self *EdgeClientsConfig) ClientCount(block string) int {
	return self.blockClientCounts[block]
}

// Whether the file names the block, the host among the block's hosts, and an index below the block's K. A refusal
// says which part, never more.
func (self *EdgeClientsConfig) Admits(args *EdgeClientMintArgs) error {
	if args == nil {
		return fmt.Errorf("%w: no client", ErrEdgeClientRefused)
	}
	clientCount, ok := self.blockClientCounts[args.Block]
	if !ok {
		return fmt.Errorf("%w: block", ErrEdgeClientRefused)
	}
	if !slices.Contains(self.blockHosts[args.Block], args.Host) {
		return fmt.Errorf("%w: host", ErrEdgeClientRefused)
	}
	if args.Index < 0 || clientCount <= args.Index {
		return fmt.Errorf("%w: index", ErrEdgeClientRefused)
	}
	return nil
}

// A platform client source's tunables.
type PlatformClientSourceSettings struct {
	// K, the indexes the source keeps: the pool's ClientCount.
	ClientCount int
	// The refresh point: the part of a jwt's life, from the answer that carried it to its end, after which the
	// source asks for the next. 0.5, as a client token refreshes between 40% and 50% of its life.
	RefreshLifeFraction float64
	// How long a refused or failed ask waits before the next: 5 s.
	RetryInterval time.Duration
	// The longest one ask may take: 10 s.
	RequestTimeout time.Duration
	// The most of an answer's body read: 16 KiB, a jwt and two short strings many times over.
	AnswerMaxByteCount int64
}

func DefaultPlatformClientSourceSettings() *PlatformClientSourceSettings {
	return &PlatformClientSourceSettings{
		ClientCount:         4,
		RefreshLifeFraction: 0.5,
		RetryInterval:       5 * time.Second,
		RequestTimeout:      10 * time.Second,
		AnswerMaxByteCount:  16 * 1024,
	}
}

// What NewPlatformClientSource takes.
type PlatformClientSourceArgs struct {
	// api's base url, with no trailing slash, which each client also reaches api at.
	ApiUrl string
	// The edge block and the host it runs on, as edge.yml lists them.
	Block string
	Host  string
	// The block's `edge` bearer, read once at start (LoadInternalGateCredential(InternalGateCallerEdge)).
	Bearer string
	// Carries every ask. nil: a client of its own with no proxy from the environment.
	HttpClient *http.Client
	// nil: real time.
	Clock sidecar.Clock
	// nil: the defaults.
	Settings *PlatformClientSourceSettings
}

// One index's last answer.
type platformClientCredential struct {
	auth *sidecar.ServiceClientAuth
	// when the jwt ends, and when the source asks for the next
	expireTime  time.Time
	refreshTime time.Time
}

// One index's state. The ask lock keeps one ask in flight for an index, and is never held with stateLock.
type platformClientIndex struct {
	askLock sync.Mutex
	// fixed once the first answer names it
	clientId   connect.Id
	credential *platformClientCredential
}

// sidecar.ServiceClientSource over api's edge client route. Safe for concurrent use.
type PlatformClientSource struct {
	ctx        context.Context
	cancel     context.CancelFunc
	args       *PlatformClientSourceArgs
	settings   *PlatformClientSourceSettings
	clock      sidecar.Clock
	httpClient *http.Client
	mintUrl    string

	stateLock sync.Mutex
	indexes   []*platformClientIndex

	// Rises as each answer is kept, after it is, so a wait can see one land and then move a fake clock.
	answerCount *sidecar.MonitorValue[uint64]

	loopGroup sync.WaitGroup
	closeOnce sync.Once
}

var _ sidecar.ServiceClientSource = (*PlatformClientSource)(nil)

// The source for one edge block, which starts asking for each of its K indexes at once and keeps each fresh until
// ctx ends or Close. It returns before any answer: ServiceClient waits for an index's first.
func NewPlatformClientSource(ctx context.Context, args *PlatformClientSourceArgs) (*PlatformClientSource, error) {
	if args == nil {
		return nil, fmt.Errorf("%w: no args", ErrPlatformClientSource)
	}
	settings := args.Settings
	if settings == nil {
		settings = DefaultPlatformClientSourceSettings()
	}
	keySettings := DefaultEdgeClientKeySettings()
	switch {
	case settings.ClientCount < 1 || keySettings.ClientMaxCount < settings.ClientCount:
		return nil, fmt.Errorf("%w: client count", ErrPlatformClientSource)
	case settings.RefreshLifeFraction <= 0 || 1 <= settings.RefreshLifeFraction:
		return nil, fmt.Errorf("%w: refresh point", ErrPlatformClientSource)
	case settings.RetryInterval <= 0 || settings.RequestTimeout <= 0 || settings.AnswerMaxByteCount <= 0:
		return nil, fmt.Errorf("%w: settings", ErrPlatformClientSource)
	case !edgeClientNameValid(args.Block, keySettings.BlockMaxByteCount):
		return nil, fmt.Errorf("%w: block", ErrPlatformClientSource)
	case !edgeClientNameValid(args.Host, keySettings.HostMaxByteCount):
		return nil, fmt.Errorf("%w: host", ErrPlatformClientSource)
	case args.Bearer == "":
		return nil, fmt.Errorf("%w: no bearer", ErrPlatformClientSource)
	}
	apiUrl, err := url.Parse(args.ApiUrl)
	if err != nil || (apiUrl.Scheme != "http" && apiUrl.Scheme != "https") || apiUrl.Host == "" || strings.HasSuffix(args.ApiUrl, "/") {
		return nil, fmt.Errorf("%w: api url", ErrPlatformClientSource)
	}
	httpClient := args.HttpClient
	if httpClient == nil {
		httpClient = &http.Client{Transport: &http.Transport{}}
	}
	clock := args.Clock
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &PlatformClientSource{
		ctx:         cancelCtx,
		cancel:      cancel,
		args:        args,
		settings:    settings,
		clock:       clock,
		httpClient:  httpClient,
		mintUrl:     args.ApiUrl + EdgeClientMintPath,
		answerCount: sidecar.NewMonitorValue(uint64(0)),
	}
	for range settings.ClientCount {
		self.indexes = append(self.indexes, &platformClientIndex{})
	}
	for index := range settings.ClientCount {
		self.loopGroup.Add(1)
		go func() {
			defer self.loopGroup.Done()
			self.keepFresh(index)
		}()
	}
	return self, nil
}

// A context that ends when ctx or the source does, or RequestTimeout passes on the source's clock.
func (self *PlatformClientSource) askContext(ctx context.Context) (context.Context, func()) {
	askCtx, cancel := context.WithCancel(ctx)
	stopTimeout := self.clock.AfterFunc(self.settings.RequestTimeout, cancel)
	stopSource := context.AfterFunc(self.ctx, cancel)
	return askCtx, func() {
		stopTimeout()
		stopSource()
		cancel()
	}
}

// One ask of api for index. An error names the part that failed, never the bearer or a jwt.
func (self *PlatformClientSource) ask(ctx context.Context, index int) (*platformClientCredential, error) {
	askCtx, done := self.askContext(ctx)
	defer done()
	body, err := json.Marshal(&EdgeClientMintArgs{Block: self.args.Block, Host: self.args.Host, Index: index})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(askCtx, http.MethodPost, self.mintUrl, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: the request could not be made", ErrPlatformClientSource)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+self.args.Bearer)
	askTime := self.clock.Now()
	response, err := self.httpClient.Do(request)
	if err != nil {
		// a transport error can quote the url, which holds no secret, but nothing of the request's headers
		return nil, fmt.Errorf("%w: api did not answer", ErrPlatformClientSource)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: api answered %d", ErrPlatformClientSource, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, self.settings.AnswerMaxByteCount+1))
	if err != nil || self.settings.AnswerMaxByteCount < int64(len(data)) {
		return nil, fmt.Errorf("%w: api's answer could not be read", ErrPlatformClientSource)
	}
	result := &EdgeClientMintResult{}
	if err := json.Unmarshal(data, result); err != nil {
		return nil, fmt.Errorf("%w: api's answer is not a client", ErrPlatformClientSource)
	}
	clientId, err := ParseId(result.ClientId)
	if err != nil || clientId == (Id{}) || result.ByClientJwt == "" {
		return nil, fmt.Errorf("%w: api's answer holds no client id or jwt", ErrPlatformClientSource)
	}
	platformUrl, err := url.Parse(result.PlatformUrl)
	if err != nil || (platformUrl.Scheme != "ws" && platformUrl.Scheme != "wss") || platformUrl.Host == "" {
		return nil, fmt.Errorf("%w: api's answer holds no platform url", ErrPlatformClientSource)
	}
	expireTime := time.UnixMilli(result.ExpireTimeMillis)
	life := expireTime.Sub(askTime)
	if life <= 0 {
		return nil, fmt.Errorf("%w: api's answer holds a jwt that has ended", ErrPlatformClientSource)
	}
	return &platformClientCredential{
		auth: &sidecar.ServiceClientAuth{
			ClientId:    connect.Id(clientId),
			ByJwt:       result.ByClientJwt,
			ApiUrl:      self.args.ApiUrl,
			PlatformUrl: result.PlatformUrl,
		},
		expireTime:  expireTime,
		refreshTime: askTime.Add(time.Duration(float64(life) * self.settings.RefreshLifeFraction)),
	}, nil
}

// The index's credential as last answered, nil before the first.
func (self *PlatformClientSource) held(index int) *platformClientCredential {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.indexes[index].credential
}

// Asks for the index's next credential and keeps it, unless one newer than `than` is already held, which another
// caller's ask brought. An answer naming another client id than the index's first is refused: a client's id is
// the same for the process's life (sidecar.ServiceClientAuth).
func (self *PlatformClientSource) refresh(ctx context.Context, index int, than *platformClientCredential) (*platformClientCredential, error) {
	state := self.indexes[index]
	state.askLock.Lock()
	defer state.askLock.Unlock()
	if held := self.held(index); held != nil && held != than {
		return held, nil
	}
	credential, err := self.ask(ctx, index)
	if err != nil {
		return nil, err
	}
	err = func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if state.clientId == (connect.Id{}) {
			state.clientId = credential.auth.ClientId
		} else if state.clientId != credential.auth.ClientId {
			return fmt.Errorf("%w: api answered client %d another id", ErrPlatformClientSource, index)
		}
		state.credential = credential
		return nil
	}()
	if err != nil {
		return nil, err
	}
	self.answerCount.Update(func(count uint64) uint64 { return count + 1 })
	return credential, nil
}

// Keeps one index fresh until the source ends: the first ask at once, each later one at its credential's refresh
// point, and a failed ask again after RetryInterval.
func (self *PlatformClientSource) keepFresh(index int) {
	for {
		held := self.held(index)
		wait := time.Duration(0)
		if held != nil {
			wait = held.refreshTime.Sub(self.clock.Now())
		}
		if 0 < wait {
			select {
			case <-self.ctx.Done():
				return
			case <-self.clock.After(wait):
			}
			continue
		}
		if _, err := self.refresh(self.ctx, index, held); err != nil {
			if self.ctx.Err() != nil {
				return
			}
			// the error names a part or a status, never a secret
			glog.Infof("[edge]client %d's credential was not answered: %v\n", index, err)
			select {
			case <-self.ctx.Done():
				return
			case <-self.clock.After(self.settings.RetryInterval):
			}
		}
	}
}

// See sidecar.ServiceClientSource: the index's credential, the one held while it is before its refresh point,
// and otherwise a new one asked of api now. When that ask fails, the held one still answers until its jwt ends:
// an api outage shorter than the rest of a jwt's life costs no client.
func (self *PlatformClientSource) ServiceClient(ctx context.Context, clientIndex int) (*sidecar.ServiceClientAuth, error) {
	if clientIndex < 0 || len(self.indexes) <= clientIndex {
		return nil, fmt.Errorf("%w: no client %d", ErrPlatformClientSource, clientIndex)
	}
	if self.ctx.Err() != nil {
		return nil, fmt.Errorf("%w: closed", ErrPlatformClientSource)
	}
	held := self.held(clientIndex)
	now := self.clock.Now()
	if held != nil && now.Before(held.refreshTime) {
		auth := *held.auth
		return &auth, nil
	}
	credential, err := self.refresh(ctx, clientIndex, held)
	if err != nil {
		if held == nil || !self.clock.Now().Before(held.expireTime) {
			return nil, err
		}
		credential = held
	}
	auth := *credential.auth
	return &auth, nil
}

// Whether every index holds a credential whose jwt has not ended.
func (self *PlatformClientSource) Ready() bool {
	if self.ctx.Err() != nil {
		return false
	}
	now := self.clock.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, state := range self.indexes {
		if state.credential == nil || !now.Before(state.credential.expireTime) {
			return false
		}
	}
	return true
}

// Ends the source's asks and joins its loops. Idempotent.
func (self *PlatformClientSource) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		self.loopGroup.Wait()
	})
}
