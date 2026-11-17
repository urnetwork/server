package controller

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The gated tier's two routes (connect/EXTENDER.md R3, R4).
//
// A release hands an authenticated identity -- the account and the device of
// its client jwt -- a few records of the durable fleet, chosen by
// connect.ExtenderReleasePolicy over the database ledger, and signed fresh so
// the identity refreshes them by asking again. A block report is a client's
// word that it could not reach an extender, from the country its address
// places it in; enough distinct networks reporting while the operator's own
// probe still reaches the extender is the per-country blocked state a release
// skips (model.NetworkExtenderBlockedInCountry).
//
// Both are 200 answers with an `error` field on refusal, as activation is:
// the caller is the sdk's refresh pass, which needs to tell a limit from a
// fault. The vantage of a release is the requester's address prefix
// (connect.ExtenderVantagePrefix); the operator has no asn source yet, and
// the prefix stands in for it as the policy allows.
//
// A release also answers with an outcome and, where the policy knows one, a
// retry time (connect/EXTENDER-UPDATE2.md §1.1, contract R), both additive:
// a client that predates them reads `records` and `error` as before. The
// client's declared capabilities decide which records are eligible and
// nothing else (§3.2, extender_capability.go). A candidate's record is signed
// when the policy first asks about it, so the requirement it is checked
// against is the one in the record handed out, and a record that cannot be
// signed is never charged.

const (
	// Block reports per user per hour. A report is one line about one
	// extender; a client in trouble names a handful, not hundreds.
	ExtenderBlockReportRateLimitAction = model.AccountActionExtenderBlockReport
	ExtenderBlockReportRateLimit       = 64
	ExtenderBlockReportRateLimitWindow = time.Hour
)

type ExtenderReleaseArgs struct {
	// the families the client can dial; empty is any
	IpVersions []int `json:"ip_versions"`
	// what the client can use, such as tcp-camouflage-v1 (contract R): it
	// decides which records are eligible and nothing else, and a value the
	// server does not know is ignored
	Capabilities []string `json:"capabilities,omitempty"`
}

