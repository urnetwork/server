// SIGNALS.md §22.4 real validation paths, attempt completeness and pool
// quality. The reader reads the public /verify/keys, /verify/proofs (with the
// server-signed original transitions) and /verify/stats at both enrolled Api
// origins and verifies every published Final and verifier Extend signature
// with the historical server keys, every original Seed/Extend/Assign/Final
// transition through sn's receipt validator, the stats against the exact
// Wilson rule, and the validators' presence and completion distribution. The
// validators' own measurement replay and the poisoning controls need sources
// the monitor does not have; those classes stay explicitly unknown.
package monitor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

const subnetValidationRunBudget = 120 * time.Second
const maximumSubnetVerifyKeys = 256
const maximumSubnetVerifyKeyBytes = 64 * 1024
const maximumSubnetVerifyPages = 4

// The reserved incident classes of §22.4.
const (
	subnetPathProofInvalidClass    = "subnet-path-proof-invalid"
	subnetVerifyPoisoningClass     = "subnet-verify-poisoning"
	subnetAttemptHistoryGapClass   = "subnet-attempt-history-gap"
	subnetMeasurementCoverageClass = "subnet-measurement-coverage"
	subnetPoolQualityMismatchClass = "subnet-pool-quality-mismatch"
	subnetEgressAttributionClass   = "subnet-egress-attribution"
	subnetMeasurementBiasClass     = "subnet-measurement-bias"
)

var subnetValidationFamily = subnetFamily{
	number: "22.4", key: "subnet-validation", probeId: "subnet/validation",
	title:   "real validation paths, attempt completeness and pool quality",
	classes: []string{subnetPathProofInvalidClass, subnetVerifyPoisoningClass, subnetAttemptHistoryGapClass, subnetMeasurementCoverageClass, subnetPoolQualityMismatchClass, subnetEgressAttributionClass, subnetMeasurementBiasClass},
	sources: "the public /verify/keys, /verify/proofs with original transitions and /verify/stats at both enrolled API origins, the reviewed policy document and validator measurement keys",
}

func NewSubnetValidationSignal() Signal {
	return &signalAdapter{
		number: "22.4",
		key:    "subnet-validation",
		name:   "Subnet validation paths, attempt completeness and pool quality",
		probe:  newSubnetValidationProbe(),
	}
}

// Probe-owned observation state: the published server key history and the
// end of the last published proof window.
type subnetValidationProbe struct {
	keys     *subnetServerKeyHistory
	window   *subnetProofWindow
	runtimes *subnetRuntimeCache
	latch    *subnetReplayLatch
}

func newSubnetValidationProbe() *subnetValidationProbe {
	return &subnetValidationProbe{keys: &subnetServerKeyHistory{keys: map[byte]string{}}, window: &subnetProofWindow{}, runtimes: newSubnetRuntimeCache(), latch: newSubnetReplayLatch()}
}

func (*subnetValidationProbe) id() string               { return "subnet/validation" }
func (*subnetValidationProbe) tier() string             { return tierWarn }
func (*subnetValidationProbe) cadence() time.Duration   { return 5 * time.Minute }
func (*subnetValidationProbe) runBudget() time.Duration { return subnetValidationRunBudget }

// Every server key id ever published maps to one public key forever.
type subnetServerKeyHistory struct {
	stateLock sync.Mutex
	keys      map[byte]string
}

// The reasons a key set rewrites or drops the published history; the
// accepted set is recorded on publish.
func (self *subnetServerKeyHistory) compare(keys map[byte]ed25519.PublicKey) []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	reasons := []string{}
	for id, previous := range self.keys {
		current, ok := keys[id]
		switch {
		case !ok:
			reasons = append(reasons, "server-key-history-dropped")
		case hex.EncodeToString(current) != previous:
			reasons = append(reasons, "server-key-history-rewritten")
		}
	}
	return reasons
}

func (self *subnetServerKeyHistory) remember(keys map[byte]ed25519.PublicKey) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for id, key := range keys {
		self.keys[id] = hex.EncodeToString(key)
	}
}

// The end of the last published proof window.
type subnetProofWindow struct {
	stateLock sync.Mutex
	end       time.Time
}

func (self *subnetProofWindow) last() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.end
}

func (self *subnetProofWindow) advance(end time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if end.After(self.end) {
		self.end = end
	}
}

