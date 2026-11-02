package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The camouflage report (connect/EXTENDER-UPDATE2.md §3.3, rollout gates;
// connect/EXTENDER-SPLICE-ROLLOUT.md, Reporting path): what a client's tcp
// camouflage dials came to, and for an extender how it classified its tcp
// connections, posted as a delta about once an hour, so the numeric rollout
// gates can be read in Prometheus.
//
// The route only increments counters (extender_camouflage_report_metrics.go).
// It stores nothing and adds no table. The caller's identity authenticates the
// post and spends a rate budget, and that is all it does: the budget's Redis key
// is a keyed hash of the client id that expires at the end of its window, and
// the id reaches no label and no log. Every label value is one of a fixed set
// the server holds, whatever the report says.
//
// A report is counted once. The sender keeps one report id and one body across
// its bounded retries, and the first post of an id claims it in Redis for longer
// than any sender retries (extenderCamouflageReportClaimTtl), so a retry of a
// post whose answer was lost is answered duplicate and adds nothing. What cannot
// be claimed is not counted, so every failure here undercounts and none
// overcounts: a lost claim reply is a report that goes uncounted.
//
// The country is the server's own lookup of the caller's address, never the
// report's word. A report that crossed an extender arrives from that extender's
// address, which says nothing of where the client is (connect GetExtenderHint),
// so the caller's address being an active extender's that the caller does not
// own is the country `relayed`. A relay the operator does not know, a VLESS
// server or a user's proxy, reads as that relay's country.
//
// An extender's part of a report is taken only from a client that activated an
// extender that is active now. Rollback thresholds read those counters, so
// anyone else's word about them is dropped, and the rest of the report stands.
//
// Off unless `extender.yml` sets `camouflage_report.enabled`, which is also the
// kill switch: off, the route answers disabled before it reads anything, and the
// sender waits hours before it asks again. The client side is off by default
// too (connect ExtenderCamouflageReporter).

const (
	// the longest report body read. Every field at its bound, with
	// extenderCamouflageReportMaxTargetCount names of the longest dns name, fits
	ExtenderCamouflageReportMaxByteCount = 16 * 1024

	extenderCamouflageReportIdLength = 32
	// reports per client per window. The sender posts about once an hour, with
	// at most three retries of each report; a budget of twelve never meets one
	// that behaves
	extenderCamouflageReportRateLimit  = 12
	extenderCamouflageReportRateWindow = time.Hour
	// how long a report id stays claimed: longer than the two hours a sender
	// keeps one report (connect ExtenderCamouflageReporterSettings.MaxReportAge)
	extenderCamouflageReportClaimTtl = 3 * time.Hour
	// the bound of one Redis command of the route
	extenderCamouflageReportRedisTimeout = 5 * time.Second

	extenderCamouflageReportMaxClientVersionByteCount  = 64
	extenderCamouflageReportMaxResourceVersion         = 1 << 20
	extenderCamouflageReportMaxIpVersionCount          = 2
	extenderCamouflageReportMaxDegradedReasonByteCount = 64
	extenderCamouflageReportMaxTargetCount             = 32
	extenderCamouflageReportMaxTargetNameByteCount     = 253

	// The most one report may add to one counter. The sender holds the excess
	// for its next report, so a client that behaves never meets these. A client's
	// bound is about 18 dials a second for an hour, and an extender's is 18,000
	// connections a second. A client is unverified, so its bound is also how much
	// one identity can move the fleet's numbers per report.
	extenderCamouflageReportMaxClientCount       int64 = 1 << 16
	extenderCamouflageReportMaxExtenderCount     int64 = 1 << 26
	extenderCamouflageReportMaxTargetSpliceCount int64 = 1 << 24
	extenderCamouflageReportMaxTargetByteCount   int64 = 1 << 44
)

// The answers of the route.
const (
	// counted now
	ExtenderCamouflageReportAccepted = "accepted"
	// counted by an earlier post of the same id
	ExtenderCamouflageReportDuplicate = "duplicate"
	// not counted; post again after retry_after_seconds
	ExtenderCamouflageReportRateLimited = "rate_limited"
	// not counted; the operator has the reports off
	ExtenderCamouflageReportDisabled = "disabled"
	// not counted, and another post of this report would not be either
	ExtenderCamouflageReportInvalid = "invalid"
	// not counted; post again later
	ExtenderCamouflageReportUnavailable = "unavailable"
)

// What a report's client part counts, one field per
// connect.ExtenderTcpCamouflageClientStats counter, each the increase since the
// sender's last report.
type ExtenderCamouflageClientCounts struct {
	CamouflageWon             int64 `json:"camouflage_won,omitempty"`
	LegacyWon                 int64 `json:"legacy_won,omitempty"`
	BothFailed                int64 `json:"both_failed,omitempty"`
	Refused                   int64 `json:"refused,omitempty"`
	CamouflageUnauthenticated int64 `json:"camouflage_unauthenticated,omitempty"`
	CamouflageLeafRejected    int64 `json:"camouflage_leaf_rejected,omitempty"`
	LegacyOnlyNoKey           int64 `json:"legacy_only_no_key,omitempty"`
	LegacyOnlyFingerprint     int64 `json:"legacy_only_fingerprint,omitempty"`
	LegacyOnlyNoFrontName     int64 `json:"legacy_only_no_front_name,omitempty"`
}

// One borrowed site's splice load on one extender since its last report.
type ExtenderCamouflageTargetCounts struct {
	Name             string `json:"name"`
	SpliceCount      int64  `json:"splice_count,omitempty"`
	RelayedByteCount int64  `json:"relayed_byte_count,omitempty"`
}

// What a report's extender part counts, one field per
// extender.ExtenderCamouflageStats counter, each the increase since the
// extender's last report, and its coverage verdict as of the report
// (extender.ExtenderCamouflageStatus).
type ExtenderCamouflageExtenderCounts struct {
	Authenticated            int64                             `json:"authenticated,omitempty"`
	Spliced                  int64                             `json:"spliced,omitempty"`
	Terminated               int64                             `json:"terminated,omitempty"`
	SpliceFallbackTerminated int64                             `json:"splice_fallback_terminated,omitempty"`
	ReplayRefused            int64                             `json:"replay_refused,omitempty"`
	TimeWindowFailed         int64                             `json:"time_window_failed,omitempty"`
	SpliceDialFailed         int64                             `json:"splice_dial_failed,omitempty"`
	BorrowVerifyFailed       int64                             `json:"borrow_verify_failed,omitempty"`
	Targets                  []*ExtenderCamouflageTargetCounts `json:"targets,omitempty"`
	Covered                  bool                              `json:"covered,omitempty"`
	DegradedReason           string                            `json:"degraded_reason,omitempty"`
}

// One report as the sender posts it (connect ExtenderCamouflageReportArgs).
// Fields this server does not know are ignored, and a field like a country or a
// client id has no effect here whatever it says.
type ExtenderCamouflageReportArgs struct {
	// 32 lower case hex digits, random, and the same across the retries of one
	// report
	ReportId string `json:"report_id"`
	// the sender's build; reduced to a label by server.AppVersionLabel
	ClientVersion string `json:"client_version,omitempty"`
	// the version of the borrow list the sender reads, zero for none
	ResourceVersion int `json:"resource_version,omitempty"`
	// the families the sender dials extenders of; empty is any
	IpVersions []int                             `json:"ip_versions,omitempty"`
	Client     *ExtenderCamouflageClientCounts   `json:"client,omitempty"`
	Extender   *ExtenderCamouflageExtenderCounts `json:"extender,omitempty"`
}

type ExtenderCamouflageReportResult struct {
	// One of the ExtenderCamouflageReport* answers. A sender treats one it does
	// not know as unavailable.
	Outcome string `json:"outcome"`
	// seconds until a report can be taken, with a rate limit
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// The active extenders as a report reads them: who may report as an extender,
// and which addresses are relays. Built whole and never changed after.
type extenderCamouflageFleet struct {
	ownerClientIds map[server.Id]bool
	addressOwners  map[netip.Prefix]map[server.Id]bool
}

func newExtenderCamouflageFleet(ownerAddresses []*model.NetworkExtenderOwnerAddress) *extenderCamouflageFleet {
	fleet := &extenderCamouflageFleet{
		ownerClientIds: map[server.Id]bool{},
		addressOwners:  map[netip.Prefix]map[server.Id]bool{},
	}
	for _, ownerAddress := range ownerAddresses {
		fleet.ownerClientIds[ownerAddress.ClientId] = true
		prefix := extenderCamouflageAddressPrefix(ownerAddress.Ip)
		if !prefix.IsValid() {
			continue
		}
		owners, ok := fleet.addressOwners[prefix]
		if !ok {
			owners = map[server.Id]bool{}
			fleet.addressOwners[prefix] = owners
		}
		owners[ownerAddress.ClientId] = true
	}
	return fleet
}

// What the fleet compares an address by: the /32 of an ipv4 address, and the /64
// of an ipv6 one, since a host may source from another address of its network.
func extenderCamouflageAddressPrefix(ip netip.Addr) netip.Prefix {
	if !ip.IsValid() {
		return netip.Prefix{}
	}
	ip = ip.Unmap()
	if ip.Is4() {
		return netip.PrefixFrom(ip, 32)
	}
	return netip.PrefixFrom(ip, 64).Masked()
}

// Whether this client activated an extender that is active now.
func (self *extenderCamouflageFleet) owner(clientId server.Id) bool {
	return self.ownerClientIds[clientId]
}

// Whether a report from this client at this address crossed an extender: the
// address is an active extender's, and not one this client activated.
func (self *extenderCamouflageFleet) relayed(ip netip.Addr, clientId server.Id) bool {
	owners, ok := self.addressOwners[extenderCamouflageAddressPrefix(ip)]
	return ok && !owners[clientId]
}

// How long one process reads the fleet from its copy, and how long one read of it
// may take. A report from an extender activated within the interval may count
// without its extender part, which undercounts it once.
const (
	extenderCamouflageFleetCacheTimeout = 10 * time.Minute
	extenderCamouflageFleetReadTimeout  = 5 * time.Second
)

// One process's copy of the fleet. The read belongs to the process, not to the
// request that found it due: it has its own bound, no lock is held across it,
// and while it runs every other request is answered from the copy at hand.
type extenderCamouflageFleetCache struct {
	read func(ctx context.Context) []*model.NetworkExtenderOwnerAddress

	stateLock sync.Mutex
	fleet     *extenderCamouflageFleet
	// when a read last started, whatever it answered
	checkedAt time.Time
}

// The copy at hand, read first when it is due. Nil while there is none yet: no
// read has finished, or none has worked.
func (self *extenderCamouflageFleetCache) get(ctx context.Context, now time.Time) *extenderCamouflageFleet {
	claimed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.checkedAt.IsZero() && now.Sub(self.checkedAt) < extenderCamouflageFleetCacheTimeout {
			return false
		}
		self.checkedAt = now
		return true
	}()
	if claimed {
		readCtx, readCancel := context.WithTimeout(context.WithoutCancel(ctx), extenderCamouflageFleetReadTimeout)
		defer readCancel()
		server.HandleError(func() {
			fleet := newExtenderCamouflageFleet(self.read(readCtx))
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.fleet = fleet
		}, func(err error) {
			if glog.V(1) {
				glog.Infof("[extender]camouflage report fleet read: %s\n", err)
			}
		})
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.fleet
}

// What a report needs from outside the route. The process's are the vault's,
// Redis and the database; a test replaces each.
type extenderCamouflageReporting struct {
	// the control: `camouflage_report.enabled`
	enabled func() bool
	now     func() time.Time
	// counts one report against the client's window, and says whether it fits
	spendRateBudget func(ctx context.Context, clientId server.Id, now time.Time) (allowed bool, returnErr error)
	// claims a report id, and says whether this post is the first of it
	claimReport func(ctx context.Context, reportId string) (first bool, returnErr error)
	fleet       func(ctx context.Context, now time.Time) *extenderCamouflageFleet
	// the lower case country code of an address, empty when it has none
	country func(ip netip.Addr) (string, error)
	// the versions and names labels may carry
	labels  func() *extenderCamouflageReportLabels
	metrics *extenderCamouflageReportMetrics
}

var defaultExtenderCamouflageFleetCache = &extenderCamouflageFleetCache{
	read: model.GetActiveNetworkExtenderOwnerAddresses,
}

var defaultExtenderCamouflageReporting = &extenderCamouflageReporting{
	enabled:         extenderCamouflageReportEnabled,
	now:             server.NowUtc,
	spendRateBudget: spendExtenderCamouflageReportRateBudget,
	claimReport:     claimExtenderCamouflageReport,
	fleet:           defaultExtenderCamouflageFleetCache.get,
	country:         extenderCamouflageReportCountryCode,
	labels:          sync.OnceValue(bundledExtenderCamouflageReportLabels),
	metrics:         defaultExtenderCamouflageReportMetrics,
}

// The server's own country of an address, an error when it has no database to read,
// which panics at every lookup rather than failing one: a report does without a
// country (server.IpInfoDatabaseDeployed).
func extenderCamouflageReportCountryCode(ip netip.Addr) (string, error) {
	if !server.IpInfoDatabaseDeployed() {
		return "", errors.New("no ip database is deployed")
	}
	ipInfo, err := server.GetIpInfo(ip)
	if err != nil {
		return "", err
	}
	return ipInfo.CountryCode, nil
}

// Whether the reports are on (`camouflage_report.enabled`). An operator with no
// extender network has them off.
func extenderCamouflageReportEnabled() bool {
	config, err := EnvExtenderConfig()
	return err == nil && config.CamouflageReport.Enabled
}

// The hmac domain of a client's rate budget key.
const extenderCamouflageReportRateKeyDomain = "ur-extender-camouflage-report-rate-v1"

// Counts one report in a client's window, a key that expires when the window ends.
var extenderCamouflageReportRateBudgetScript = redis.NewScript(`
local count = redis.call('INCR', KEYS[1])
redis.call('PEXPIREAT', KEYS[1], ARGV[1])
return count
`)

// The key of a client's rate budget in the window that holds now. It holds a keyed
// hash of the client id, so what Redis holds names no client to whoever reads it.
func extenderCamouflageReportRateBudgetKey(secret []byte, clientId server.Id, window int64) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(extenderCamouflageReportRateKeyDomain))
	mac.Write(clientId.Bytes())
	return fmt.Sprintf("extender_camouflage_report_rate_%s_%d", hex.EncodeToString(mac.Sum(nil)[:16]), window)
}

