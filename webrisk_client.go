package server

// Google Web Risk's Update API, the client the abuse worker's hourly check runs (ABUSE-M5-13; §21.6, §2.8 ext-6,
// QD-554). Safe Browsing is non-commercial only, so this is Web Risk, and of Web Risk only the Update API: the Lookup
// API takes whole urls, so it is never called.
//
// The client keeps one local list of hash prefixes for each threat type it watches, in this process's memory and
// nowhere else, so a cold start fetches each list in full and no row or object holds them. threatLists.computeDiff
// keeps a list current in raw form, never Rice: a reset clears the list first, a diff removes entries by their
// indices into the sorted list, then adds the new prefixes, and either way the result must hash to the SHA-256 the
// answer gives over the sorted prefixes. A list that does not is dropped and fetched in full once more, and a second
// mismatch fails the update with no list kept. A list is not asked again before the time the API recommends.
//
// A lookup takes the SHA-256 of one url expression (WebRiskHostExpressions). No request leaves for a hash no local
// list holds a prefix of. For one that matches, hashes.search is asked for the shortest matching prefix alone, never
// more of the hash, and answers the full hashes listed under it; the hash is listed only if it is among them. So the
// API sees list updates, and the prefixes of names that matched a local prefix, as ext-6 says, and never a name that
// matched nothing. Answers are cached as the API asks: a listed full hash until its expire time, any other hash of
// the prefix until the negative expire time.
//
// The key rides the X-Goog-Api-Key header, never the url, and no error carries a url, a prefix, a name or the key
// (I-13). Safe for concurrent use: a lookup reads a list as one update left it, never a list mid-update.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
)

// One of Web Risk's threat lists, as the API names it.
type WebRiskThreatType string

const (
	WebRiskThreatTypeMalware                           WebRiskThreatType = "MALWARE"
	WebRiskThreatTypeSocialEngineering                 WebRiskThreatType = "SOCIAL_ENGINEERING"
	WebRiskThreatTypeUnwantedSoftware                  WebRiskThreatType = "UNWANTED_SOFTWARE"
	WebRiskThreatTypeSocialEngineeringExtendedCoverage WebRiskThreatType = "SOCIAL_ENGINEERING_EXTENDED_COVERAGE"
)

// The vault file that holds §21.16's Web Risk key, and the key's field in it.
const (
	WebRiskKeyResourceName = "abuse-web-risk.yml"
	webRiskKeyField        = "api_key"
)

// The API's names: its paths, the header the key rides, the one compression asked for and the answer types.
const (
	webRiskComputeDiffPath   = "/v1/threatLists:computeDiff"
	webRiskSearchHashesPath  = "/v1/hashes:search"
	webRiskApiKeyHeaderName  = "X-Goog-Api-Key"
	webRiskCompressionRaw    = "RAW"
	webRiskResponseTypeDiff  = "DIFF"
	webRiskResponseTypeReset = "RESET"
)

var (
	// The API gave no usable answer: unreachable, refused, past a bound, or not in its own form. Nothing of the
	// answer is kept.
	ErrWebRiskSource = errors.New("web risk: the source gave no usable answer")
	// No key, or settings no client may run on.
	ErrWebRiskClientRefused = errors.New("web risk: client refused")
	// A list's prefixes do not hash to the checksum the API gave for them.
	ErrWebRiskChecksum = errors.New("web risk: a list does not match its checksum")
)

