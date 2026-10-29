// The validator head summary ingest, POST /sn/head-summary (LEADERTOP.md
// §6.1, design question 9; sn docs/spec/head-summary-v1.md). After each
// native tempo's steering intent the SN25 validator uploads a signed summary
// of its head result: every eligible head UID in rank order with its exact
// score, its fleet identities and whether it was selected. The board shows it
// as the validator's own ranking of claimed fleets.
//
// The operator, in the spec's order:
//  1. refuses a body over 16 MiB (413) and an invalid session (401);
//  2. decodes strictly: one object, no unknown or duplicate members, no
//     trailing data, and the body must equal its canonical re-encoding byte for
//     byte;
//  3. checks every value, rank order, selection and the UID partitions;
//  4. recomputes the digest, compares signing_hash and verifies the sr25519
//     signature under validator_hotkey;
//  5. accepts validator_hotkey for netuid (403 otherwise).
//
// The session authorizes nothing by itself: the hotkey signature is the
// summary's only authority. Storage is keyed by (netuid, validator_hotkey,
// native_epoch); a later native_snapshot_block replaces a stored summary, any
// other is kept, and both answer 204. Uploads are rate-limited per client
// session before the body is read and per hotkey after it verifies.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"slices"
	"time"

	"github.com/ChainSafe/go-schnorrkel"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const (
	SnHeadSummarySchemaV1 = "urnetwork-validator-head-summary-v1"
	// the signing domain, followed by one zero byte and the encoding
	snHeadSummaryDomainV1 = "urnetwork/validator/head-summary/v1"
	// the largest body the operator reads
	SnHeadSummaryMaxBytes = 16 * 1024 * 1024
	// the most eligible entries a summary carries
	snHeadSummaryMaxEntries = 4096
	// one summary per tempo and a few retries; well above that is abuse
	snHeadSummaryClientLimit = 60
	snHeadSummaryHotkeyLimit = 30
	snHeadSummaryLimitWindow = time.Hour
	// summaries are kept for the board's 7 days
	snHeadSummaryRetention = 7 * 24 * time.Hour
)

// The canonical envelope, members in the spec's order. h256 values are
// 0x-hex strings so the body re-encodes byte for byte.
type SnHeadSummary struct {
	Schema                  string               `json:"schema"`
	Netuid                  uint16               `json:"netuid"`
	NativeEpoch             uint64               `json:"native_epoch"`
	NativeSnapshotBlock     uint64               `json:"native_snapshot_block"`
	NativeSnapshotHash      string               `json:"native_snapshot_hash"`
	EvmSnapshotBlock        uint64               `json:"evm_snapshot_block"`
	EvmSnapshotHash         string               `json:"evm_snapshot_hash"`
	SettlementEpoch         uint64               `json:"settlement_epoch"`
	PolicyId                uint64               `json:"policy_id"`
	PolicyHash              string               `json:"policy_hash"`
	MeasurementArtifactHash string               `json:"measurement_artifact_hash"`
	ValidatorHotkey         string               `json:"validator_hotkey"`
	ValidatorUid            uint16               `json:"validator_uid"`
	MaximumHeadFleets       uint16               `json:"maximum_head_fleets"`
	Entries                 []SnHeadSummaryEntry `json:"entries"`
	SelectedUids            []uint16             `json:"selected_uids"`
	RejectedUids            []uint16             `json:"rejected_uids"`
	SignedAt                string               `json:"signed_at"`
	SignatureScheme         string               `json:"signature_scheme"`
	SigningHash             string               `json:"signing_hash"`
	Signature               string               `json:"signature"`
}

// One eligible head UID.
type SnHeadSummaryEntry struct {
	Rank     uint32                `json:"rank"`
	Uid      uint16                `json:"uid"`
	Hotkey   string                `json:"hotkey"`
	Score    SnHeadSummaryRational `json:"score"`
	Selected bool                  `json:"selected"`
	Fleets   []SnHeadSummaryFleet  `json:"fleets"`
}

// One fleet identity of an entry, with the entry's uid and hotkey.
type SnHeadSummaryFleet struct {
	FleetId    string `json:"fleet_id"`
	Generation uint64 `json:"generation"`
}

// An exact score in its unique reduced form.
type SnHeadSummaryRational struct {
	Numerator   string `json:"numerator"`
	Denominator string `json:"denominator"`
}

