// The validator head summary ingest, POST /sn/head-summary (LEADERTOP.md
// §6.1, design question 9; sn docs/spec/head-summary-v1.md). After each
// native tempo's steering intent the SN25 validator uploads a signed summary
// of its head result: every eligible head UID in rank order with its exact
// score, its fleet identities and whether it was selected. The board shows it
// as the validator's own ranking of claimed fleets.
//
// sn's reference decoder (protocol/headsummary) does the spec's protocol
// checks: the 16 MiB bound, one strict canonical envelope, every value, the
// rank order and selection (protocol/headscore), and the sr25519 signature by
// validator_hotkey over the domain-separated digest. The operator adds only
// what the decoder leaves to it: the client session (401, before the body is
// read), rate limits per session and per hotkey (429), and whether it accepts
// validator_hotkey as a validator of its netuid (403). The session authorizes
// nothing by itself: the hotkey signature is the summary's only authority.
//
// Storage is keyed by (netuid, validator_hotkey, native_epoch), the tempo
// index; a greater native_snapshot_block replaces a stored summary, any other
// is kept, and both answer 204.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/urfoundation/sn/v2026/protocol/headscore"
	"github.com/urfoundation/sn/v2026/protocol/headsummary"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// SnHeadSummary is the canonical envelope: sn's reference type, so the body
// re-encodes byte for byte and h256 values stay 0x-hex strings.
type SnHeadSummary = headsummary.Summary

const (
	// the largest body the operator reads; 413 above it
	SnHeadSummaryMaxBytes = headsummary.MaxBytes
	// one summary per tempo and a few retries; well above that is abuse
	snHeadSummaryClientLimit = 60
	snHeadSummaryHotkeyLimit = 30
	snHeadSummaryLimitWindow = time.Hour
	// summaries are kept for the board's 7 days
	snHeadSummaryRetention = 7 * 24 * time.Hour
)

// A refused summary. Invalid is 400 and NotAccepted 403.
var (
	ErrSnHeadSummaryInvalid     = errors.New("head summary is invalid")
	ErrSnHeadSummaryNotAccepted = errors.New("head summary validator is not accepted")
	ErrSnHeadSummaryRateLimited = errors.New("head summary uploads are rate limited")
	ErrSnHeadSummaryTooLarge    = errors.New("head summary body is over 16 MiB")
)

// DecodeSnHeadSummary is sn's strict decoder: a canonical, consistent and
// correctly signed summary, or ErrSnHeadSummaryInvalid. It does not decide
// whether the operator trusts the hotkey.
func DecodeSnHeadSummary(body []byte) (*SnHeadSummary, error) {
	summary, err := headsummary.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrSnHeadSummaryInvalid, err)
	}
	return summary, nil
}

// Whether the operator accepts validator_hotkey for netuid: a hotkey listed in
// st.yml's validator_hotkeys, or, when st.yml lists none, the hotkey that holds
// UID 1 (UR's validator, LEADERTOP.md §3.6) with a validator permit at the
// newest incentive census, read in this tempo or the one before.
func snHeadSummaryAccepted(cfg *StConfig, snapshot *model.SnIncentiveSnapshot, summary *SnHeadSummary, hotkey [32]byte) bool {
	if cfg == nil || uint64(summary.Netuid) != cfg.Netuid {
		return false
	}
	if 0 < len(cfg.ValidatorHotkeys) {
		return slices.Contains(cfg.ValidatorHotkeys, hotkey)
	}
	if snapshot == nil || summary.ValidatorUID != snIncentiveDefaultReferenceUid || snapshot.SubnetEpochIndex+1 < summary.NativeEpoch {
		return false
	}
	holder, ok := snapshot.IncentiveFor(summary.ValidatorUID, hotkey)
	return ok && holder.ValidatorPermit
}

