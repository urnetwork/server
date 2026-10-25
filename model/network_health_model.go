// The public network health status: what GET /health reports, and what ur.io
// shows in its footer status pill and, while the network is degraded, in a
// banner on every page. An operator sets it with `bringyourctl health
// degrade` and clears it with `bringyourctl health recover`. No stored status
// is healthy.
//
// It is not the contract degradation valve (network_degradation_model.go).
// The valve decides how new contracts are funded, and its state is never
// public: it would announce when an exhausted payer gets a zero cost
// contract. Setting this status changes nothing but what /health reports.
//
// The reason is shown verbatim to everyone who opens the site.
// ValidateNetworkHealthReason refuses what must never be published there:
// internal names, addresses, files, identifiers, credentials and amounts, the
// rules the published outage pages follow. A stored reason is checked again
// on every read, so text that fails the current rules is never served.
//
// Safe for concurrent use.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

type NetworkHealthStatus string

const (
	NetworkHealthHealthy NetworkHealthStatus = "healthy"
	// the levels an operator sets, mildest first: the scale of the outage pages
	NetworkHealthDegraded NetworkHealthStatus = "degraded"
	NetworkHealthPartial  NetworkHealthStatus = "partial"
	NetworkHealthMajor    NetworkHealthStatus = "major"
	NetworkHealthFull     NetworkHealthStatus = "full"
	// the status could not be read, or what is stored is not a valid status
	NetworkHealthUnknown NetworkHealthStatus = "unknown"
)

// The levels an operator can set, mildest first.
var NetworkHealthLevels = []NetworkHealthStatus{
	NetworkHealthDegraded,
	NetworkHealthPartial,
	NetworkHealthMajor,
	NetworkHealthFull,
}

// Who set the status. Only operators set it today. The source is stored so
// that a later automatic writer can leave an operator's status alone; it is
// never public.
type NetworkHealthSource string

const NetworkHealthSourceOperator NetworkHealthSource = "operator"

// The longest reason, in characters. The site shows it on one or two lines.
const NetworkHealthReasonMaxLength = 160

// GET /health trusts one read of the status this long.
const networkHealthCacheTtl = 10 * time.Second

// After a failed read the process reports unknown and asks again this soon.
const networkHealthErrorRetry = time.Second

// Bounds every Redis round trip.
const networkHealthRedisTimeout = time.Second

const networkHealthKey = "network_health:status:v1"

// What is stored does not parse, names no level, or has a reason that fails
// ValidateNetworkHealthReason.
var ErrNetworkHealthMalformed = errors.New("the stored network health status is malformed")

// The stored status. Since is when the network left healthy, and a change of
// level or reason keeps it. Updated is the last change.
type NetworkHealthRecord struct {
	Status  NetworkHealthStatus `json:"status"`
	Reason  string              `json:"reason"`
	Since   time.Time           `json:"since"`
	Updated time.Time           `json:"updated"`
	Source  NetworkHealthSource `json:"source"`
}

// The GET /health response. A status other than healthy or unknown carries
// its reason and its RFC 3339 UTC times.
type NetworkHealthResult struct {
	Status  NetworkHealthStatus `json:"status"`
	Reason  string              `json:"reason,omitempty"`
	Since   string              `json:"since,omitempty"`
	Updated string              `json:"updated,omitempty"`
}

// ParseNetworkHealthLevel parses a level an operator can set.
func ParseNetworkHealthLevel(text string) (NetworkHealthStatus, error) {
	level := NetworkHealthStatus(strings.ToLower(strings.TrimSpace(text)))
	if !slices.Contains(NetworkHealthLevels, level) {
		return "", fmt.Errorf("the level must be degraded, partial, major or full, not %q", text)
	}
	return level, nil
}