var (
	snHeadSummaryH256        = regexp.MustCompile(`^0x[0-9a-f]{64}$`)
	snHeadSummarySha256      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	snHeadSummarySignature   = regexp.MustCompile(`^0x[0-9a-f]{128}$`)
	snHeadSummaryNumerator   = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	snHeadSummaryDenominator = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// A refused summary. Invalid is 400 and NotAccepted 403.
var (
	ErrSnHeadSummaryInvalid     = errors.New("head summary is invalid")
	ErrSnHeadSummaryNotAccepted = errors.New("head summary validator is not accepted")
	ErrSnHeadSummaryRateLimited = errors.New("head summary uploads are rate limited")
	ErrSnHeadSummaryTooLarge    = errors.New("head summary body is over 16 MiB")
)

func snHeadSummaryInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSnHeadSummaryInvalid, fmt.Sprintf(format, args...))
}

func snHeadSummaryHex32(label string, value string, nonzero bool) ([32]byte, error) {
	if !snHeadSummaryH256.MatchString(value) {
		return [32]byte{}, snHeadSummaryInvalid("%s is not an h256", label)
	}
	var out [32]byte
	// the pattern admits only hex
	hex.Decode(out[:], []byte(value[2:]))
	if nonzero && out == ([32]byte{}) {
		return [32]byte{}, snHeadSummaryInvalid("%s is zero", label)
	}
	return out, nil
}

