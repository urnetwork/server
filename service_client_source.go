package server

// An edge block's platform clients (TLS-M5-11; §6.10.1, §13.6, §19.4; QD-812, QD-873): how tlsedge, mxedge's
// router and smtpedge, which mount no vault and hold no network-level token, come to hold client jwts in
// urnetwork-edge.
//
//   - The block mounts one credential, the `edge` class's internal gate bearer (internal_gate.go:
//     internal-gate-edge.yml among its secret_files, 256 random bits, api holding only its SHA-256).
//   - POST EdgeClientMintPath on api, which takes only that class, answers one client's short-lived jwt for
//     (block, host, slot, index): the client id is derived, never chosen by the caller, so a restart asks for the
//     ids it held before and no state lives on the edge, and a stolen bearer can mint only the environment's fixed
//     edge clients (model.MintEdgeClient).
//   - api admits only what config/<env>/edge.yml names: the block, a host in the block's list, a slot below the
//     key's SlotCount and an index below the block's K (EdgeClientsConfig). The file's name and layout are this
//     draft's reading, as the decision names what api checks, not where the list lives. A missing or broken file
//     admits nothing.
//
// The derivation, fixed for the life of the environment (DeriveEdgeClientId): the first 16 bytes of
//
//	SHA-256("urnetwork-edge-client-v1" 0x00 <environment> 0x00 <host> 0x00 <block> 0x00 <slot, decimal> 0x00
//	        <index, decimal>)
//
// and the client's device the same under "urnetwork-edge-device-v1". "block" is the edge block's name as edge.yml
// lists it, tlsedge, mxedge or smtpedge, never a warp block name. The slot (QD-873, which amended QD-812's
// derivation before any deployment used it) is the deploy slot warp hands the process in WARP_SLOT, beside
// WARP_BLOCK (LoadEdgeClientSlot): a deploy's old and new process of a block on a host hold different slots,
// alternating per deploy, so the new process connects its own K clients while the old one drains its streams on
// its own clients for drain_s (§13.5), each counting only its own toward readiness, and urnetwork-edge holds at
// most SlotCount times K clients a block and host.
//
// PlatformClientSource is the block's end, sidecar.ServiceClientSource for sidecar.EdgeClientPool: it asks for
// each of its K indexes at start, keeps each answer until it is refreshed, and refreshes each before it lapses,
// at RefreshLifeFraction of the shorter life the answer carries.
//
// Each answer also carries the client's 15-minute service token of audience sockets (QD-1054), since sockets
// takes service tokens alone (QD-1029) and no edge client can use the token exchange: it binds no ClientKey, no
// sidecar and no projection. The source keeps it beside the jwt, renewed with it, and the block's calls to
// sockets present it: tlsedge's and mxedge's router's Finds on the first client's (SocketFindHttpArgs), and
// smtpedge's ur/smtpedge registrations each on its own client's (SocketPlatformRegistrationsArgs). A removed
// edge client is refused at sockets by its client rev/ object (model's edge client removal effect).
//
// A jwt, a sockets token and the bearer are secrets: none is logged (I-7), and log lines name a client by its
// slot and index alone (I-13).

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
	"os"
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

// The environment value warp hands each process its deploy slot in, beside WARP_BLOCK (QD-873).
const EdgeClientSlotEnvName = "WARP_SLOT"

// What an edge block asks of EdgeClientMintPath.
type EdgeClientMintArgs struct {
	// The edge block, as edge.yml lists it: tlsedge, mxedge or smtpedge.
	Block string `json:"block"`
	// The host the block runs on, as edge.yml lists it.
	Host string `json:"host"`
	// The process's deploy slot, from 0 (LoadEdgeClientSlot). A request without it reads 0, as a process without
	// WARP_SLOT does while warp's change that sets it rolls out.
	Slot int `json:"slot"`
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
	// The client's service token of audience sockets (QD-1054): network_id urnetwork-edge's and ur_client_id this
	// client's, with no sidecar, member or scope, signed under the svc- set as the token exchange signs. A secret:
	// never logged.
	SocketsToken string `json:"sockets_token"`
	// When the sockets token ends, as its exp carries it: a service token's 15 minutes after its mint.
	SocketsTokenExpireTimeMillis int64 `json:"sockets_token_expire_time_millis"`
}

// An edge client that edge.yml does not admit, or a file that admits nothing.
var ErrEdgeClientRefused = errors.New("edge client refused")

// The source could not be made or could not answer.
var ErrPlatformClientSource = errors.New("platform client source")