func (self *subnetValidationProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	return runSubnetFamily(ctx, env, subnetValidationFamily, self.latch, func(ctx context.Context, run *subnetRun) ([]finding, error) {
		return self.observe(ctx, env, run)
	})
}

// Decodes one Json object with no trailing data.
func subnetDecodeJson(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return subnetReadFailure("trailing-json", nil)
	}
	return nil
}

func (self *subnetValidationProbe) observe(ctx context.Context, env *probeEnv, run *subnetRun) ([]finding, error) {
	// The agreed finalized checkpoint dates the published evidence in the
	// §22.10 frame. The evidence is off chain, so it is still decided when
	// the checkpoint is unavailable; the frame then says so.
	if _, err := readSubnetCheckpoint(ctx, run, self.runtimes); err != nil {
		return nil, err
	}
	findings := []finding{
		subnetUnknown(subnetValidationFamily, subnetVerifyPoisoningClass, "subnet-validation/poisoning", "authorized-canary-not-deployed", subnetConfigurationSustain, "a separately authorized bounded /verify canary; passive public reads cannot distinguish poisoned from valid seeds"),
		subnetUnknown(subnetValidationFamily, subnetPoolQualityMismatchClass, "subnet-validation/pool-quality", "validator-decision-unpublished", subnetConfigurationSustain, "each validator's signed release measurement and steering intent, which stay in the validator's private state directory"),
	}
	keys, keyFindings, err := self.observeKeys(ctx, run)
	if err != nil {
		return nil, err
	}
	findings = append(findings, keyFindings...)
	policy := subnetEnrolledPolicy(run.settings)
	now := env.now()
	proofs, proofFindings, err := self.observeProofs(ctx, run, keys, policy, now)
	if err != nil {
		return nil, err
	}
	findings = append(findings, proofFindings...)
	stats, err := observeSubnetVerifyStats(ctx, run, policy, now, proofs)
	if err != nil {
		return nil, err
	}
	return append(findings, stats...), nil
}

// The published server keys must agree at both origins and keep their
// history: an id never maps to another key and never disappears.
func (self *subnetValidationProbe) observeKeys(ctx context.Context, run *subnetRun) (map[byte]ed25519.PublicKey, []finding, error) {
	target := "subnet-validation/keys"
	unknown := func(reason string) []finding {
		return []finding{subnetUnknown(subnetValidationFamily, subnetPathProofInvalidClass, target, reason, subnetTransientSustain, "the published historical server keys at both enrolled origins")}
	}
	sets := []map[byte]ed25519.PublicKey{}
	for i, origin := range run.settings.ApiOrigins {
		raw, _, err := readSubnetApi(ctx, run, origin, "/verify/keys", nil, maximumSubnetVerifyKeyBytes)
		if canceled := subnetCanceled(ctx); canceled != nil {
			return nil, nil, canceled
		}
		if err != nil {
			return nil, unknown(fmt.Sprintf("origin-%d-%s", i+1, subnetReadReason(err))), nil
		}
		var result controller.GetVerifyKeysResult
		if err := subnetDecodeJson(raw, &result); err != nil || len(result.Keys) == 0 || len(result.Keys) > maximumSubnetVerifyKeys {
			return nil, unknown(fmt.Sprintf("origin-%d-keys-invalid", i+1)), nil
		}
		keys := map[byte]ed25519.PublicKey{}
		for _, key := range result.Keys {
			if key == nil || len(key.PublicKey) != ed25519.PublicKeySize {
				return nil, unknown(fmt.Sprintf("origin-%d-keys-invalid", i+1)), nil
			}
			if _, duplicate := keys[key.ServerKeyId]; duplicate {
				return nil, unknown(fmt.Sprintf("origin-%d-keys-duplicate", i+1)), nil
			}
			keys[key.ServerKeyId] = ed25519.PublicKey(key.PublicKey)
		}
		sets = append(sets, keys)
	}
	reasons := []string{}
	for id, key := range sets[0] {
		if other, ok := sets[1][id]; !ok || !bytes.Equal(other, key) {
			reasons = append(reasons, "origins-differ")
		}
	}
	if len(sets[0]) != len(sets[1]) {
		reasons = append(reasons, "origins-differ")
	}
	reasons = append(reasons, self.keys.compare(sets[0])...)
	if len(reasons) != 0 {
		return sets[0], []finding{subnetDecided(subnetValidationFamily, subnetPathProofInvalidClass, target, "", run.scope("complete", "two-api-origins"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "server-key-history-consistent", expected: "identical-keys-at-both-origins-and-stable-key-ids", observed: strings.Join(subnetSortedUnique(reasons), ","),
				symptom:   "The published verify server key history is inconsistent",
				mechanism: "The two enrolled origins publish different server keys, or a key id this process already observed now maps to another key or has disappeared. A rewritten or dropped historical key makes old proofs unverifiable or lets a forged key verify them.",
				baseline:  "Both origins publish identical keys, and every historical key id keeps its original public key forever.",
				action:    "Preserve both origins' key responses; investigate the verify key configuration before accepting new proofs.",
				verify:    "Both origins publish the same complete historical key set on a fresh run.",
			},
			evidence: fmt.Sprintf("%d published key ids. Public keys are compared in memory and not rendered.", len(sets[0])),
		})}, nil
	}
	run.afterPublish(func() { self.keys.remember(sets[0]) })
	return sets[0], []finding{subnetHealthy(subnetValidationFamily, subnetPathProofInvalidClass, target)}, nil
}