// What a public reason must not contain, each with what it names. The first
// rules are the outage pages' own; the rest refuse the shapes of addresses,
// files and identifiers. Every name here is already public in this
// repository.
var networkHealthReasonRules = []struct {
	pattern *regexp.Regexp
	names   string
}{
	{regexp.MustCompile(`(?i)\bedge-?\d+\b|\b(?:fireside|crisp|planetoid)\b`), "a host name"},
	{regexp.MustCompile(`(?i)\b(?:xops|vault|warpctl|bringyourctl|run-edges|run-dbs|ansible|config-updater)\b`), "an internal tool"},
	{regexp.MustCompile(`\bSIGNALS\b|\bRUN-MAIN\b|\bT\d{3,}\b|\bFP2\b|(?i:\bfactury\b|postmortem/)`), "an internal document or record"},
	{regexp.MustCompile(`(?i)\b(?:the owner|another developer|the operator|operator session|coordinating session|agent session)\b`), "a person or role"},
	{regexp.MustCompile(`(?i)\b(?:claude|codex|llm|classifier)\b`), "an AI agent or its tooling"},
	{regexp.MustCompile(`\$\s?\d|(?i:\b\d[\d,.]*\s?(?:usd|usdc|sol|zec|xmr)\b)`), "an amount of money"},
	{regexp.MustCompile(`(?i)\b(?:circle|usdc wallet|sol wallet)\b`), "a payment provider's detail"},
	{regexp.MustCompile(`(?i)\b(?:jwt|signing key|tls key|private key|passphrase|hmac|secret)s?\b`), "a credential or the security design"},
	{regexp.MustCompile(`(?i)\b(?:earn|earning|earnings|payout|payouts)\b`), "provider earnings or payouts"},
	{regexp.MustCompile(`(?i)\b(?:https?|wss?|ftp)://|\bwww\.`), "a URL (the site links the outage pages itself)"},
	{regexp.MustCompile(`[\w.+-]+@[\w-]+(?:\.[\w-]+)+`), "an email address"},
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b|(?i:\b(?:[0-9a-f]{1,4}:){4,7}[0-9a-f]{1,4}\b|[0-9a-f]{0,4}::[0-9a-f]{1,4})`), "an IP address"},
	{regexp.MustCompile(`(?i)\b[\w-]+\.(?:md|go|ya?ml|json|sh|sql|py|js|ts|toml|conf|log|txt)\b`), "a file name"},
	{regexp.MustCompile(`(?:^|\s)/[\w.-]+/`), "a file path"},
	{regexp.MustCompile(`\b[A-Za-z0-9]+_[A-Za-z0-9_]+\b`), "an internal identifier"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "an identifier"},
	{regexp.MustCompile(`[A-Za-z0-9+/=_-]{24,}`), "a token or identifier"},
}

// Names with a dot and a top level domain; only the public sites may appear.
var networkHealthHostPattern = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9-]*(?:\.[a-z0-9][a-z0-9-]*)*\.[a-z]{2,24}\b`)

var networkHealthPublicHosts = []string{"ur.io", "ur.xyz", "ur.network", "lag.dev", "bringyour.com", "api.bringyour.com"}

// A name and a port, as in "host:8080". The name must have a letter, so a
// time like 08:10 is not a port.
var networkHealthHostPortPattern = regexp.MustCompile(`\b([\w.-]+):(\d{2,5})\b`)