// Tunables of the client. The prefix sizes and the host label count are the protocol's, so a deployment changes them
// only with the API.
type WebRiskClientSettings struct {
	// The API's origin: https, with no path.
	BaseUrl string
	// The threat lists kept locally and searched, at least one.
	ThreatTypes []WebRiskThreatType
	// One request's deadline.
	RequestTimeout time.Duration
	// The most bytes one computeDiff answer is read to: a full list of MaxDatabaseEntries of the longest prefixes,
	// in base64, with room for its framing.
	DiffResponseMaxByteCount int64
	// The most bytes one hashes.search answer is read to.
	SearchResponseMaxByteCount int64
	// The API's constraints.maxDatabaseEntries: the most entries it sends a list, a power of two from 2^10 to 2^20.
	// Zero asks for no bound.
	MaxDatabaseEntries int
	// The API's constraints.maxDiffEntries: the most entries one diff carries, in the same form. Zero asks for no
	// bound.
	MaxDiffEntries int
	// The most prefixes one local list holds, whatever the API sends: an update past it fails, keeping the list as it
	// was.
	ListPrefixMaxCount int
	// The shortest and the longest hash prefix the API sends, in bytes: the protocol's 4 and 32.
	HashPrefixMinByteCount int
	HashPrefixMaxByteCount int
	// How many of a host's last labels its suffixes are drawn from: the protocol's 5, so a host is looked up as
	// itself and at most four suffixes, never its top-level label alone.
	HostLabelMaxCount int
	// The most prefixes whose search answers are kept. An answer past it drops the expired ones first, then all of
	// them. Non-positive keeps none, so every matching lookup asks again.
	SearchCacheMaxCount int
}

// Web Risk's public endpoint and §21.6's two lists: phishing (social engineering) and malware. Unwanted software is
// a list a deployment adds; each list a hit names becomes §21.4's category in the abuse worker.
func DefaultWebRiskClientSettings() *WebRiskClientSettings {
	return &WebRiskClientSettings{
		BaseUrl:                    "https://webrisk.googleapis.com",
		ThreatTypes:                []WebRiskThreatType{WebRiskThreatTypeMalware, WebRiskThreatTypeSocialEngineering},
		RequestTimeout:             60 * time.Second,
		DiffResponseMaxByteCount:   64 * 1024 * 1024,
		SearchResponseMaxByteCount: 1024 * 1024,
		MaxDatabaseEntries:         1024 * 1024,
		MaxDiffEntries:             0,
		ListPrefixMaxCount:         2 * 1024 * 1024,
		HashPrefixMinByteCount:     4,
		HashPrefixMaxByteCount:     32,
		HostLabelMaxCount:          5,
		SearchCacheMaxCount:        64 * 1024,
	}
}

// What NewWebRiskClient takes.
type WebRiskClientArgs struct {
	// The API key (LoadWebRiskApiKey). Required.
	ApiKey string
	// How the client reaches the API. nil dials directly, with a transport of the client's own.
	Transport http.RoundTripper
	// nil takes the defaults.
	Settings *WebRiskClientSettings
}

// The client, its local lists and its search cache. See the file header.
type WebRiskClient struct {
	settings     *WebRiskClientSettings
	baseUrl      string
	apiKey       string
	httpClient   *http.Client
	ownTransport *http.Transport

	stateLock   sync.Mutex
	lists       map[WebRiskThreatType]*webRiskList
	searchCache map[string]*webRiskSearchEntry
}

// A client with no lists yet, so its first update fetches each in full. Close releases its connections.
func NewWebRiskClient(args *WebRiskClientArgs) (*WebRiskClient, error) {
	if args == nil || strings.TrimSpace(args.ApiKey) == "" {
		return nil, fmt.Errorf("%w: no key", ErrWebRiskClientRefused)
	}
	settings := args.Settings
	if settings == nil {
		settings = DefaultWebRiskClientSettings()
	}
	parsed, err := url.Parse(settings.BaseUrl)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("%w: the base url is not an https origin", ErrWebRiskClientRefused)
	}
	if len(settings.ThreatTypes) == 0 {
		return nil, fmt.Errorf("%w: no threat list", ErrWebRiskClientRefused)
	}
	if settings.RequestTimeout <= 0 || settings.DiffResponseMaxByteCount <= 0 || settings.SearchResponseMaxByteCount <= 0 ||
		settings.ListPrefixMaxCount <= 0 || settings.HashPrefixMinByteCount <= 0 ||
		settings.HashPrefixMaxByteCount < settings.HashPrefixMinByteCount || sha256.Size < settings.HashPrefixMaxByteCount ||
		settings.HostLabelMaxCount <= 0 || settings.MaxDatabaseEntries < 0 || settings.MaxDiffEntries < 0 {
		return nil, fmt.Errorf("%w: a bound that is not positive", ErrWebRiskClientRefused)
	}
	self := &WebRiskClient{
		settings:    settings,
		baseUrl:     strings.TrimSuffix(settings.BaseUrl, "/"),
		apiKey:      strings.TrimSpace(args.ApiKey),
		lists:       map[WebRiskThreatType]*webRiskList{},
		searchCache: map[string]*webRiskSearchEntry{},
	}
	transport := args.Transport
	if transport == nil {
		self.ownTransport = &http.Transport{Proxy: nil, ForceAttemptHTTP2: true}
		transport = self.ownTransport
	}
	self.httpClient = &http.Client{
		Transport: transport,
		// the API answers where it is asked
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return self, nil
}