// The verified proof census of one window.
type subnetProofCensus struct {
	complete  map[string]int
	expired   map[string]int
	truncated bool
	rows      int
}

// The public row hop shape, decoded strictly.
type subnetPublicHop struct {
	ClientId     connect.Id `json:"client_id"`
	TimeMs       uint64     `json:"time_ms"`
	EgressIpHash *[32]byte  `json:"egress_ip_hash"`
}

var subnetIpv4Literal = regexp.MustCompile(`(^|[^0-9.])(25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1?[0-9]?[0-9])){3}($|[^0-9.])`)

// Every Json string value that could carry a raw address. Keys, base64
// signatures and timestamps never match the dotted quad.
func subnetRawAddressLeak(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		if value, ok := token.(string); ok && (subnetIpv4Literal.MatchString(value) || strings.Count(value, ":") >= 2 && strings.Contains(value, "::")) {
			return true
		}
	}
}

func (self *subnetValidationProbe) observeProofs(ctx context.Context, run *subnetRun, keys map[byte]ed25519.PublicKey, policy *protocol.Policy, now time.Time) (*subnetProofCensus, []finding, error) {
	target := "subnet-validation/proofs"
	from := now.Add(-run.limits.proofWindow)
	if last := self.window.last(); !last.IsZero() && last.After(from) {
		from = last.Add(-time.Minute)
	}
	limit := min(run.limits.maxProofRows, 256)
	census := &subnetProofCensus{complete: map[string]int{}, expired: map[string]int{}}
	unknown := func(reason string) (*subnetProofCensus, []finding, error) {
		return nil, []finding{
			subnetUnknown(subnetValidationFamily, subnetPathProofInvalidClass, target, reason, subnetTransientSustain, "the public proof index and original transitions at both origins"),
			subnetUnknown(subnetValidationFamily, subnetAttemptHistoryGapClass, "subnet-validation/attempts", reason, subnetTransientSustain, "the public original transition census"),
			subnetUnknown(subnetValidationFamily, subnetEgressAttributionClass, "subnet-validation/egress", reason, subnetTransientSustain, "the public proof hops"),
		}, nil
	}
	pageReasons := []string{}
	warnReasons := []string{}
	attemptReasons := []string{}
	egressReasons := []string{}
	seenRows := map[string][]byte{}
	transitions := map[string]map[int]string{}
	reviewed := map[string]bool{}
	for _, validator := range run.settings.Validators {
		reviewed[validator.Vpk] = true
	}
	foreign := 0
	pageFrom := from
	for page := 0; page < maximumSubnetVerifyPages; page++ {
		query := url.Values{"from": {pageFrom.UTC().Format(time.RFC3339Nano)}, "to": {now.UTC().Format(time.RFC3339Nano)}, "limit": {strconv.Itoa(limit)}, "originals": {"true"}}
		raws := [][]byte{}
		for i, origin := range run.settings.ApiOrigins {
			raw, _, err := readSubnetApi(ctx, run, origin, "/verify/proofs", query, run.limits.maxResponseBytes)
			if canceled := subnetCanceled(ctx); canceled != nil {
				return nil, nil, canceled
			}
			if err != nil {
				return unknown(fmt.Sprintf("origin-%d-%s", i+1, subnetReadReason(err)))
			}
			raws = append(raws, raw)
		}
		var result controller.GetVerifyProofsResult
		if err := subnetDecodeJson(raws[0], &result); err != nil || result.Schema != "urnetwork-verify-proof-index-v1" {
			return unknown("proof-index-invalid")
		}
		var mirror controller.GetVerifyProofsResult
		if err := subnetDecodeJson(raws[1], &mirror); err != nil || len(mirror.Rows) != len(result.Rows) {
			warnReasons = append(warnReasons, "origins-differ")
		}
		if subnetRawAddressLeak(raws[0]) || subnetRawAddressLeak(raws[1]) {
			egressReasons = append(egressReasons, "raw-address-in-public-evidence")
		}
		if result.PolicyHash != run.settings.Deployment.PolicyHash {
			warnReasons = append(warnReasons, "verify-policy-differs")
		}
		latest := pageFrom
		for _, row := range result.Rows {
			if row == nil {
				return unknown("proof-row-invalid")
			}
			id := connect.Id(row.TrailId)
			key := hex.EncodeToString(id[:])
			encoded, _ := json.Marshal(row)
			if previous, ok := seenRows[key]; ok {
				if !bytes.Equal(previous, encoded) {
					pageReasons = append(pageReasons, "proof-fork")
				}
				continue
			}
			seenRows[key] = encoded
			census.rows++
			if row.CreateTime.After(latest) {
				latest = row.CreateTime
			}
			vpk := "0x" + hex.EncodeToString(row.Vpk)
			if !reviewed[vpk] {
				foreign++
			}
			reasons, egress := subnetProofRowReasons(row, keys, policy)
			pageReasons = append(pageReasons, reasons...)
			egressReasons = append(egressReasons, egress...)
			switch row.Status {
			case model.VerifyTrailRowStatusComplete:
				census.complete[vpk]++
			case model.VerifyTrailRowStatusExpired:
				census.expired[vpk]++
			}
		}
		for _, transition := range result.OriginalTransitions {
			reason, trail, depth, digest := subnetTransitionReasons(transition, keys, run.settings.Deployment)
			if reason != "" {
				pageReasons = append(pageReasons, reason)
				continue
			}
			if transitions[trail] == nil {
				transitions[trail] = map[int]string{}
			}
			if previous, ok := transitions[trail][depth]; ok && previous != digest {
				attemptReasons = append(attemptReasons, "transition-fork")
			}
			transitions[trail][depth] = digest
		}
		if result.OriginalTruncated || len(result.Rows) >= limit {
			census.truncated = true
		}
		if len(result.Rows) < limit {
			break
		}
		if !latest.After(pageFrom) {
			census.truncated = true
			break
		}
		pageFrom = latest
	}
	findings := []finding{}
	evidence := fmt.Sprintf("Window %d minutes; %d proof rows; %d trails with original transitions; %d rows from validators outside the reviewed census (validators are permissionless).", int(now.Sub(from).Minutes()), census.rows, len(transitions), foreign)
	if len(pageReasons) != 0 {
		findings = append(findings, subnetDecided(subnetValidationFamily, subnetPathProofInvalidClass, target, "", run.scope("proof-window", "two-api-origins-and-historical-server-keys"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "published-proofs-verify", expected: "every-row-and-original-transition-verifies", observed: strings.Join(subnetSortedUnique(pageReasons), ","),
				symptom:   "Published subnet validation evidence does not verify",
				mechanism: "A published completed proof fails its server FINAL or validator EXTEND signature, its depth or hop structure, or forks under one trail id, or an original transition fails the SEED/EXTEND/ASSIGN/FINAL receipt validation or names another deployment. Invalid evidence in the public record can enter scoring or a committed artifact.",
				baseline:  "Every completed proof verifies under its historical server key and its validator's key with the policy depth and unique ordered hops, and every original transition validates under its server key within this deployment's scope.",
				action:    "Preserve the minimal signed counterexample bytes privately and investigate the verify server and key custody before more proofs are scored.",
				verify:    "A fresh window's proofs and transitions all verify.",
			},
			evidence: evidence + " Correctly refused attacks never reach the public index and are not counted here.",
		}))
	}
	if len(warnReasons) != 0 {
		findings = append(findings, subnetDecided(subnetValidationFamily, subnetPathProofInvalidClass, target, "publication", run.scope("proof-window", "two-api-origins"), subnetDecision{
			state: "inconsistent",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetTransientSustain,
				invariant: "origins-publish-one-window", expected: "identical-windows-and-the-sn-yml-policy", observed: strings.Join(subnetSortedUnique(warnReasons), ","),
				symptom:   "The two verify origins or the verify policy disagree",
				mechanism: "The enrolled origins return different proof windows, or the verify server reports a policy other than the deployment's reviewed policy. Two names can share one store, so agreement would not prove independence; disagreement shows one is stale or misconfigured.",
				baseline:  "Both origins return the same window under the reviewed policy hash.",
				action:    "Compare both origins' verify configuration and replication privately.",
				verify:    "Both origins agree under the reviewed policy on a fresh run.",
			},
			evidence: evidence,
		}))
	}
	if len(pageReasons) == 0 && len(warnReasons) == 0 {
		findings = append(findings, subnetHealthy(subnetValidationFamily, subnetPathProofInvalidClass, target))
	}
	attemptTarget := "subnet-validation/attempts"
	switch {
	case len(attemptReasons) != 0:
		findings = append(findings, subnetDecided(subnetValidationFamily, subnetAttemptHistoryGapClass, attemptTarget, "", run.scope("proof-window", "two-api-origins-and-historical-server-keys"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "one-receipt-per-trail-depth", expected: "one-valid-receipt", observed: strings.Join(subnetSortedUnique(attemptReasons), ","),
				symptom:   "A published attempt history forks",
				mechanism: "Two valid original transitions claim different outcomes at the same depth of one trail. An attempt has one ordered durable sequence; a fork double-applies or fabricates history.",
				baseline:  "Each trail's transitions form one ordered sequence with one outcome per depth.",
				action:    "Recover the trail's retained journal evidence; never reset statistics to zero to clear it.",
				verify:    "A fresh window has one transition per trail depth.",
			},
			evidence: evidence,
		}))
	case census.truncated:
		findings = append(findings, subnetUnknown(subnetValidationFamily, subnetAttemptHistoryGapClass, attemptTarget, "transition-census-truncated", subnetTransientSustain, "a complete original transition census for the window"))
	default:
		findings = append(findings, subnetUnknown(subnetValidationFamily, subnetAttemptHistoryGapClass, attemptTarget, "validator-cuts-unpublished", subnetConfigurationSustain, "each validator's signed attempt cuts and closures, which prove completeness beyond the operator's public index"))
	}
	egressTarget := "subnet-validation/egress"
	if len(egressReasons) != 0 {
		findings = append(findings, subnetDecided(subnetValidationFamily, subnetEgressAttributionClass, egressTarget, "", run.scope("proof-window", "two-api-origins"), subnetDecision{
			state: "violated",
			issue: subnetReplayIssue{
				tier: tierPage, sustain: subnetProvedSustain,
				invariant: "no-raw-egress-in-public-evidence", expected: "keyed-egress-hashes-only", observed: strings.Join(subnetSortedUnique(egressReasons), ","),
				symptom:   "Public validation evidence leaks or omits egress attribution",
				mechanism: "A public proof carries a raw address or an unexpected hop field, or a completed hop has no keyed egress hash. Raw egress addresses must never be public; a missing hash cannot feed head breadth.",
				baseline:  "Every completed public hop has exactly its client id, server time and a nonzero keyed egress hash, and no public string is a raw address.",
				action:    "Restrict the affected evidence and remove the leaking field from publication; investigate source attribution before blaming providers.",
				verify:    "A fresh window's public evidence has only keyed egress hashes.",
			},
			evidence: evidence + " The leaked value itself is never copied into this alert.",
		}))
	} else {
		findings = append(findings, subnetUnknown(subnetValidationFamily, subnetEgressAttributionClass, egressTarget, "source-attribution-private", subnetConfigurationSustain, "the operator's private source-IP to provider mapping, which decides whether a credited address was ambiguous, spoofed or stale"))
	}
	if !census.truncated {
		run.afterPublish(func() { self.window.advance(now) })
	}
	return census, findings, nil
}