// A run of hex digits. With a digit and a letter it is a commit or a hash.
var networkHealthHexPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{7,}\b`)

// ValidateNetworkHealthReason checks that reason, trimmed, is one line of
// public text that names nothing internal.
func ValidateNetworkHealthReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("the reason is empty")
	}
	if !utf8.ValidString(reason) {
		return errors.New("the reason is not UTF-8 text")
	}
	if n := utf8.RuneCountInString(reason); NetworkHealthReasonMaxLength < n {
		return fmt.Errorf("the reason is %d characters (at most %d)", n, NetworkHealthReasonMaxLength)
	}
	if strings.ContainsFunc(reason, unicode.IsControl) {
		return errors.New("the reason must be one line of plain text")
	}
	for _, rule := range networkHealthReasonRules {
		if match := rule.pattern.FindString(reason); match != "" {
			return fmt.Errorf("the reason names %s: %q", rule.names, strings.TrimSpace(match))
		}
	}
	for _, match := range networkHealthHostPattern.FindAllString(reason, -1) {
		if !slices.Contains(networkHealthPublicHosts, strings.ToLower(match)) {
			return fmt.Errorf("the reason names a host name: %q", match)
		}
	}
	for _, match := range networkHealthHostPortPattern.FindAllStringSubmatch(reason, -1) {
		if strings.ContainsFunc(match[1], unicode.IsLetter) {
			return fmt.Errorf("the reason names a host and port: %q", match[0])
		}
	}
	for _, match := range networkHealthHexPattern.FindAllString(reason, -1) {
		if strings.ContainsFunc(match, unicode.IsDigit) && strings.ContainsFunc(match, unicode.IsLetter) {
			return fmt.Errorf("the reason names a commit or hash: %q", match)
		}
	}
	return nil
}

// The clock of the writers and the cache. Replaced in tests.
var networkHealthNow = server.NowUtc

// Reads the stored value; present is false for a missing key. Replaced in
// tests to inject a stored value or a Redis error.
var readNetworkHealthRaw = func(ctx context.Context) (raw string, present bool, returnErr error) {
	readCtx, cancel := context.WithTimeout(ctx, networkHealthRedisTimeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(readCtx, func(client server.RedisClient) error {
		value, err := client.Get(readCtx, networkHealthKey).Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		raw, present = value, true
		return nil
	})
	if returnErr != nil {
		raw, present = "", false
	}
	return
}

// Unknown fields are allowed, so a newer writer's record stays readable.
func parseNetworkHealthRecord(raw string) (*NetworkHealthRecord, error) {
	var record NetworkHealthRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNetworkHealthMalformed, err)
	}
	if !slices.Contains(NetworkHealthLevels, record.Status) {
		return nil, fmt.Errorf("%w: no level %q", ErrNetworkHealthMalformed, record.Status)
	}
	if record.Since.IsZero() || record.Updated.IsZero() {
		return nil, fmt.Errorf("%w: missing times", ErrNetworkHealthMalformed)
	}
	if err := ValidateNetworkHealthReason(record.Reason); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNetworkHealthMalformed, err)
	}
	return &record, nil
}

// ReadNetworkHealth reads the stored status from Redis, without the cache:
// nil when the network is healthy. A stored value that is not a valid status
// is ErrNetworkHealthMalformed.
func ReadNetworkHealth(ctx context.Context) (*NetworkHealthRecord, error) {
	raw, present, err := readNetworkHealthRaw(ctx)
	if err != nil || !present {
		return nil, err
	}
	return parseNetworkHealthRecord(raw)
}

// SetNetworkHealth stores level with the public reason and returns what it
// stored. A network that is already degraded keeps its since time, so the
// site can say how long the degradation has lasted.
func SetNetworkHealth(ctx context.Context, level NetworkHealthStatus, reason string, source NetworkHealthSource) (*NetworkHealthRecord, error) {
	if !slices.Contains(NetworkHealthLevels, level) {
		return nil, fmt.Errorf("the level must be degraded, partial, major or full, not %q", level)
	}
	if source == "" {
		return nil, errors.New("the status needs a source")
	}
	reason = strings.TrimSpace(reason)
	if err := ValidateNetworkHealthReason(reason); err != nil {
		return nil, err
	}
	now := networkHealthNow().Truncate(time.Second)
	record := &NetworkHealthRecord{Status: level, Reason: reason, Since: now, Updated: now, Source: source}
	// a malformed status is replaced as if absent
	previous, err := ReadNetworkHealth(ctx)
	if err != nil && !errors.Is(err, ErrNetworkHealthMalformed) {
		return nil, err
	}
	if previous != nil {
		record.Since = previous.Since
	}
	value, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	writeCtx, cancel := context.WithTimeout(ctx, networkHealthRedisTimeout)
	defer cancel()
	err = server.RedisWithDeadline(writeCtx, func(client server.RedisClient) error {
		return client.Set(writeCtx, networkHealthKey, string(value), 0).Err()
	})
	// this process sees the change at once; other processes within the cache period
	networkHealthCache.invalidate()
	if err != nil {
		return nil, err
	}
	return record, nil
}

// ClearNetworkHealth returns the network to healthy. cleared is whether a
// status was stored; previous is that status, or nil when it was malformed.
func ClearNetworkHealth(ctx context.Context) (previous *NetworkHealthRecord, cleared bool, returnErr error) {
	previous, err := ReadNetworkHealth(ctx)
	if err != nil && !errors.Is(err, ErrNetworkHealthMalformed) {
		return nil, false, err
	}
	writeCtx, cancel := context.WithTimeout(ctx, networkHealthRedisTimeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(writeCtx, func(client server.RedisClient) error {
		count, err := client.Del(writeCtx, networkHealthKey).Result()
		cleared = 0 < count
		return err
	})
	networkHealthCache.invalidate()
	if returnErr != nil {
		return nil, false, returnErr
	}
	if !cleared {
		previous = nil
	}
	return previous, cleared, nil
}

// The public form of a stored status; nil is healthy.
func NetworkHealthResultOf(record *NetworkHealthRecord) *NetworkHealthResult {
	if record == nil {
		return &NetworkHealthResult{Status: NetworkHealthHealthy}
	}
	return &NetworkHealthResult{
		Status:  record.Status,
		Reason:  record.Reason,
		Since:   record.Since.UTC().Format(time.RFC3339),
		Updated: record.Updated.UTC().Format(time.RFC3339),
	}
}

// One cached read of the status.
type networkHealthSnapshot struct {
	loadTime  time.Time
	expiresAt time.Time
	result    *NetworkHealthResult
}

// The process cache. One caller refreshes a stale snapshot while the others
// keep using it; only the first read of the process waits for the refresh.
type networkHealthCacheState struct {
	refreshLock sync.Mutex
	snapshot    atomic.Pointer[networkHealthSnapshot]
}

var networkHealthCache = &networkHealthCacheState{}

// A snapshot serves reads until it expires, and never for a clock that moved
// behind its load.
func (self *networkHealthCacheState) fresh(snapshot *networkHealthSnapshot, now time.Time) bool {
	return snapshot != nil && now.Before(snapshot.expiresAt) && !now.Before(snapshot.loadTime)
}

func (self *networkHealthCacheState) current(ctx context.Context, now time.Time) *networkHealthSnapshot {
	snapshot := self.snapshot.Load()
	if self.fresh(snapshot, now) {
		return snapshot
	}
	if snapshot == nil {
		self.refreshLock.Lock()
	} else if !self.refreshLock.TryLock() {
		return snapshot
	}
	defer self.refreshLock.Unlock()
	if latest := self.snapshot.Load(); self.fresh(latest, now) {
		return latest
	}
	refreshed := loadNetworkHealthSnapshot(ctx, now)
	self.snapshot.Store(refreshed)
	return refreshed
}

// Drops the snapshot, so the next read refreshes.
func (self *networkHealthCacheState) invalidate() {
	self.snapshot.Store(nil)
}

// The Redis read is detached from the caller's cancellation, so one canceled
// request cannot make the process report unknown.
func loadNetworkHealthSnapshot(ctx context.Context, now time.Time) *networkHealthSnapshot {
	snapshot := &networkHealthSnapshot{loadTime: now, expiresAt: now.Add(networkHealthCacheTtl)}
	record, err := ReadNetworkHealth(context.WithoutCancel(ctx))
	if errors.Is(err, ErrNetworkHealthMalformed) {
		glog.Infof("[health]GET /health reports unknown: %v\n", err)
		snapshot.result = &NetworkHealthResult{Status: NetworkHealthUnknown}
		return snapshot
	}
	if err != nil {
		snapshot.expiresAt = now.Add(networkHealthErrorRetry)
		if glog.V(1) {
			glog.Infof("[health]network health read failed; GET /health reports unknown: %v\n", err)
		}
		snapshot.result = &NetworkHealthResult{Status: NetworkHealthUnknown}
		return snapshot
	}
	snapshot.result = NetworkHealthResultOf(record)
	return snapshot
}

// GetNetworkHealth is the status GET /health reports, read through the
// process cache. It never fails: a status that cannot be read is unknown.
func GetNetworkHealth(ctx context.Context) *NetworkHealthResult {
	return networkHealthCache.current(ctx, networkHealthNow()).result
}

// Drops the process cache, so the next read sees the stored status.
func Testing_ResetNetworkHealthCache() {
	networkHealthCache.invalidate()
}