// Releases the client's own connections.
func (self *WebRiskClient) Close() {
	if self.ownTransport != nil {
		self.ownTransport.CloseIdleConnections()
	}
}

// The API key from this process's vault: abuse-web-risk.yml's api_key. An error names the file and never the key,
// nor any text of the file.
func LoadWebRiskApiKey() (string, error) {
	resource, err := Vault.SimpleResource(WebRiskKeyResourceName)
	if err != nil {
		return "", fmt.Errorf("%w: no %s", ErrWebRiskClientRefused, WebRiskKeyResourceName)
	}
	keys := map[string]any{}
	if err := resource.UnmarshalYamlE(&keys); err != nil {
		return "", fmt.Errorf("%w: %s does not parse", ErrWebRiskClientRefused, WebRiskKeyResourceName)
	}
	key, _ := keys[webRiskKeyField].(string)
	if key = strings.TrimSpace(key); key == "" {
		return "", fmt.Errorf("%w: %s holds no %s", ErrWebRiskClientRefused, WebRiskKeyResourceName, webRiskKeyField)
	}
	return key, nil
}

// The url expressions a host is looked up as, each a host and the path "/": the host itself, then its suffixes from
// its last HostLabelMaxCount labels, each one label shorter, never the top-level label alone (Web Risk's suffix and
// prefix expressions, for a url with no path). host is lowercase with no trailing dot; a host of one label is looked
// up as itself.
func WebRiskHostExpressions(host string, settings *WebRiskClientSettings) []string {
	expressions := []string{host + "/"}
	labels := strings.Split(host, ".")
	for index := max(len(labels)-settings.HostLabelMaxCount, 1); index < len(labels)-1; index += 1 {
		expressions = append(expressions, strings.Join(labels[index:], ".")+"/")
	}
	return expressions
}

// One threat list as this process holds it. Immutable: an update makes a new one.
type webRiskList struct {
	versionToken []byte
	// sorted bytewise, the order the API's removal indices count in
	prefixes []string
	// the distinct sizes in prefixes, smallest first
	prefixByteCounts []int
	// when the API recommended the next diff; zero for no recommendation
	nextDiffTime time.Time
}

// The shortest of the list's prefixes that starts hash, ok false for none.
func (self *webRiskList) match(hash []byte) (prefix string, ok bool) {
	for _, byteCount := range self.prefixByteCounts {
		if len(hash) < byteCount {
			break
		}
		prefix = string(hash[:byteCount])
		if _, found := slices.BinarySearch(self.prefixes, prefix); found {
			return prefix, true
		}
	}
	return "", false
}

// The parts of a computeDiff answer a raw diff reads. A Rice part is read only to refuse it.
type webRiskDiffResponse struct {
	ResponseType        string                `json:"responseType"`
	Additions           *webRiskDiffAdditions `json:"additions"`
	Removals            *webRiskDiffRemovals  `json:"removals"`
	NewVersionToken     string                `json:"newVersionToken"`
	Checksum            *webRiskDiffChecksum  `json:"checksum"`
	RecommendedNextDiff string                `json:"recommendedNextDiff"`
}

type webRiskDiffAdditions struct {
	RawHashes  []*webRiskRawHashes `json:"rawHashes"`
	RiceHashes json.RawMessage     `json:"riceHashes"`
}