// Counts one report in the client's fixed window at now, one bounded command that
// is never retried, and says whether it fits.
func spendExtenderCamouflageReportRateBudget(ctx context.Context, clientId server.Id, now time.Time) (allowed bool, returnErr error) {
	config, err := EnvExtenderConfig()
	if err != nil {
		return false, err
	}
	secret, err := config.DirectorySecret()
	if err != nil {
		return false, err
	}
	windowMillis := extenderCamouflageReportRateWindow.Milliseconds()
	window := now.UnixMilli() / windowMillis
	key := extenderCamouflageReportRateBudgetKey(secret, clientId, window)

	redisCtx, redisCancel := context.WithTimeout(ctx, extenderCamouflageReportRedisTimeout)
	defer redisCancel()
	var count int64
	returnErr = server.RedisWithDeadline(redisCtx, func(r server.RedisClient) error {
		var err error
		count, err = extenderCamouflageReportRateBudgetScript.Run(redisCtx, r, []string{key}, (window+1)*windowMillis).Int64()
		return err
	}, server.OptRedisNoPreflight(), server.OptRedisKeepLateReplies())
	if returnErr != nil {
		return false, returnErr
	}
	return count <= extenderCamouflageReportRateLimit, nil
}

// Claims a report id for extenderCamouflageReportClaimTtl. Nothing about the
// caller is in the key or the value.
func claimExtenderCamouflageReport(ctx context.Context, reportId string) (first bool, returnErr error) {
	redisCtx, redisCancel := context.WithTimeout(ctx, extenderCamouflageReportRedisTimeout)
	defer redisCancel()
	returnErr = server.RedisWithDeadline(redisCtx, func(r server.RedisClient) error {
		var err error
		first, err = r.SetNX(redisCtx, "extender_camouflage_report_id_"+reportId, "1", extenderCamouflageReportClaimTtl).Result()
		return err
	}, server.OptRedisNoPreflight(), server.OptRedisKeepLateReplies())
	return first, returnErr
}