// The reasons one published proof row fails, and its egress reasons.
func subnetProofRowReasons(row *controller.PublicVerifyProofRow, keys map[byte]ed25519.PublicKey, policy *protocol.Policy) ([]string, []string) {
	reasons := []string{}
	egress := []string{}
	if len(row.Vpk) != ed25519.PublicKeySize || len(row.ServerNonce) != connect.VerifyNonceSize || row.Depth < 1 || row.Depth > connect.VerifyMMax {
		return []string{"proof-shape"}, nil
	}
	// An expired row's depth is its confirmed hop count; it carries no
	// signatures.
	if row.Status != model.VerifyTrailRowStatusComplete {
		var hops []subnetPublicHop
		if row.Status != model.VerifyTrailRowStatusExpired || len(row.FinalSig) != 0 || len(row.VerifierSig) != 0 || json.Unmarshal(row.Hops, &hops) != nil || len(hops) != row.Depth {
			reasons = append(reasons, "proof-status")
		}
		return reasons, nil
	}
	if row.Depth < connect.VerifyMMin {
		return []string{"proof-shape"}, nil
	}
	if policy != nil && row.Depth != policy.Verify.TrailDepth {
		reasons = append(reasons, "proof-depth-differs-from-policy")
	}
	var fields []map[string]json.RawMessage
	var hops []subnetPublicHop
	if json.Unmarshal(row.Hops, &fields) != nil || json.Unmarshal(row.Hops, &hops) != nil || len(hops) != row.Depth {
		return append(reasons, "proof-hops-invalid"), nil
	}
	ids := make([]connect.Id, len(hops))
	proofHops := make([]connect.VerifyProofHop, len(hops))
	seen := map[connect.Id]bool{}
	for i, hop := range hops {
		if len(fields[i]) != 3 {
			egress = append(egress, "unexpected-public-hop-field")
		}
		if hop.ClientId == (connect.Id{}) || seen[hop.ClientId] || hop.TimeMs == 0 || i > 0 && hop.TimeMs < hops[i-1].TimeMs {
			reasons = append(reasons, "proof-hop-order")
		}
		if hop.EgressIpHash == nil || *hop.EgressIpHash == ([32]byte{}) {
			egress = append(egress, "egress-hash-missing")
		}
		seen[hop.ClientId] = true
		ids[i] = hop.ClientId
		proofHops[i] = connect.VerifyProofHop{ClientId: hop.ClientId, TimeMs: hop.TimeMs}
		if hop.EgressIpHash != nil {
			proofHops[i].EgressIpHash = *hop.EgressIpHash
		}
	}
	key, ok := keys[row.ServerKeyId]
	if !ok {
		return append(reasons, "proof-key-unpublished"), egress
	}
	final, err := connect.BuildVerifyFinalMessage(row.ServerKeyId, connect.Id(row.TrailId), row.ServerNonce, row.Vpk, byte(row.Depth), proofHops)
	if err != nil || !connect.VerifyVerifyMessageSignature(key, final, row.FinalSig) {
		reasons = append(reasons, "final-signature")
	}
	extend, err := connect.BuildVerifyExtendMessage(connect.Id(row.TrailId), row.ServerNonce, row.Vpk, byte(row.Depth), ids)
	if err != nil || !connect.VerifyVerifyMessageSignature(row.Vpk, extend, row.VerifierSig) {
		reasons = append(reasons, "verifier-signature")
	}
	return reasons, egress
}