// Prefixes of one size, concatenated.
type webRiskRawHashes struct {
	PrefixSize int    `json:"prefixSize"`
	RawHashes  string `json:"rawHashes"`
}

type webRiskDiffRemovals struct {
	RawIndices  *webRiskRawIndices `json:"rawIndices"`
	RiceIndices json.RawMessage    `json:"riceIndices"`
}

type webRiskRawIndices struct {
	Indices []int `json:"indices"`
}

type webRiskDiffChecksum struct {
	Sha256 string `json:"sha256"`
}

// Whether a json part is present: anything but absent or null.
func webRiskPresent(part json.RawMessage) bool {
	trimmed := bytes.TrimSpace(part)
	return 0 < len(trimmed) && !bytes.Equal(trimmed, []byte("null"))
}

// A proto3 json bytes field: standard or url-safe base64, padded or not.
func webRiskDecodeBytes(text string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(text); err == nil {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("%w: bytes that are not base64", ErrWebRiskSource)
}

// A proto3 json timestamp, zero for an absent one.
func webRiskParseTime(text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: a time that is not one", ErrWebRiskSource)
	}
	return parsed.UTC(), nil
}

// The list base becomes under response: base's prefixes, or none for a reset or a cold list, less the removals, with
// the additions, sorted and checked against the checksum. base is left as it was whatever the answer.
func applyWebRiskDiff(base *webRiskList, response *webRiskDiffResponse, settings *WebRiskClientSettings) (*webRiskList, error) {
	prefixes := []string{}
	switch response.ResponseType {
	case webRiskResponseTypeReset:
	case webRiskResponseTypeDiff:
		if base != nil {
			prefixes = base.prefixes
		}
	default:
		return nil, fmt.Errorf("%w: an answer of no known type", ErrWebRiskSource)
	}
	if (response.Additions != nil && webRiskPresent(response.Additions.RiceHashes)) ||
		(response.Removals != nil && webRiskPresent(response.Removals.RiceIndices)) {
		return nil, fmt.Errorf("%w: a compressed answer, though only raw was asked for", ErrWebRiskSource)
	}

	// the removals index the sorted list as it stood
	indices := []int{}
	if response.Removals != nil && response.Removals.RawIndices != nil {
		indices = response.Removals.RawIndices.Indices
	}
	if 0 < len(indices) && response.ResponseType == webRiskResponseTypeReset {
		return nil, fmt.Errorf("%w: a reset that removes", ErrWebRiskSource)
	}
	removed := make([]bool, len(prefixes))
	for _, index := range indices {
		if index < 0 || len(prefixes) <= index || removed[index] {
			return nil, fmt.Errorf("%w: a removal of no entry", ErrWebRiskSource)
		}
		removed[index] = true
	}
	kept := make([]string, 0, len(prefixes)-len(indices))
	for index, prefix := range prefixes {
		if !removed[index] {
			kept = append(kept, prefix)
		}
	}

	if response.Additions != nil {
		for _, raw := range response.Additions.RawHashes {
			if raw == nil || raw.PrefixSize < settings.HashPrefixMinByteCount || settings.HashPrefixMaxByteCount < raw.PrefixSize {
				return nil, fmt.Errorf("%w: an addition of a prefix size outside the protocol's", ErrWebRiskSource)
			}
			data, err := webRiskDecodeBytes(raw.RawHashes)
			if err != nil {
				return nil, err
			}
			if len(data)%raw.PrefixSize != 0 {
				return nil, fmt.Errorf("%w: an addition that is not whole prefixes", ErrWebRiskSource)
			}
			if settings.ListPrefixMaxCount < len(kept)+len(data)/raw.PrefixSize {
				return nil, fmt.Errorf("%w: a list past the local bound", ErrWebRiskSource)
			}
			for offset := 0; offset < len(data); offset += raw.PrefixSize {
				kept = append(kept, string(data[offset:offset+raw.PrefixSize]))
			}
		}
	}
	slices.Sort(kept)

	if response.Checksum == nil {
		return nil, fmt.Errorf("%w: no checksum", ErrWebRiskChecksum)
	}
	want, err := webRiskDecodeBytes(response.Checksum.Sha256)
	if err != nil || len(want) != sha256.Size {
		return nil, fmt.Errorf("%w: a checksum that is not a SHA-256", ErrWebRiskChecksum)
	}
	digest := sha256.New()
	for _, prefix := range kept {
		io.WriteString(digest, prefix)
	}
	if !bytes.Equal(digest.Sum(nil), want) {
		return nil, ErrWebRiskChecksum
	}

	versionToken, err := webRiskDecodeBytes(response.NewVersionToken)
	if err != nil {
		return nil, err
	}
	nextDiffTime, err := webRiskParseTime(response.RecommendedNextDiff)
	if err != nil {
		return nil, err
	}
	list := &webRiskList{
		versionToken: versionToken,
		prefixes:     kept,
		nextDiffTime: nextDiffTime,
	}
	for _, prefix := range kept {
		if !slices.Contains(list.prefixByteCounts, len(prefix)) {
			list.prefixByteCounts = append(list.prefixByteCounts, len(prefix))
		}
	}
	slices.Sort(list.prefixByteCounts)
	return list, nil
}