// ExtenderCamouflageReport backs `POST /network/extender-camouflage-report`:
// one report, from a body the handler has bounded.
func ExtenderCamouflageReport(
	body io.Reader,
	clientSession *session.ClientSession,
) (*ExtenderCamouflageReportResult, error) {
	return defaultExtenderCamouflageReporting.report(body, clientSession), nil
}

// One report. Everything a client got wrong is counted and answered, and logged
// at V(1) only, by its reason and nothing from the report.
func (self *extenderCamouflageReporting) report(
	body io.Reader,
	clientSession *session.ClientSession,
) *ExtenderCamouflageReportResult {
	if !self.enabled() {
		self.metrics.reports.WithLabelValues(extenderCamouflageReportOutcomeDisabled).Inc()
		return &ExtenderCamouflageReportResult{Outcome: ExtenderCamouflageReportDisabled}
	}
	refuse := func(reason string, outcome string) *ExtenderCamouflageReportResult {
		self.metrics.reports.WithLabelValues(reason).Inc()
		if glog.V(1) {
			glog.Infof("[extender]camouflage report %s\n", reason)
		}
		return &ExtenderCamouflageReportResult{Outcome: outcome}
	}
	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		return refuse(extenderCamouflageReportOutcomeOutOfBounds, ExtenderCamouflageReportInvalid)
	}
	clientId := *clientSession.ByJwt.ClientId

	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return refuse(extenderCamouflageReportOutcomeTooLarge, ExtenderCamouflageReportInvalid)
		}
		return refuse(extenderCamouflageReportOutcomeMalformed, ExtenderCamouflageReportInvalid)
	}
	args := &ExtenderCamouflageReportArgs{}
	if err := json.Unmarshal(bodyBytes, args); err != nil {
		return refuse(extenderCamouflageReportOutcomeMalformed, ExtenderCamouflageReportInvalid)
	}
	if !args.inBounds() {
		return refuse(extenderCamouflageReportOutcomeOutOfBounds, ExtenderCamouflageReportInvalid)
	}

	now := self.now()
	allowed, err := self.spendRateBudget(clientSession.Ctx, clientId, now)
	if err != nil {
		return refuse(extenderCamouflageReportOutcomeUnavailable, ExtenderCamouflageReportUnavailable)
	}
	if !allowed {
		self.metrics.reports.WithLabelValues(extenderCamouflageReportOutcomeRateLimited).Inc()
		if glog.V(1) {
			glog.Infof("[extender]camouflage report %s\n", extenderCamouflageReportOutcomeRateLimited)
		}
		return &ExtenderCamouflageReportResult{
			Outcome:           ExtenderCamouflageReportRateLimited,
			RetryAfterSeconds: extenderCamouflageReportWindowLeftSeconds(now),
		}
	}
	// read before the claim, so a report the fleet holds up can be posted again
	fleet := self.fleet(clientSession.Ctx, now)
	if fleet == nil {
		return refuse(extenderCamouflageReportOutcomeUnavailable, ExtenderCamouflageReportUnavailable)
	}
	first, err := self.claimReport(clientSession.Ctx, args.ReportId)
	if err != nil {
		return refuse(extenderCamouflageReportOutcomeUnavailable, ExtenderCamouflageReportUnavailable)
	}
	if !first {
		self.metrics.reports.WithLabelValues(extenderCamouflageReportOutcomeDuplicate).Inc()
		return &ExtenderCamouflageReportResult{Outcome: ExtenderCamouflageReportDuplicate}
	}

	dimensions := &extenderCamouflageReportDimensions{
		country:         self.reportCountry(clientSession.ClientAddress, clientId, fleet),
		family:          extenderReleaseFamily(args.IpVersions),
		clientVersion:   server.AppVersionLabel(args.ClientVersion, now),
		resourceVersion: self.labels().resourceVersion(args.ResourceVersion),
	}
	if args.Client != nil {
		self.metrics.observeClient(args.Client, dimensions)
	}
	outcome := extenderCamouflageReportOutcomeAccepted
	if args.Extender != nil {
		if fleet.owner(clientId) {
			self.metrics.observeExtender(args.Extender, dimensions, self.labels())
		} else {
			// never named to the caller: a sender has nothing to retry, and the
			// answer would say who the operator counts as an extender
			outcome = extenderCamouflageReportOutcomeAcceptedExtenderIgnored
		}
	}
	self.metrics.reports.WithLabelValues(outcome).Inc()
	return &ExtenderCamouflageReportResult{Outcome: ExtenderCamouflageReportAccepted}
}