// Validates one original transition under its server key and the deployment
// scope; returns the trail, depth and body digest of a valid one, so two
// different valid receipts at one trail depth are a fork whatever status
// they claim.
func subnetTransitionReasons(transition *model.VerifyOriginalTransition, keys map[byte]ed25519.PublicKey, deployment SubnetDeploymentIdentity) (string, string, int, string) {
	if transition == nil {
		return "transition-invalid", "", 0, ""
	}
	receipt := &protocol.ProviderAttemptReceipt{Body: transition.Body, Signature: transition.Signature}
	body, err := protocol.DecodeProviderAttemptReceipt(receipt)
	if err != nil {
		return "transition-invalid", "", 0, ""
	}
	key, ok := keys[body.Trail.ServerKeyId]
	if !ok {
		return "transition-key-unpublished", "", 0, ""
	}
	if _, err := protocol.ValidateProviderAttemptReceipt(receipt, key); err != nil {
		return "transition-invalid", "", 0, ""
	}
	if scope := body.Scope; scope != nil {
		if subnetHex32(scope.GenesisHash) != deployment.GenesisHash || scope.DeploymentId != deployment.DeploymentId || subnetHex32(scope.PolicyHash) != deployment.PolicyHash || scope.Netuid != uint64(deployment.Netuid) {
			return "transition-scope", "", 0, ""
		}
	}
	trail := hex.EncodeToString(body.Trail.TrailId[:])
	digest := sha256.Sum256(transition.Body)
	return "", trail, body.PreviousDepth, hex.EncodeToString(digest[:])
}