// The list held for threatType, nil for none.
func (self *WebRiskClient) currentList(threatType WebRiskThreatType) *webRiskList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.lists[threatType]
}

// Holds list for threatType; nil holds none, so the next update is a full fetch.
func (self *WebRiskClient) setList(threatType WebRiskThreatType, list *webRiskList) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if list == nil {
		delete(self.lists, threatType)
		return
	}
	self.lists[threatType] = list
}

// Whether every watched list is held: a client that holds none of one has not fetched it, or dropped it.
func (self *WebRiskClient) Ready() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, threatType := range self.settings.ThreatTypes {
		if self.lists[threatType] == nil {
			return false
		}
	}
	return true
}

// Brings every list current at now. A list the API has a diff for, its recommended time come or never fetched, takes
// it; a cold list is fetched in full. A list that fails its checksum is fetched in full once more, and dropped if it
// fails again. An error answers the first list that could not be brought current: ErrWebRiskSource, or
// ErrWebRiskChecksum when it was dropped. A list that failed otherwise stays as it was, and lists brought current
// before it stay current.
func (self *WebRiskClient) Update(ctx context.Context, now time.Time) error {
	for _, threatType := range self.settings.ThreatTypes {
		if err := self.updateList(ctx, threatType, now); err != nil {
			return err
		}
	}
	return nil
}

// Update for one list.
func (self *WebRiskClient) updateList(ctx context.Context, threatType WebRiskThreatType, now time.Time) error {
	base := self.currentList(threatType)
	if base != nil && now.Before(base.nextDiffTime) {
		return nil
	}
	list, err := self.fetchList(ctx, threatType, base)
	if errors.Is(err, ErrWebRiskChecksum) && base != nil {
		// the local list drifted from the API's: fetch it in full
		list, err = self.fetchList(ctx, threatType, nil)
	}
	if errors.Is(err, ErrWebRiskChecksum) {
		self.setList(threatType, nil)
	}
	if err != nil {
		return err
	}
	self.setList(threatType, list)
	return nil
}

// computeDiff for threatType from base's version, or in full from nil, applied to base.
func (self *WebRiskClient) fetchList(ctx context.Context, threatType WebRiskThreatType, base *webRiskList) (*webRiskList, error) {
	query := url.Values{}
	query.Set("threatType", string(threatType))
	if base != nil && 0 < len(base.versionToken) {
		query.Set("versionToken", base64.StdEncoding.EncodeToString(base.versionToken))
	}
	if 0 < self.settings.MaxDiffEntries {
		query.Set("constraints.maxDiffEntries", strconv.Itoa(self.settings.MaxDiffEntries))
	}
	if 0 < self.settings.MaxDatabaseEntries {
		query.Set("constraints.maxDatabaseEntries", strconv.Itoa(self.settings.MaxDatabaseEntries))
	}
	query.Set("constraints.supportedCompressions", webRiskCompressionRaw)
	body, err := self.get(ctx, webRiskComputeDiffPath, query, self.settings.DiffResponseMaxByteCount)
	if err != nil {
		return nil, err
	}
	response := &webRiskDiffResponse{}
	if err := json.Unmarshal(body, response); err != nil {
		return nil, fmt.Errorf("%w: not a diff", ErrWebRiskSource)
	}
	return applyWebRiskDiff(base, response, self.settings)
}

