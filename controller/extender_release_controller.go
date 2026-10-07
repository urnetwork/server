package controller

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"

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
// places it in; enough distinct reporters while the operator's own probe
// still reaches the extender is the per-country blocked state a release
// skips (model.NetworkExtenderBlockedInCountry).
//
// Both are 200 answers with an `error` field on refusal, as activation is:
// the caller is the sdk's refresh pass, which needs to tell a limit from a
// fault. The vantage of a release is the requester's address prefix
// (connect.ExtenderVantagePrefix); the operator has no asn source yet, and
// the prefix stands in for it as the policy allows.

const (
	// Block reports per user per hour. A report is one line about one
	// extender; a client in trouble names a handful, not hundreds.
	ExtenderBlockReportRateLimitAction = "extender_block_report"
	ExtenderBlockReportRateLimit       = 64
	ExtenderBlockReportRateLimitWindow = time.Hour
)

type ExtenderReleaseArgs struct {
	// the families the client can dial; empty is any
	IpVersions []int `json:"ip_versions"`
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
	refuse := func(message string) (*ExtenderReleaseResult, error) {
		return &ExtenderReleaseResult{
			Records: []string{},
			Error:   message,
		}, nil
	}
	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		return refuse("a release requires a client")
	}
	for _, ipVersion := range args.IpVersions {
		if ipVersion != 4 && ipVersion != 6 {
			return refuse("ip_versions must be 4 or 6")
		}
	}

	config, err := EnvExtenderConfig()
	if err != nil {
		return refuse(err.Error())
	}
	rootPrivateKey, err := config.RootPrivateKey()
	if err != nil {
		return refuse(err.Error())
	}
	secret, err := config.DirectorySecret()
	if err != nil {
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
	eligible := func(keyHex string) bool {
		extender, ok := byKeyHex[keyHex]
		if !ok {
			return false
		}
		if len(args.IpVersions) == 0 {
			return true
		}
		for _, address := range extender.Addresses {
			if slices.Contains(args.IpVersions, address.IpVersion) {
				return true
			}
		}
		return false
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
	}, keyHexes)
	if err != nil {
		return refuse(err.Error())
	}

	result := &ExtenderReleaseResult{
		Records:   []string{},
		Epoch:     release.Epoch,
		Count:     release.Count,
		Probation: release.Probation,
	}
	for _, keyHex := range release.KeyHexes {
		extender := byKeyHex[keyHex]
		// signed now, so the record is as fresh as the release and a client
		// that asks again within its epoch refreshes the same record
		record, _, err := SignExtenderRecord(
			config,
			rootPrivateKey,
			extender.Extender,
			extender.Addresses,
			server.NowUtc(),
		)
		if err != nil {
			glog.Errorf("[extender]release record for %s not signed: %s\n", extender.Extender.ExtenderId, err)
			continue
		}
		recordBase64, err := encodeExtenderRecord(record)
		if err != nil {
			continue
		}
		result.Records = append(result.Records, recordBase64)
	}
	return result, nil
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
	model.RecordNetworkExtenderBlockReport(
		clientSession.Ctx,
		extender.ExtenderId,
		*clientSession.ByJwt.ClientId,
		countryCode,
		server.NowUtc(),
	)
	return &ExtenderBlockReportResult{Recorded: true}, nil
}