// DecodeSnHeadSummary decodes a body strictly and checks every value, the
// rank order, the selection and the UID partitions. It does not verify the
// signature.
func DecodeSnHeadSummary(body []byte) (*SnHeadSummary, error) {
	if SnHeadSummaryMaxBytes < len(body) {
		return nil, snHeadSummaryInvalid("body is over %d bytes", SnHeadSummaryMaxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	summary := &SnHeadSummary{}
	if err := decoder.Decode(summary); err != nil {
		return nil, snHeadSummaryInvalid("decode: %s", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, snHeadSummaryInvalid("trailing data after the object")
	}
	if summary.Schema != SnHeadSummarySchemaV1 {
		return nil, snHeadSummaryInvalid("unknown schema %q", summary.Schema)
	}
	// arrays are [] when empty, never null
	if summary.Entries == nil || summary.SelectedUids == nil || summary.RejectedUids == nil {
		return nil, snHeadSummaryInvalid("an array is null")
	}
	canonical, err := json.Marshal(summary)
	if err != nil {
		return nil, snHeadSummaryInvalid("encode: %s", err)
	}
	if !bytes.Equal(canonical, body) {
		return nil, snHeadSummaryInvalid("body is not the canonical encoding")
	}
	if err := validateSnHeadSummary(summary); err != nil {
		return nil, err
	}
	return summary, nil
}

func validateSnHeadSummary(summary *SnHeadSummary) error {
	if summary.Netuid == 0 || summary.NativeSnapshotBlock == 0 || summary.EvmSnapshotBlock == 0 || summary.PolicyId == 0 || summary.MaximumHeadFleets == 0 {
		return snHeadSummaryInvalid("netuid, a snapshot block, policy_id or maximum_head_fleets is zero")
	}
	for label, value := range map[string]string{"native_snapshot_hash": summary.NativeSnapshotHash, "evm_snapshot_hash": summary.EvmSnapshotHash, "policy_hash": summary.PolicyHash} {
		if _, err := snHeadSummaryHex32(label, value, false); err != nil {
			return err
		}
	}
	if _, err := snHeadSummaryHex32("validator_hotkey", summary.ValidatorHotkey, true); err != nil {
		return err
	}
	if !snHeadSummarySha256.MatchString(summary.MeasurementArtifactHash) || !snHeadSummarySha256.MatchString(summary.SigningHash) {
		return snHeadSummaryInvalid("measurement_artifact_hash or signing_hash is not a sha256 address")
	}
	if summary.SignatureScheme != "sr25519" || !snHeadSummarySignature.MatchString(summary.Signature) {
		return snHeadSummaryInvalid("signature is not a 64-byte sr25519 signature")
	}
	signedAt, err := time.Parse(time.RFC3339Nano, summary.SignedAt)
	if err != nil || signedAt.UTC().Format(time.RFC3339Nano) != summary.SignedAt {
		return snHeadSummaryInvalid("signed_at is not a canonical UTC RFC 3339 time")
	}
	if snHeadSummaryMaxEntries < len(summary.Entries) {
		return snHeadSummaryInvalid("%d entries, at most %d", len(summary.Entries), snHeadSummaryMaxEntries)
	}
	uids := map[uint16]bool{}
	selected, rejected := []uint16{}, []uint16{}
	var priorScore *big.Rat
	for index, entry := range summary.Entries {
		if entry.Rank != uint32(index+1) {
			return snHeadSummaryInvalid("entry %d has rank %d", index, entry.Rank)
		}
		if uids[entry.Uid] {
			return snHeadSummaryInvalid("uid %d repeats", entry.Uid)
		}
		uids[entry.Uid] = true
		if _, err := snHeadSummaryHex32("entry hotkey", entry.Hotkey, true); err != nil {
			return err
		}
		score, err := snHeadSummaryScore(entry.Score)
		if err != nil {
			return err
		}
		// score descending, then uid ascending
		if priorScore != nil {
			order := priorScore.Cmp(score)
			if order < 0 || order == 0 && summary.Entries[index-1].Uid >= entry.Uid {
				return snHeadSummaryInvalid("entry %d is out of rank order", index)
			}
		}
		priorScore = score
		if entry.Selected != (entry.Rank <= uint32(summary.MaximumHeadFleets)) {
			return snHeadSummaryInvalid("entry %d selection differs from its rank", index)
		}
		if entry.Selected {
			selected = append(selected, entry.Uid)
		} else {
			rejected = append(rejected, entry.Uid)
		}
		if len(entry.Fleets) == 0 {
			return snHeadSummaryInvalid("entry %d has no fleet", index)
		}
		var priorFleet []byte
		var priorGeneration uint64
		for fleetIndex, fleet := range entry.Fleets {
			fleetId, err := snHeadSummaryHex32("fleet_id", fleet.FleetId, true)
			if err != nil {
				return err
			}
			if fleet.Generation == 0 {
				return snHeadSummaryInvalid("entry %d fleet %d has generation 0", index, fleetIndex)
			}
			if 0 < fleetIndex {
				order := bytes.Compare(priorFleet, fleetId[:])
				if 0 < order || order == 0 && fleet.Generation <= priorGeneration {
					return snHeadSummaryInvalid("entry %d fleets are not strictly ordered", index)
				}
			}
			priorFleet, priorGeneration = fleetId[:], fleet.Generation
		}
	}
	if !slices.Equal(selected, summary.SelectedUids) || !slices.Equal(rejected, summary.RejectedUids) {
		return snHeadSummaryInvalid("selected_uids or rejected_uids differ from the entries")
	}
	return nil
}

// A score's exact value: reduced, with a positive numerator.
func snHeadSummaryScore(score SnHeadSummaryRational) (*big.Rat, error) {
	if !snHeadSummaryNumerator.MatchString(score.Numerator) || !snHeadSummaryDenominator.MatchString(score.Denominator) {
		return nil, snHeadSummaryInvalid("score is not two decimals")
	}
	numerator, _ := new(big.Int).SetString(score.Numerator, 10)
	denominator, _ := new(big.Int).SetString(score.Denominator, 10)
	if numerator.Sign() <= 0 {
		return nil, snHeadSummaryInvalid("score is not positive")
	}
	if new(big.Int).GCD(nil, nil, numerator, denominator).Cmp(big.NewInt(1)) != 0 {
		return nil, snHeadSummaryInvalid("score is not reduced")
	}
	return new(big.Rat).SetFrac(numerator, denominator), nil
}

// SnHeadSummaryDigest is SHA-256 over the domain, a zero byte and the
// canonical encoding with signing_hash and signature set to "".
func SnHeadSummaryDigest(summary *SnHeadSummary) [32]byte {
	unsigned := *summary
	unsigned.SigningHash, unsigned.Signature = "", ""
	// a struct of strings, integers and slices always encodes
	encoded, _ := json.Marshal(&unsigned)
	digest := sha256.New()
	digest.Write([]byte(snHeadSummaryDomainV1))
	digest.Write([]byte{0})
	digest.Write(encoded)
	return [32]byte(digest.Sum(nil))
}

// VerifySnHeadSummarySignature compares signing_hash with the recomputed
// digest and verifies the sr25519 signature over it in the Substrate signing
// context, under validator_hotkey.
func VerifySnHeadSummarySignature(summary *SnHeadSummary) error {
	digest := SnHeadSummaryDigest(summary)
	if summary.SigningHash != "sha256:"+hex.EncodeToString(digest[:]) {
		return snHeadSummaryInvalid("signing_hash differs from the digest")
	}
	hotkey, err := snHeadSummaryHex32("validator_hotkey", summary.ValidatorHotkey, true)
	if err != nil {
		return err
	}
	var signatureBytes [64]byte
	if _, err := hex.Decode(signatureBytes[:], []byte(summary.Signature[2:])); err != nil {
		return snHeadSummaryInvalid("signature is not hex")
	}
	publicKey := &schnorrkel.PublicKey{}
	if err := publicKey.Decode(hotkey); err != nil {
		return snHeadSummaryInvalid("validator_hotkey is not an sr25519 key")
	}
	signature := &schnorrkel.Signature{}
	if err := signature.Decode(signatureBytes); err != nil {
		return snHeadSummaryInvalid("signature is not an sr25519 signature")
	}
	ok, err := publicKey.Verify(signature, schnorrkel.NewSigningContext([]byte("substrate"), digest[:]))
	if err != nil || !ok {
		return snHeadSummaryInvalid("signature does not verify under validator_hotkey")
	}
	return nil
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
	if snapshot == nil || summary.ValidatorUid != snIncentiveDefaultReferenceUid || snapshot.SubnetEpochIndex+1 < summary.NativeEpoch {
		return false
	}
	holder, ok := snapshot.IncentiveFor(summary.ValidatorUid, hotkey)
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
	if err := VerifySnHeadSummarySignature(summary); err != nil {
		return "", err
	}
	hotkey, _ := snHeadSummaryHex32("validator_hotkey", summary.ValidatorHotkey, true)
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

// The stored form: the envelope's identities, the body, and one row per
// fleet identity with its entry's rank, score and selection.
func snStoredHeadSummary(summary *SnHeadSummary, body []byte, receivedAt time.Time) (*model.SnHeadSummary, error) {
	stored := &model.SnHeadSummary{
		Netuid: summary.Netuid, ValidatorUid: summary.ValidatorUid, NativeEpoch: summary.NativeEpoch,
		NativeSnapshotBlock: summary.NativeSnapshotBlock, EvmSnapshotBlock: summary.EvmSnapshotBlock,
		SettlementEpoch: summary.SettlementEpoch, PolicyId: summary.PolicyId,
		MeasurementArtifactHash: summary.MeasurementArtifactHash, MaximumHeadFleets: summary.MaximumHeadFleets,
		EntryCount: len(summary.Entries), SelectedCount: len(summary.SelectedUids),
		SigningHash: summary.SigningHash, ReceivedAt: receivedAt, Body: append([]byte(nil), body...),
	}
	var err error
	if stored.ValidatorHotkey, err = snHeadSummaryHex32("validator_hotkey", summary.ValidatorHotkey, true); err != nil {
		return nil, err
	}
	if stored.NativeSnapshotHash, err = snHeadSummaryHex32("native_snapshot_hash", summary.NativeSnapshotHash, false); err != nil {
		return nil, err
	}
	if stored.EvmSnapshotHash, err = snHeadSummaryHex32("evm_snapshot_hash", summary.EvmSnapshotHash, false); err != nil {
		return nil, err
	}
	if stored.PolicyHash, err = snHeadSummaryHex32("policy_hash", summary.PolicyHash, false); err != nil {
		return nil, err
	}
	if stored.SignedAt, err = time.Parse(time.RFC3339Nano, summary.SignedAt); err != nil {
		return nil, snHeadSummaryInvalid("signed_at: %s", err)
	}
	for _, entry := range summary.Entries {
		hotkey, err := snHeadSummaryHex32("entry hotkey", entry.Hotkey, true)
		if err != nil {
			return nil, err
		}
		score, err := snHeadSummaryScore(entry.Score)
		if err != nil {
			return nil, err
		}
		value, _ := score.Float64()
		for _, fleet := range entry.Fleets {
			fleetId, err := snHeadSummaryHex32("fleet_id", fleet.FleetId, true)
			if err != nil {
				return nil, err
			}
			stored.Fleets = append(stored.Fleets, &model.SnHeadSummaryFleetScore{
				Key:  model.SnFleetKey{FleetId: fleetId, Hotkey: hotkey, Generation: fleet.Generation, Uid: entry.Uid},
				Rank: int(entry.Rank), ScoreNumerator: entry.Score.Numerator, ScoreDenominator: entry.Score.Denominator,
				Score: value, Selected: entry.Selected,
			})
		}
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