// The country label of the caller: the server's own lookup of its address.
func (self *extenderCamouflageReporting) reportCountry(
	clientAddress string,
	clientId server.Id,
	fleet *extenderCamouflageFleet,
) string {
	clientIpStr, _, err := server.SplitClientAddress(clientAddress)
	if err != nil {
		return extenderReleaseCountryUnknown
	}
	clientIp, err := netip.ParseAddr(clientIpStr)
	if err != nil {
		return extenderReleaseCountryUnknown
	}
	if fleet.relayed(clientIp, clientId) {
		return extenderCamouflageReportCountryRelayed
	}
	countryCode, err := self.country(clientIp)
	if err != nil {
		return extenderReleaseCountryUnknown
	}
	return extenderReleaseCountryLabel(countryCode)
}

// Seconds until the rate window that holds now ends, rounded up.
func extenderCamouflageReportWindowLeftSeconds(now time.Time) int {
	windowMillis := extenderCamouflageReportRateWindow.Milliseconds()
	leftMillis := windowMillis - now.UnixMilli()%windowMillis
	return int((leftMillis + 999) / 1000)
}

// Whether the report is within every bound of the route. A report past any of
// them is refused whole: the sender stays inside them, so this is not a sender
// that behaves.
func (self *ExtenderCamouflageReportArgs) inBounds() bool {
	if !extenderCamouflageReportIdInBounds(self.ReportId) {
		return false
	}
	if extenderCamouflageReportMaxClientVersionByteCount < len(self.ClientVersion) {
		return false
	}
	if self.ResourceVersion < 0 || extenderCamouflageReportMaxResourceVersion < self.ResourceVersion {
		return false
	}
	if extenderCamouflageReportMaxIpVersionCount < len(self.IpVersions) {
		return false
	}
	for _, ipVersion := range self.IpVersions {
		if ipVersion != 4 && ipVersion != 6 {
			return false
		}
	}
	if self.Client == nil && self.Extender == nil {
		return false
	}
	if self.Client != nil {
		for _, count := range self.Client.outcomeCounts() {
			if !extenderCamouflageReportCountInBounds(count.count, extenderCamouflageReportMaxClientCount) {
				return false
			}
		}
	}
	if self.Extender != nil {
		for _, count := range self.Extender.outcomeCounts() {
			if !extenderCamouflageReportCountInBounds(count.count, extenderCamouflageReportMaxExtenderCount) {
				return false
			}
		}
		if extenderCamouflageReportMaxDegradedReasonByteCount < len(self.Extender.DegradedReason) {
			return false
		}
		if extenderCamouflageReportMaxTargetCount < len(self.Extender.Targets) {
			return false
		}
		for _, target := range self.Extender.Targets {
			if target == nil || extenderCamouflageReportMaxTargetNameByteCount < len(target.Name) ||
				!extenderCamouflageReportCountInBounds(target.SpliceCount, extenderCamouflageReportMaxTargetSpliceCount) ||
				!extenderCamouflageReportCountInBounds(target.RelayedByteCount, extenderCamouflageReportMaxTargetByteCount) {
				return false
			}
		}
	}
	return true
}