// The limits of an edge client's key, each the format's value (QD-702). api's route and the block's source each
// carry them, so each default lives here alone.
type EdgeClientKeySettings struct {
	// The longest host name: RFC 1035's 253.
	HostMaxByteCount int
	// The longest block name.
	BlockMaxByteCount int
	// The most clients one block may list as its K.
	ClientMaxCount int
	// The deploy slots a block's processes on one host take: 2, a deploy's old process and its new one (QD-873).
	// A slot at or past it is refused, so urnetwork-edge holds at most this many times K clients a block and host.
	// warp hands out the same number (warpctl's DeploySlotSettings.SlotCount). Not positive: no slot is admitted.
	SlotCount int
}

func DefaultEdgeClientKeySettings() *EdgeClientKeySettings {
	return &EdgeClientKeySettings{
		HostMaxByteCount:  253,
		BlockMaxByteCount: 32,
		ClientMaxCount:    64,
		SlotCount:         2,
	}
}

// Whether slot is one the key admits: from 0 and below SlotCount.
func (self *EdgeClientKeySettings) slotValid(slot int) bool {
	return 0 <= slot && slot < self.SlotCount
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

func deriveEdgeId(label string, environment string, host string, block string, slot int, index int) Id {
	hash := sha256.New()
	for _, part := range []string{label, environment, host, block, strconv.Itoa(slot)} {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	hash.Write([]byte(strconv.Itoa(index)))
	var id Id
	copy(id[:], hash.Sum(nil))
	return id
}

// The client id of an edge block's client: see the file header. The same for the life of the environment.
func DeriveEdgeClientId(environment string, host string, block string, slot int, index int) Id {
	return deriveEdgeId(edgeClientIdLabel, environment, host, block, slot, index)
}

// The device the client hangs from, derived the same way under its own label.
func DeriveEdgeDeviceId(environment string, host string, block string, slot int, index int) Id {
	return deriveEdgeId(edgeDeviceIdLabel, environment, host, block, slot, index)
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
	// the key's limits the file was read under, its SlotCount among them
	keySettings *EdgeClientKeySettings
}

// Reads edge.yml. A key it does not know, a block or host that is not a plain name, a K outside
// [1, ClientMaxCount] or a platform url that is not ws or wss refuses the whole file, so a misspelled entry
// admits nothing rather than something else. settings nil takes the defaults.
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
		keySettings:       settings,
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

// edge.yml from config, read under settings, nil taking the defaults. An error names the file and the part, and
// its caller then admits nothing.
func LoadEdgeClientsConfig(settings *EdgeClientKeySettings) (*EdgeClientsConfig, error) {
	resource, err := Config.SimpleResource(edgeClientsConfigResourceName)
	if err != nil {
		return nil, fmt.Errorf("%w: no %s: %v", ErrEdgeClientRefused, edgeClientsConfigResourceName, err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrEdgeClientRefused, edgeClientsConfigResourceName, err)
	}
	return ParseEdgeClientsConfig(data, settings)
}

// Where an edge client connects.
func (self *EdgeClientsConfig) PlatformUrl() string {
	return self.platformUrl
}

// The block's K, 0 for a block the file does not list.
func (self *EdgeClientsConfig) ClientCount(block string) int {
	return self.blockClientCounts[block]
}

// Whether the file names the block, the host among the block's hosts, a slot below the key's SlotCount and an
// index below the block's K. A refusal says which part, never more.
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
	if !self.keySettings.slotValid(args.Slot) {
		return fmt.Errorf("%w: slot", ErrEdgeClientRefused)
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
	// The refresh point: the part of an answer's life, from the answer to the end of the shorter-lived of its jwt
	// and its sockets token, after which the source asks for the next. 0.5, as a client token refreshes between
	// 40% and 50% of its life: with a 15-minute sockets token beside an hour's jwt, an ask every 7.5 minutes.
	RefreshLifeFraction float64
	// How long a refused or failed ask waits before the next: 5 s.
	RetryInterval time.Duration
	// The longest one ask may take: 10 s.
	RequestTimeout time.Duration
	// The most of an answer's body read: 16 KiB, a jwt and two short strings many times over.
	AnswerMaxByteCount int64
	// The key's limits, the same as api's: its ClientMaxCount bounds ClientCount and its SlotCount the slot.
	Key *EdgeClientKeySettings
	// Whether a process with no WARP_SLOT refuses to start (LoadEdgeClientSlot). false while warp's change that
	// sets WARP_SLOT rolls out (QD-873): a missing value then reads slot 0, and the log says so. A later change
	// makes it true once every host's warp sets it.
	SlotRequired bool
}

func DefaultPlatformClientSourceSettings() *PlatformClientSourceSettings {
	return &PlatformClientSourceSettings{
		ClientCount:         4,
		RefreshLifeFraction: 0.5,
		RetryInterval:       5 * time.Second,
		RequestTimeout:      10 * time.Second,
		AnswerMaxByteCount:  16 * 1024,
		Key:                 DefaultEdgeClientKeySettings(),
		SlotRequired:        false,
	}
}

// The process's deploy slot from WARP_SLOT, the decimal warp sets beside WARP_BLOCK and alternates per deploy
// (QD-873), which the block passes to NewPlatformClientSource. A value that is not a decimal in [0, SlotCount) is
// refused, so a process never holds a slot api would refuse or one that names another. With none set, settings'
// SlotRequired refuses; otherwise the slot reads 0 and the log says so, which holds only while warp's change
// rolls out. settings nil takes the defaults. The error names the variable, and a value is not secret.
func LoadEdgeClientSlot(settings *PlatformClientSourceSettings) (int, error) {
	value, set := os.LookupEnv(EdgeClientSlotEnvName)
	return parseEdgeClientSlot(value, set, settings)
}

// LoadEdgeClientSlot on a value read: set false, or an empty value, is no value at all.
func parseEdgeClientSlot(value string, set bool, settings *PlatformClientSourceSettings) (int, error) {
	if settings == nil {
		settings = DefaultPlatformClientSourceSettings()
	}
	keySettings := settings.Key
	if keySettings == nil {
		keySettings = DefaultEdgeClientKeySettings()
	}
	if !set || value == "" {
		if settings.SlotRequired {
			return 0, fmt.Errorf("%w: %s is not set", ErrPlatformClientSource, EdgeClientSlotEnvName)
		}
		glog.Warningf("[edge]%s is not set: the process holds slot 0 until warp sets it\n", EdgeClientSlotEnvName)
		return 0, nil
	}
	slot, err := strconv.Atoi(value)
	// the round trip refuses a sign, a leading zero or a space, so one slot has one spelling
	if err != nil || strconv.Itoa(slot) != value || !keySettings.slotValid(slot) {
		return 0, fmt.Errorf("%w: %s=%q is not a slot below %d", ErrPlatformClientSource, EdgeClientSlotEnvName, value, keySettings.SlotCount)
	}
	return slot, nil
}

// What NewPlatformClientSource takes.
type PlatformClientSourceArgs struct {
	// api's base url, with no trailing slash, which each client also reaches api at.
	ApiUrl string
	// The edge block and the host it runs on, as edge.yml lists them.
	Block string
	Host  string
	// The process's deploy slot (LoadEdgeClientSlot), below the key's SlotCount: the other process of a deploy
	// on this host holds the other slot, so the two hold disjoint clients.
	Slot int
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
	// the client's service token of audience sockets, a secret, and when it ends
	socketsToken           string
	socketsTokenExpireTime time.Time
	// when the jwt ends, and when the source asks for the next: RefreshLifeFraction into the shorter life
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

// The source for one edge block's process, which starts asking for each of its K indexes at once and keeps each
// fresh until ctx ends or Close. It returns before any answer: ServiceClient waits for an index's first.
func NewPlatformClientSource(ctx context.Context, args *PlatformClientSourceArgs) (*PlatformClientSource, error) {
	if args == nil {
		return nil, fmt.Errorf("%w: no args", ErrPlatformClientSource)
	}
	settings := args.Settings
	if settings == nil {
		settings = DefaultPlatformClientSourceSettings()
	}
	keySettings := settings.Key
	if keySettings == nil {
		keySettings = DefaultEdgeClientKeySettings()
	}
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
	case !keySettings.slotValid(args.Slot):
		return nil, fmt.Errorf("%w: slot", ErrPlatformClientSource)
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
	body, err := json.Marshal(&EdgeClientMintArgs{
		Block: self.args.Block,
		Host:  self.args.Host,
		Slot:  self.args.Slot,
		Index: index,
	})
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
	// the token sockets takes (QD-1054): an answer without one is no client's, since the block's every call to
	// sockets needs it
	if result.SocketsToken == "" {
		return nil, fmt.Errorf("%w: api's answer holds no sockets token", ErrPlatformClientSource)
	}
	socketsTokenExpireTime := time.UnixMilli(result.SocketsTokenExpireTimeMillis)
	socketsTokenLife := socketsTokenExpireTime.Sub(askTime)
	if socketsTokenLife <= 0 {
		return nil, fmt.Errorf("%w: api's answer holds a sockets token that has ended", ErrPlatformClientSource)
	}
	return &platformClientCredential{
		auth: &sidecar.ServiceClientAuth{
			ClientId:    connect.Id(clientId),
			ByJwt:       result.ByClientJwt,
			ApiUrl:      self.args.ApiUrl,
			PlatformUrl: result.PlatformUrl,
		},
		socketsToken:           result.SocketsToken,
		socketsTokenExpireTime: socketsTokenExpireTime,
		expireTime:             expireTime,
		refreshTime:            askTime.Add(time.Duration(float64(min(life, socketsTokenLife)) * self.settings.RefreshLifeFraction)),
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
			glog.Infof("[edge]slot %d's client %d's credential was not answered: %v\n", self.args.Slot, index, err)
			select {
			case <-self.ctx.Done():
				return
			case <-self.clock.After(self.settings.RetryInterval):
			}
		}
	}
}

// The index's credential: the one held while it is before its refresh point, and otherwise a new one asked of
// api now. When that ask fails, the held one still answers until the end heldEnd reads of it passes, so an api
// outage shorter than the rest of what the caller presents costs it nothing.
func (self *PlatformClientSource) current(ctx context.Context, clientIndex int, heldEnd func(*platformClientCredential) time.Time) (*platformClientCredential, error) {
	if clientIndex < 0 || len(self.indexes) <= clientIndex {
		return nil, fmt.Errorf("%w: no client %d", ErrPlatformClientSource, clientIndex)
	}
	if self.ctx.Err() != nil {
		return nil, fmt.Errorf("%w: closed", ErrPlatformClientSource)
	}
	held := self.held(clientIndex)
	if held != nil && self.clock.Now().Before(held.refreshTime) {
		return held, nil
	}
	credential, err := self.refresh(ctx, clientIndex, held)
	if err != nil {
		if held == nil || !self.clock.Now().Before(heldEnd(held)) {
			return nil, err
		}
		return held, nil
	}
	return credential, nil
}

// See sidecar.ServiceClientSource: the index's credential as current answers it, an outage ridden on the held
// jwt until it ends.
func (self *PlatformClientSource) ServiceClient(ctx context.Context, clientIndex int) (*sidecar.ServiceClientAuth, error) {
	credential, err := self.current(ctx, clientIndex, func(held *platformClientCredential) time.Time {
		return held.expireTime
	})
	if err != nil {
		return nil, err
	}
	auth := *credential.auth
	return &auth, nil
}

// The index's service token of audience sockets (QD-1054), as current answers it, an outage ridden on the held
// token until it ends. A secret: the caller presents it to sockets alone and never logs it.
func (self *PlatformClientSource) SocketsToken(ctx context.Context, clientIndex int) (string, error) {
	credential, err := self.current(ctx, clientIndex, func(held *platformClientCredential) time.Time {
		return held.socketsTokenExpireTime
	})
	if err != nil {
		return "", err
	}
	return credential.socketsToken, nil
}

// The sockets token of the index whose client is clientId, as SocketsToken answers it: what a client's own call
// to sockets presents. A client no index has been answered for is refused.
func (self *PlatformClientSource) ClientSocketsToken(ctx context.Context, clientId sidecar.Id) (string, error) {
	clientIndex := func() int {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for index, state := range self.indexes {
			if state.clientId != (connect.Id{}) && state.clientId == connect.Id(clientId) {
				return index
			}
		}
		return -1
	}()
	if clientIndex < 0 {
		return "", fmt.Errorf("%w: not one of the source's clients", ErrPlatformClientSource)
	}
	return self.SocketsToken(ctx, clientIndex)
}

// How an edge block's dialer sends its Finds (tlsedge's, mxedge's router's) to the sockets service at
// socketsUrl: each Find on the sockets token of the first of the block's K clients, since a Find is the block's
// own call, naming all K (§9.6). Carried by a transport of its own.
func (self *PlatformClientSource) SocketFindHttpArgs(socketsUrl string) *sidecar.SocketFindHttpArgs {
	return &sidecar.SocketFindHttpArgs{
		SocketsUrl: socketsUrl,
		Transport:  &http.Transport{},
		Bearer: func(ctx context.Context) (string, error) {
			return self.SocketsToken(ctx, 0)
		},
	}
}

// How an edge block's platform listener (smtpedge's ur/smtpedge) sends each client's registrations to the
// sockets service at socketsUrl: each on that client's own sockets token, since a registration is the client's
// own lease (§9.5). Carried by a transport of its own.
func (self *PlatformClientSource) SocketPlatformRegistrationsArgs(socketsUrl string) *sidecar.SocketPlatformRegistrationsArgs {
	return &sidecar.SocketPlatformRegistrationsArgs{
		SocketsUrl: socketsUrl,
		Transport:  &http.Transport{},
		Bearer:     self.ClientSocketsToken,
	}
}

// Whether every index holds a credential whose jwt and sockets token have not ended: the process's own K
// clients, not another slot's.
func (self *PlatformClientSource) Ready() bool {
	if self.ctx.Err() != nil {
		return false
	}
	now := self.clock.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, state := range self.indexes {
		if state.credential == nil || !now.Before(state.credential.expireTime) || !now.Before(state.credential.socketsTokenExpireTime) {
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