// The outcome of an accepted upload.
const (
	SnHeadSummaryStored = "stored"
	// an equal or later snapshot of the same tempo is already stored
	SnHeadSummaryKept = "kept"
)

// SnUploadHeadSummary verifies, accepts and stores one summary body for an
// authenticated client session. A refusal wraps ErrSnHeadSummaryInvalid (400),
// ErrSnHeadSummaryNotAccepted (403) or ErrSnHeadSummaryRateLimited (429).
func SnUploadHeadSummary(ctx context.Context, clientSession *session.ClientSession, body []byte) (string, error) {
	result, err := snUploadHeadSummary(ctx, stConfig(), clientSession, body, server.NowUtc())
	observeSnHeadSummaryUpload(result, err)
	return result, err
}

// SnHeadSummaryClientAdmission counts one upload of the client session before
// its body is read; false is 429.
func SnHeadSummaryClientAdmission(ctx context.Context, clientSession *session.ClientSession) (bool, error) {
	if clientSession == nil || clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		return false, nil
	}
	count, err := server.IncrementRateLimitWindow(ctx, fmt.Sprintf("sn_head_summary_client_%s", clientSession.ByJwt.ClientId), snHeadSummaryLimitWindow)
	if err != nil {
		return false, err
	}
	if snHeadSummaryClientLimit < count {
		observeSnHeadSummaryUpload("", ErrSnHeadSummaryRateLimited)
		return false, nil
	}
	return true, nil
}

func snUploadHeadSummary(ctx context.Context, cfg *StConfig, clientSession *session.ClientSession, body []byte, now time.Time) (string, error) {
	if cfg == nil || cfg.DeploymentKey() == "" {
		return "", errStNotConfigured
	}
	summary, err := DecodeSnHeadSummary(body)
	if err != nil {
		return "", err
	}
	if summary.Schema == headsummary.SchemaV2 && summary.EvidenceRecipient != cfg.NoId {
		return "", fmt.Errorf("%w: evidence is addressed to another operator", ErrSnHeadSummaryNotAccepted)
	}
	// the decoder accepted the hotkey's form
	hotkey, err := headsummary.ParseHex32(summary.ValidatorHotkey)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrSnHeadSummaryInvalid, err)
	}
	count, err := server.IncrementRateLimitWindow(ctx, fmt.Sprintf("sn_head_summary_hotkey_%x", hotkey), snHeadSummaryLimitWindow)
	if err != nil {
		return "", err
	}
	if snHeadSummaryHotkeyLimit < count {
		return "", ErrSnHeadSummaryRateLimited
	}
	if !snHeadSummaryAccepted(cfg, model.GetLatestSnIncentiveSnapshot(ctx, cfg.DeploymentKey()), summary, hotkey) {
		return "", fmt.Errorf("%w: hotkey 0x%x for netuid %d", ErrSnHeadSummaryNotAccepted, hotkey, summary.Netuid)
	}
	stored, err := snStoredHeadSummary(summary, body, now)
	if err != nil {
		return "", err
	}
	if !model.PutSnHeadSummary(ctx, stored) {
		return SnHeadSummaryKept, nil
	}
	if removed := model.RemoveSnHeadSummariesBefore(ctx, now.Add(-snHeadSummaryRetention)); 0 < removed {
		glog.Infof("[sn]removed %d head summaries past retention\n", removed)
	}
	snHeadSummaryEntries.Set(float64(len(summary.Entries)))
	snHeadSummaryLastStored.Set(float64(now.Unix()))
	return SnHeadSummaryStored, nil
}