// Stats freshness, the exact Wilson replay, eligible coverage, every reviewed
// validator's presence, terminal lag and the completion distribution.
func observeSubnetVerifyStats(ctx context.Context, run *subnetRun, policy *protocol.Policy, now time.Time, proofs *subnetProofCensus) ([]finding, error) {
	coverageTarget := "subnet-validation/coverage"
	biasTarget := "subnet-validation/bias"
	if policy == nil {
		return []finding{
			subnetUnknown(subnetValidationFamily, subnetMeasurementCoverageClass, coverageTarget, "policy-document-not-enrolled", subnetConfigurationSustain, "the reviewed policy's a_min, trail depth and egress key"),
			subnetUnknown(subnetValidationFamily, subnetMeasurementBiasClass, biasTarget, "policy-document-not-enrolled", subnetConfigurationSustain, "the reviewed policy"),
		}, nil
	}
	query := url.Values{"from": {now.Add(-2 * run.limits.statsMaxAge).UTC().Format(time.RFC3339Nano)}, "to": {now.UTC().Format(time.RFC3339Nano)}, "limit": {"10000"}}
	raw, _, err := readSubnetApi(ctx, run, run.settings.ApiOrigins[0], "/verify/stats", query, run.limits.maxResponseBytes)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return nil, canceled
	}
	var stats controller.GetVerifyStatsResult
	if err == nil {
		err = subnetDecodeJson(raw, &stats)
	}
	if err != nil || stats.Schema != "urnetwork-verify-stats-index-v1" {
		return []finding{subnetUnknown(subnetValidationFamily, subnetMeasurementCoverageClass, coverageTarget, "stats-"+subnetReadReason(err), subnetTransientSustain, "the public verify statistics")}, nil
	}
	reasons := []string{}
	aMin := policy.Verify.ReliabilityAMin
	var newest time.Time
	latestPeriod := time.Time{}
	for _, row := range stats.Rows {
		if row == nil || row.Confirmations > row.Assignments || !row.PeriodStart.Before(row.PeriodEnd) {
			reasons = append(reasons, "stats-row-invalid")
			continue
		}
		if row.ReliabilityPPM != protocol.ReliabilityPPM(row.Confirmations, row.Assignments, aMin) {
			reasons = append(reasons, "stats-reliability-differs")
		}
		if row.PeriodEnd.After(newest) {
			newest = row.PeriodEnd
		}
		if row.PeriodStart.After(latestPeriod) {
			latestPeriod = row.PeriodStart
		}
	}
	if newest.IsZero() || now.Sub(newest) > run.limits.statsMaxAge {
		reasons = append(reasons, "stats-stale")
	}
	if stats.EgressHashKeyId != policy.Verify.EgressHashKeyID {
		reasons = append(reasons, "egress-key-differs")
	}
	if stats.PolicyHash != run.settings.Deployment.PolicyHash {
		reasons = append(reasons, "stats-policy-differs")
	}
	active, eligible := 0, 0
	for _, row := range stats.Rows {
		if row != nil && row.PeriodStart.Equal(latestPeriod) && row.Assignments > 0 {
			active++
			if row.Assignments >= aMin {
				eligible++
			}
		}
	}
	if active != 0 && uint64(eligible)*100 < uint64(active)*run.limits.coverageMinEligiblePercent {
		reasons = append(reasons, "eligible-coverage-low")
	}
	if proofs != nil && !proofs.truncated {
		for i, validator := range run.settings.Validators {
			if proofs.complete[validator.Vpk]+proofs.expired[validator.Vpk] == 0 {
				reasons = append(reasons, fmt.Sprintf("validator-%d-absent", i+1))
			}
		}
	}
	findings := []finding{}
	if len(reasons) != 0 {
		findings = append(findings, subnetDecided(subnetValidationFamily, subnetMeasurementCoverageClass, coverageTarget, "", run.scope("complete", "two-api-origins-and-the-wilson-rule"), subnetDecision{
			state: "degraded",
			issue: subnetReplayIssue{
				tier: tierWarn, sustain: subnetTransientSustain,
				invariant: "measurement-coverage", expected: "fresh-exact-statistics-and-every-reviewed-validator-present", observed: strings.Join(subnetSortedUnique(reasons), ","),
				symptom:   "Subnet measurement coverage is below the reviewed policy",
				mechanism: "The public verify statistics are stale, do not reproduce the exact Wilson reliability rule, report another policy or egress key, show too few providers at a_min, or a reviewed validator published no trail in the window. Required scoring may not finish by its deadline.",
				baseline:  "Fresh statistics reproduce the Wilson rule under the reviewed policy, most active providers reach a_min, and every reviewed validator publishes trails.",
				action:    "Check the verify statistics job, the operator's transport and each absent validator's trail loop.",
				verify:    "Fresh statistics and every reviewed validator's trails appear on later runs.",
			},
			evidence: fmt.Sprintf("%d statistics rows; latest period %d of %d active providers at a_min. Separate provider failure from validator abandonment, shared-region outage or operator transport failure before attribution.", len(stats.Rows), eligible, active),
		}))
	} else {
		findings = append(findings, subnetHealthy(subnetValidationFamily, subnetMeasurementCoverageClass, coverageTarget))
	}
	findings = append(findings, subnetCompletionBias(run, proofs)...)
	return findings, nil
}