func extenderCamouflageReportCountInBounds(count int64, maxCount int64) bool {
	return 0 <= count && count <= maxCount
}

// A report id is 32 lower case hex digits, so what is claimed in Redis has one
// form and one length.
func extenderCamouflageReportIdInBounds(reportId string) bool {
	if len(reportId) != extenderCamouflageReportIdLength {
		return false
	}
	for _, c := range []byte(reportId) {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// How many versions around its own a server names in a resource version label.
// A client may hold a newer list than the server's build, or an older one, and
// past this the label is other.
const extenderCamouflageReportResourceVersionWindow = 4

// The most borrowed names a target label may name.
const extenderCamouflageReportMaxTargetLabelCount = 256

// The fixed sets a report's words are checked against: drawn from the borrow
// lists bundled with this server's build, so they change only with a release.
type extenderCamouflageReportLabels struct {
	minResourceVersion int
	maxResourceVersion int
	// the normalized name of each borrowed site a target label may name
	targets map[string]bool
}

func bundledExtenderCamouflageReportLabels() *extenderCamouflageReportLabels {
	return newExtenderCamouflageReportLabels(connect.ExtenderCamouflageBorrowLists())
}

func newExtenderCamouflageReportLabels(borrowLists []*connect.ExtenderBorrowList) *extenderCamouflageReportLabels {
	version := 0
	targets := map[string]bool{}
	for _, borrowList := range borrowLists {
		version = max(version, borrowList.Version)
		for _, name := range borrowList.AcceptNames() {
			if len(targets) < extenderCamouflageReportMaxTargetLabelCount {
				targets[name] = true
			}
		}
	}
	return &extenderCamouflageReportLabels{
		minResourceVersion: max(1, version-extenderCamouflageReportResourceVersionWindow),
		maxResourceVersion: version + extenderCamouflageReportResourceVersionWindow,
		targets:            targets,
	}
}

// The resource version label: none for no list, the version itself within the
// window around this build's own, and other past it.
func (self *extenderCamouflageReportLabels) resourceVersion(version int) string {
	switch {
	case version == 0:
		return extenderCamouflageReportResourceVersionNone
	case self.minResourceVersion <= version && version <= self.maxResourceVersion:
		return strconv.Itoa(version)
	default:
		return extenderCamouflageReportLabelOther
	}
}

// The target label of a name in a report: the name when the allowlist holds it,
// and other for anything else.
func (self *extenderCamouflageReportLabels) target(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if self.targets[name] {
		return name
	}
	return extenderCamouflageReportLabelOther
}

// The verdict label of an extender's coverage (extender.ExtenderCamouflageStatus):
// covered only when it says so and names no reason, one of the fixed degraded
// reasons otherwise, and other for anything else.
func extenderCamouflageReportVerdict(covered bool, degradedReason string) string {
	if covered {
		if degradedReason == "" {
			return extenderCamouflageReportVerdictCovered
		}
		return extenderCamouflageReportLabelOther
	}
	switch degradedReason {
	case extenderCamouflageReportVerdictCamouflageOff,
		extenderCamouflageReportVerdictSpliceOff,
		extenderCamouflageReportVerdictSpliceGateClosed,
		extenderCamouflageReportVerdictNoVerifiedTarget,
		extenderCamouflageReportVerdictSpliceFallbackActive,
		extenderCamouflageReportVerdictCoverageIncomplete:
		return degradedReason
	}
	return extenderCamouflageReportLabelOther
}

// Each counter of a client's part with its outcome label.
func (self *ExtenderCamouflageClientCounts) outcomeCounts() []extenderCamouflageOutcomeCount {
	return []extenderCamouflageOutcomeCount{
		{extenderCamouflageClientOutcomeCamouflageWon, self.CamouflageWon},
		{extenderCamouflageClientOutcomeLegacyWon, self.LegacyWon},
		{extenderCamouflageClientOutcomeBothFailed, self.BothFailed},
		{extenderCamouflageClientOutcomeRefused, self.Refused},
		{extenderCamouflageClientOutcomeCamouflageUnauthenticated, self.CamouflageUnauthenticated},
		{extenderCamouflageClientOutcomeCamouflageLeafRejected, self.CamouflageLeafRejected},
		{extenderCamouflageClientOutcomeLegacyOnlyNoKey, self.LegacyOnlyNoKey},
		{extenderCamouflageClientOutcomeLegacyOnlyFingerprint, self.LegacyOnlyFingerprint},
		{extenderCamouflageClientOutcomeLegacyOnlyNoFrontName, self.LegacyOnlyNoFrontName},
	}
}

// Each counter of an extender's part with its outcome label.
func (self *ExtenderCamouflageExtenderCounts) outcomeCounts() []extenderCamouflageOutcomeCount {
	return []extenderCamouflageOutcomeCount{
		{extenderCamouflageExtenderOutcomeAuthenticated, self.Authenticated},
		{extenderCamouflageExtenderOutcomeSpliced, self.Spliced},
		{extenderCamouflageExtenderOutcomeTerminated, self.Terminated},
		{extenderCamouflageExtenderOutcomeSpliceFallbackTerminated, self.SpliceFallbackTerminated},
		{extenderCamouflageExtenderOutcomeReplayRefused, self.ReplayRefused},
		{extenderCamouflageExtenderOutcomeTimeWindowFailed, self.TimeWindowFailed},
		{extenderCamouflageExtenderOutcomeSpliceDialFailed, self.SpliceDialFailed},
		{extenderCamouflageExtenderOutcomeBorrowVerifyFailed, self.BorrowVerifyFailed},
	}
}