// The stored form of a decoded summary: the envelope's identities, the body,
// and one row per fleet identity with its entry's rank, score and selection.
// The decoder has already checked every value it parses here.
func snStoredHeadSummary(summary *SnHeadSummary, body []byte, receivedAt time.Time) (*model.SnHeadSummary, error) {
	stored := &model.SnHeadSummary{
		Netuid: summary.Netuid, ValidatorUid: summary.ValidatorUID, NativeEpoch: summary.NativeEpoch,
		NativeSnapshotBlock: summary.NativeSnapshotBlock, EvmSnapshotBlock: summary.EVMSnapshotBlock,
		SettlementEpoch: summary.SettlementEpoch, PolicyId: summary.PolicyID,
		MeasurementArtifactHash: summary.MeasurementArtifactHash, MaximumHeadFleets: summary.MaximumHeadFleets,
		ReplicaGenesis: summary.Schema == headsummary.SchemaV2 && summary.PreviousMeasurementArtifactHash == "" && summary.EvidenceEmptyPrior,
		EntryCount:     len(summary.Entries), SelectedCount: len(summary.SelectedUIDs),
		SigningHash: summary.SigningHash, ReceivedAt: receivedAt, Body: append([]byte(nil), body...),
	}
	var err error
	parse := func(label string, value string) [32]byte {
		parsed, parseErr := headsummary.ParseHex32(value)
		if parseErr != nil && err == nil {
			err = fmt.Errorf("%w: %s: %s", ErrSnHeadSummaryInvalid, label, parseErr)
		}
		return parsed
	}
	stored.ValidatorHotkey = parse("validator_hotkey", summary.ValidatorHotkey)
	stored.NativeSnapshotHash = parse("native_snapshot_hash", summary.NativeSnapshotHash)
	stored.EvmSnapshotHash = parse("evm_snapshot_hash", summary.EVMSnapshotHash)
	stored.PolicyHash = parse("policy_hash", summary.PolicyHash)
	signedAt, signedAtErr := time.Parse(time.RFC3339Nano, summary.SignedAt)
	if signedAtErr != nil {
		return nil, fmt.Errorf("%w: signed_at: %s", ErrSnHeadSummaryInvalid, signedAtErr)
	}
	stored.SignedAt = signedAt
	for _, entry := range summary.Entries {
		hotkey := parse("entry hotkey", entry.Hotkey)
		score, scoreErr := headscore.DecodeRational(entry.Score)
		if scoreErr != nil {
			return nil, fmt.Errorf("%w: entry %d score: %s", ErrSnHeadSummaryInvalid, entry.Rank, scoreErr)
		}
		for _, fleet := range entry.Fleets {
			stored.Fleets = append(stored.Fleets, &model.SnHeadSummaryFleetScore{
				Key:  model.SnFleetKey{FleetId: parse("fleet_id", fleet.FleetID), Hotkey: hotkey, Generation: fleet.Generation, Uid: entry.UID},
				Rank: int(entry.Rank), ScoreNumerator: entry.Score.Numerator, ScoreDenominator: entry.Score.Denominator,
				Score: headscore.Float64(score), Selected: entry.Selected,
			})
		}
	}
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// SnLatestHeadSummary returns the newest stored summary of the reference
// validator for the board: the holder of the newest incentive census's
// reference UID, or the newest of any accepted validator before a census. Nil
// before the first summary.
func SnLatestHeadSummary(ctx context.Context) (*model.SnHeadSummary, error) {
	cfg := stConfig()
	if cfg == nil || cfg.DeploymentKey() == "" || cfg.Netuid == 0 || cfg.Netuid > uint64(^uint16(0)) {
		return nil, errStNotConfigured
	}
	var hotkey *[32]byte
	if snapshot := model.GetLatestSnIncentiveSnapshot(ctx, cfg.DeploymentKey()); snapshot != nil && snapshot.ReferenceUid != nil {
		if int(*snapshot.ReferenceUid) < len(snapshot.Uids) && snapshot.Uids[*snapshot.ReferenceUid].HolderRead {
			reference := snapshot.Uids[*snapshot.ReferenceUid].Hotkey
			hotkey = &reference
		}
	}
	return model.GetLatestSnHeadSummary(ctx, uint16(cfg.Netuid), hotkey), nil
}