// Each validator's completion share against the pooled share, with a
// two-proportion z test over sample sizes the policy bounds. A statistical
// outlier warns; it never proves collusion.
func subnetCompletionBias(run *subnetRun, proofs *subnetProofCensus) []finding {
	target := "subnet-validation/bias"
	if proofs == nil || proofs.truncated {
		return []finding{subnetUnknown(subnetValidationFamily, subnetMeasurementBiasClass, target, "proof-census-incomplete", subnetTransientSustain, "a complete proof census for the window")}
	}
	totalComplete, total := 0, 0
	vpks := []string{}
	for vpk := range proofs.complete {
		vpks = append(vpks, vpk)
	}
	for vpk := range proofs.expired {
		if proofs.complete[vpk] == 0 {
			vpks = append(vpks, vpk)
		}
	}
	sort.Strings(vpks)
	for _, vpk := range vpks {
		totalComplete += proofs.complete[vpk]
		total += proofs.complete[vpk] + proofs.expired[vpk]
	}
	outliers := 0
	sampled := 0
	if total != 0 {
		pooled := float64(totalComplete) / float64(total)
		threshold := float64(run.limits.biasZScoreTenths) / 10
		for _, vpk := range vpks {
			n := proofs.complete[vpk] + proofs.expired[vpk]
			if uint64(n) < run.limits.biasMinTrails || n == total {
				continue
			}
			sampled++
			others := float64(totalComplete-proofs.complete[vpk]) / float64(total-n)
			share := float64(proofs.complete[vpk]) / float64(n)
			deviation := math.Sqrt(pooled * (1 - pooled) * (1/float64(n) + 1/float64(total-n)))
			if deviation == 0 {
				if share != others {
					outliers++
				}
				continue
			}
			if math.Abs(share-others)/deviation >= threshold {
				outliers++
			}
		}
	}
	if outliers == 0 {
		return []finding{subnetHealthy(subnetValidationFamily, subnetMeasurementBiasClass, target)}
	}
	return []finding{subnetDecided(subnetValidationFamily, subnetMeasurementBiasClass, target, "", run.scope("proof-window", "published-trails"), subnetDecision{
		state: "outlier",
		issue: subnetReplayIssue{
			tier: tierWarn, sustain: subnetTransientSustain,
			invariant: "validator-completion-within-threshold", expected: "no-outlier", observed: fmt.Sprintf("%d-outliers", outliers),
			symptom:   "A validator's trail completion is a statistical outlier",
			mechanism: "A validator with enough trails completes them at a rate the others do not, beyond the reviewed z threshold. Adversarial abandonment, too-perfect single-validator clusters and selective /verify service produce this shape; so can honest regional differences.",
			baseline:  "Every sufficiently sampled validator's completion rate is within the reviewed z threshold of the others'.",
			action:    "Investigate the outlier's cohorts with controls before any attribution; do not label it collusion from this correlation.",
			verify:    "The validator's completion rate returns within the threshold, or an investigation records the honest cause.",
		},
		evidence: "Validator keys stay private; the counts are the only published values. Correlation does not prove collusion or slashing, and proof-of-routing is outside v1.",
		details:  fmt.Sprintf("outliers=%d sampled_validators=%d trails=%d", outliers, sampled, total),
	})}
}