// What Lookup found for one full hash.
type WebRiskLookup struct {
	// A local list holds a prefix of the hash, so the API's answer for that prefix was read, cached or asked.
	PrefixMatch bool
	// The watched lists that list the full hash, empty when none does.
	ThreatTypes []WebRiskThreatType
}

// Looks up one full hash at now, the SHA-256 of a url expression. A hash no local list holds a prefix of sends
// nothing and answers no match. Otherwise the shortest matching prefix's cached answer serves while it is fresh, and
// else hashes.search is asked for that prefix alone and its answer kept. An error is the search failing, and nothing
// of it is kept.
func (self *WebRiskClient) Lookup(ctx context.Context, fullHash [sha256.Size]byte, now time.Time) (*WebRiskLookup, error) {
	prefix, ok := self.matchPrefix(fullHash[:])
	if !ok {
		return &WebRiskLookup{}, nil
	}
	lookup := &WebRiskLookup{PrefixMatch: true}
	if threatTypes, fresh := self.cachedAnswer(prefix, fullHash, now); fresh {
		lookup.ThreatTypes = threatTypes
		return lookup, nil
	}
	entry, err := self.search(ctx, prefix)
	if err != nil {
		return nil, err
	}
	self.keepAnswer(prefix, entry, now)
	lookup.ThreatTypes, _ = entry.answer(fullHash, now)
	return lookup, nil
}

// The shortest prefix of hash any list holds, ok false for none.
func (self *WebRiskClient) matchPrefix(hash []byte) (prefix string, ok bool) {
	lists := func() []*webRiskList {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		lists := make([]*webRiskList, 0, len(self.lists))
		for _, list := range self.lists {
			lists = append(lists, list)
		}
		return lists
	}()
	for _, list := range lists {
		if listPrefix, found := list.match(hash); found && (!ok || len(listPrefix) < len(prefix)) {
			prefix, ok = listPrefix, true
		}
	}
	return
}

// One prefix's hashes.search answer.
type webRiskSearchEntry struct {
	// the full hashes listed under the prefix in a watched list
	threats map[[sha256.Size]byte]*webRiskThreat
	// until when a full hash of the prefix not among threats is not listed
	negativeExpireTime time.Time
}

// One listed full hash.
type webRiskThreat struct {
	threatTypes []WebRiskThreatType
	expireTime  time.Time
}

// What the entry says of fullHash at now, and whether it still says it.
func (self *webRiskSearchEntry) answer(fullHash [sha256.Size]byte, now time.Time) (threatTypes []WebRiskThreatType, fresh bool) {
	if threat, ok := self.threats[fullHash]; ok {
		if now.Before(threat.expireTime) {
			return slices.Clone(threat.threatTypes), true
		}
		return nil, false
	}
	return nil, now.Before(self.negativeExpireTime)
}

// Whether the entry says nothing any more at now.
func (self *webRiskSearchEntry) expired(now time.Time) bool {
	if now.Before(self.negativeExpireTime) {
		return false
	}
	for _, threat := range self.threats {
		if now.Before(threat.expireTime) {
			return false
		}
	}
	return true
}

// The cached answer for fullHash under prefix, fresh false when there is none to trust.
func (self *WebRiskClient) cachedAnswer(prefix string, fullHash [sha256.Size]byte, now time.Time) ([]WebRiskThreatType, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, ok := self.searchCache[prefix]
	if !ok {
		return nil, false
	}
	return entry.answer(fullHash, now)
}