type ExtenderReleaseResult struct {
	// base64 serialized protocol.ExtenderRecord messages, the release
	Records []string `json:"records"`
	// the epoch the release was dealt in
	Epoch uint64 `json:"epoch"`
	// how many the identity is entitled to this epoch
	Count     int  `json:"count"`
	Probation bool `json:"probation"`
	// why the release was refused, empty on success
	Error string `json:"error,omitempty"`
	// why the answer holds what it does (connect.ExtenderReleaseOutcome*); a
	// client treats one it does not know as empty
	Outcome string `json:"outcome,omitempty"`
	// seconds until asking again can change the answer, when known
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// The release policy's settings, with the server clock.
func extenderReleaseSettings() *connect.ExtenderReleaseSettings {
	settings := connect.DefaultExtenderReleaseSettings()
	settings.Now = server.NowUtc
	return settings
}

// ExtenderRelease backs `POST /network/extender-release` (R3).
func ExtenderRelease(
	args *ExtenderReleaseArgs,
	clientSession *session.ClientSession,
) (*ExtenderReleaseResult, error) {
	return extenderRelease(args, clientSession, defaultExtenderReleaseMetrics)
}

// One release, counted in `metrics`.
func extenderRelease(
	args *ExtenderReleaseArgs,
	clientSession *session.ClientSession,
	metrics *extenderReleaseMetrics,
) (*ExtenderReleaseResult, error) {
	family := extenderReleaseFamilyUnknown
	declared := extenderDeclaredCapabilities(args.Capabilities)
	capable := slices.Contains(declared, connect.ExtenderCapabilityTcpCamouflageV1)
	countryCode := ""
	answer := func(result *ExtenderReleaseResult) (*ExtenderReleaseResult, error) {
		metrics.observe(result.Outcome, family, capable, countryCode)
		return result, nil
	}
	refuse := func(outcome string, retryAfter time.Duration, message string) (*ExtenderReleaseResult, error) {
		return answer(&ExtenderReleaseResult{
			Records:           []string{},
			Error:             message,
			Outcome:           outcome,
			RetryAfterSeconds: extenderReleaseRetryAfterSeconds(retryAfter),
		})
	}
	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		return refuse(connect.ExtenderReleaseOutcomeUnavailable, 0, "a release requires a client")
	}
	for _, ipVersion := range args.IpVersions {
		if ipVersion != 4 && ipVersion != 6 {
			return refuse(connect.ExtenderReleaseOutcomeUnavailable, 0, "ip_versions must be 4 or 6")
		}
	}
	family = extenderReleaseFamily(args.IpVersions)

	config, err := EnvExtenderConfig()
	if err != nil {
		return refuse(connect.ExtenderReleaseOutcomeUnavailable, 0, extenderConfigRefusal(err))
	}
	rootPrivateKey, err := config.RootPrivateKey()
	if err != nil {
		return refuse(connect.ExtenderReleaseOutcomeUnavailable, 0, extenderConfigRefusal(err))
	}
	secret, err := config.DirectorySecret()
	if err != nil {
		return refuse(connect.ExtenderReleaseOutcomeUnavailable, 0, extenderConfigRefusal(err))
	}

	clientIpStr, _, err := server.SplitClientAddress(clientSession.ClientAddress)
	if err != nil {
		return refuse(connect.ExtenderReleaseOutcomeUnavailable, 0, "the caller address is not readable")
	}
	clientIp, err := netip.ParseAddr(clientIpStr)
	if err != nil {
		return refuse(connect.ExtenderReleaseOutcomeUnavailable, 0, "the caller address is not readable")
	}
	if ipInfo, err := server.GetIpInfo(clientIp); err == nil {
		countryCode = ipInfo.CountryCode
	} else if glog.V(1) {
		glog.Infof("[extender]no location for the releasing address: %s\n", err)
	}

	networkId := clientSession.ByJwt.NetworkId
	clientId := *clientSession.ByJwt.ClientId
	identity := slices.Concat(networkId.Bytes(), clientId.Bytes())
	var identityCreateTime time.Time
	if networkClient := model.GetNetworkClient(clientSession.Ctx, clientId); networkClient != nil {
		identityCreateTime = networkClient.CreateTime
	}

	gated := model.GetActiveGatedNetworkExtenders(clientSession.Ctx)
	extenderIds := model.NetworkExtenderIdsByKeyHex(gated)
	byKeyHex := map[string]*model.NetworkExtenderWithAddresses{}
	keyHexes := []string{}
	for _, extender := range gated {
		keyHex := hex.EncodeToString(extender.Extender.PublicKey)
		byKeyHex[keyHex] = extender
		keyHexes = append(keyHexes, keyHex)
	}

	// Each candidate's record is signed once, when the policy first asks
	// about it, and a released record is the one signed here: signed now, so
	// the record is as fresh as the release and a client that asks again
	// within its epoch refreshes the same record.
	issueTime := server.NowUtc()
	signed := map[string]*extenderReleaseRecord{}
	sign := func(keyHex string) *extenderReleaseRecord {
		if releaseRecord, ok := signed[keyHex]; ok {
			return releaseRecord
		}
		releaseRecord, err := signExtenderReleaseRecord(config, rootPrivateKey, byKeyHex[keyHex], issueTime)
		if err != nil {
			glog.Errorf("[extender]release record for %s not signed: %s\n", byKeyHex[keyHex].Extender.ExtenderId, err)
		}
		signed[keyHex] = releaseRecord
		return releaseRecord
	}
	eligible := func(keyHex string) bool {
		extender, ok := byKeyHex[keyHex]
		if !ok {
			return false
		}
		if 0 < len(args.IpVersions) && !slices.ContainsFunc(extender.Addresses, func(address *model.NetworkExtenderAddress) bool {
			return slices.Contains(args.IpVersions, address.IpVersion)
		}) {
			return false
		}
		// a record that cannot be signed cannot be handed out, so it is
		// never charged
		return sign(keyHex) != nil
	}
	compatible := func(keyHex string) bool {
		releaseRecord := sign(keyHex)
		return releaseRecord != nil && connect.ExtenderCapabilitiesSatisfied(releaseRecord.requiredCapabilities, declared)
	}

	policy := connect.NewExtenderReleasePolicy(
		secret,
		model.NewNetworkExtenderReleaseLedger(clientSession.Ctx, extenderIds),
		model.NewNetworkExtenderBlockedSource(clientSession.Ctx, extenderIds, nil),
		extenderReleaseSettings(),
	)
	release, err := policy.Release(&connect.ExtenderReleaseRequest{
		Identity:           identity,
		IdentityCreateTime: identityCreateTime,
		Vantage:            connect.ExtenderVantagePrefix(clientIp),
		CountryCode:        countryCode,
		Eligible:           eligible,
		Compatible:         compatible,
	}, keyHexes)
	if err != nil {
		var refusal *connect.ExtenderReleaseRefusal
		if errors.As(err, &refusal) {
			return refuse(refusal.Outcome, refusal.RetryAfter, err.Error())
		}
		return refuse(connect.ExtenderReleaseOutcomeUnavailable, 0, err.Error())
	}

	result := &ExtenderReleaseResult{
		Records:           []string{},
		Epoch:             release.Epoch,
		Count:             release.Count,
		Probation:         release.Probation,
		Outcome:           release.Outcome,
		RetryAfterSeconds: extenderReleaseRetryAfterSeconds(release.RetryAfter),
	}
	for _, keyHex := range release.KeyHexes {
		// eligible signed every record the policy could release
		releaseRecord := signed[keyHex]
		if releaseRecord == nil {
			continue
		}
		result.Records = append(result.Records, releaseRecord.recordBase64)
	}
	if result.Outcome == connect.ExtenderReleaseOutcomeOk && len(result.Records) == 0 {
		result.Outcome = connect.ExtenderReleaseOutcomeUnavailable
	}
	return answer(result)
}