// Keeps prefix's answer, within the cache's bound.
func (self *WebRiskClient) keepAnswer(prefix string, entry *webRiskSearchEntry, now time.Time) {
	if self.settings.SearchCacheMaxCount <= 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.searchCache[prefix]; !ok && self.settings.SearchCacheMaxCount <= len(self.searchCache) {
		for cachedPrefix, cachedEntry := range self.searchCache {
			if cachedEntry.expired(now) {
				delete(self.searchCache, cachedPrefix)
			}
		}
		if self.settings.SearchCacheMaxCount <= len(self.searchCache) {
			clear(self.searchCache)
		}
	}
	self.searchCache[prefix] = entry
}

// The parts of a hashes.search answer.
type webRiskSearchResponse struct {
	Threats            []*webRiskSearchThreat `json:"threats"`
	NegativeExpireTime string                 `json:"negativeExpireTime"`
}

type webRiskSearchThreat struct {
	ThreatTypes []string `json:"threatTypes"`
	Hash        string   `json:"hash"`
	ExpireTime  string   `json:"expireTime"`
}

// hashes.search for prefix in every watched list. A full hash the answer gives must start with prefix, and of its
// lists only the watched ones are kept.
func (self *WebRiskClient) search(ctx context.Context, prefix string) (*webRiskSearchEntry, error) {
	query := url.Values{}
	query.Set("hashPrefix", base64.StdEncoding.EncodeToString([]byte(prefix)))
	for _, threatType := range self.settings.ThreatTypes {
		query.Add("threatTypes", string(threatType))
	}
	body, err := self.get(ctx, webRiskSearchHashesPath, query, self.settings.SearchResponseMaxByteCount)
	if err != nil {
		return nil, err
	}
	response := &webRiskSearchResponse{}
	if err := json.Unmarshal(body, response); err != nil {
		return nil, fmt.Errorf("%w: not a search answer", ErrWebRiskSource)
	}
	entry := &webRiskSearchEntry{threats: map[[sha256.Size]byte]*webRiskThreat{}}
	if entry.negativeExpireTime, err = webRiskParseTime(response.NegativeExpireTime); err != nil {
		return nil, err
	}
	for _, threat := range response.Threats {
		if threat == nil {
			continue
		}
		hash, err := webRiskDecodeBytes(threat.Hash)
		if err != nil || len(hash) != sha256.Size || !bytes.HasPrefix(hash, []byte(prefix)) {
			return nil, fmt.Errorf("%w: a full hash that is not under the prefix asked", ErrWebRiskSource)
		}
		expireTime, err := webRiskParseTime(threat.ExpireTime)
		if err != nil {
			return nil, err
		}
		watched := []WebRiskThreatType{}
		for _, threatType := range threat.ThreatTypes {
			if slices.Contains(self.settings.ThreatTypes, WebRiskThreatType(threatType)) && !slices.Contains(watched, WebRiskThreatType(threatType)) {
				watched = append(watched, WebRiskThreatType(threatType))
			}
		}
		if len(watched) == 0 {
			continue
		}
		entry.threats[[sha256.Size]byte(hash)] = &webRiskThreat{threatTypes: watched, expireTime: expireTime}
	}
	return entry, nil
}

// One GET's body, read to maxByteCount. Anything but a 200 inside the bound is the source failing.
func (self *WebRiskClient) get(ctx context.Context, path string, query url.Values, maxByteCount int64) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, self.baseUrl+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: a request that cannot be one", ErrWebRiskSource)
	}
	request.Header.Set(webRiskApiKeyHeaderName, self.apiKey)
	response, err := self.httpClient.Do(request)
	if err != nil {
		// a transport's error carries the url, and so a prefix: not wrapped
		return nil, fmt.Errorf("%w: unreachable", ErrWebRiskSource)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrWebRiskSource, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxByteCount+1))
	if err != nil {
		return nil, fmt.Errorf("%w: a body cut short", ErrWebRiskSource)
	}
	if maxByteCount < int64(len(body)) {
		return nil, fmt.Errorf("%w: a body past the bound", ErrWebRiskSource)
	}
	return body, nil
}