// A candidate record of a release, signed and encoded, with what it requires.
type extenderReleaseRecord struct {
	recordBase64         string
	requiredCapabilities []string
}

// Signs one candidate's record and reads back the requirement it was signed
// with.
func signExtenderReleaseRecord(
	config *ExtenderConfig,
	rootPrivateKey ed25519.PrivateKey,
	extender *model.NetworkExtenderWithAddresses,
	issueTime time.Time,
) (*extenderReleaseRecord, error) {
	record, _, err := SignExtenderRecord(config, rootPrivateKey, extender.Extender, extender.Addresses, issueTime)
	if err != nil {
		return nil, err
	}
	body := &protocol.ExtenderRecordBody{}
	if err := proto.Unmarshal(record.Body, body); err != nil {
		return nil, err
	}
	recordBase64, err := encodeExtenderRecord(record)
	if err != nil {
		return nil, err
	}
	return &extenderReleaseRecord{
		recordBase64:         recordBase64,
		requiredCapabilities: body.RequiredCapabilities,
	}, nil
}

// A retry time in whole seconds, rounded up so a client never asks early;
// zero, which the answer omits, when there is none.
func extenderReleaseRetryAfterSeconds(retryAfter time.Duration) int {
	if retryAfter <= 0 {
		return 0
	}
	return int((retryAfter + time.Second - 1) / time.Second)
}

type ExtenderBlockReportArgs struct {
	PublicKeyHex string `json:"public_key_hex"`
}

type ExtenderBlockReportResult struct {
	Recorded bool `json:"recorded"`
	// why the report was not recorded, empty when it was
	Error string `json:"error,omitempty"`
}

// ExtenderBlockReport backs `POST /network/extender-block-report` (R4).
func ExtenderBlockReport(
	args *ExtenderBlockReportArgs,
	clientSession *session.ClientSession,
) (*ExtenderBlockReportResult, error) {
	refuse := func(message string) (*ExtenderBlockReportResult, error) {
		return &ExtenderBlockReportResult{
			Recorded: false,
			Error:    message,
		}, nil
	}
	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		return refuse("a block report requires a client")
	}
	publicKey, err := connect.ParseExtenderPublicKeyHex(args.PublicKeyHex)
	if err != nil {
		return refuse(fmt.Sprintf("the extender public key is not readable: %s", err))
	}
	if err := model.CheckAndRecordAccountActionRateLimit(
		clientSession.Ctx,
		clientSession.ByJwt.UserId,
		ExtenderBlockReportRateLimitAction,
		ExtenderBlockReportRateLimit,
		ExtenderBlockReportRateLimitWindow,
	); err != nil {
		return refuse(err.Error())
	}
	clientIpStr, _, err := server.SplitClientAddress(clientSession.ClientAddress)
	if err != nil {
		return refuse("the caller address is not readable")
	}
	clientIp, err := netip.ParseAddr(clientIpStr)
	if err != nil {
		return refuse("the caller address is not readable")
	}
	countryCode := ""
	if ipInfo, err := server.GetIpInfo(clientIp); err == nil {
		countryCode = ipInfo.CountryCode
	}
	extender := model.GetNetworkExtenderByPublicKey(clientSession.Ctx, publicKey)
	if extender == nil {
		// an unknown key is not an error the caller can act on, and saying
		// which keys are known would be a lookup service
		return &ExtenderBlockReportResult{Recorded: false}, nil
	}
	// a report against an extender the operator's probe does not reach now
	// describes an outage, not a block, and is not recorded
	recorded := model.RecordNetworkExtenderBlockReport(
		clientSession.Ctx,
		extender.ExtenderId,
		*clientSession.ByJwt.ClientId,
		countryCode,
		server.NowUtc(),
	)
	return &ExtenderBlockReportResult{Recorded: recorded}, nil
}
