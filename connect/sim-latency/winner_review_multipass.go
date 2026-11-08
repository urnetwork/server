// Multi-pass review is a distinct, prospectively authorized evidence protocol.
// Complete byte coverage never grants approval: scoped observations and both
// final native judgments must survive independent provenance validation.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Version changes are protocol changes, not revisions of historical receipts.
const winnerMultipassVersion = "winner-review-complete-coverage/1"

// Resource limits belong to the explicitly approved review owner. Defaults are
// ceilings for preparation, never authority to start an inference or extend time.
type winnerMultipassSettings struct {
	TokenizationProfiles    map[string]winnerMultipassTokenizationProfile `json:"tokenization_profiles"`     // Explicit runtime proofs, keyed by member; empty permits offline layout only.
	Custody                 winnerMultipassCustodySettings                `json:"custody"`                   // Explicit independent qualification custody phases; both are pinned by the prospective plan.
	MaximumArtifactBytes    int                                           `json:"maximum_artifact_bytes"`    // Positive per-file bytes; default 2 MiB.
	MaximumBundleBytes      int                                           `json:"maximum_bundle_bytes"`      // Positive aggregate artifact bytes; default 32 MiB.
	MaximumArtifacts        int                                           `json:"maximum_artifacts"`         // Positive artifact census; default 256.
	SpanBytes               int                                           `json:"span_bytes"`                // Positive target, not token admission; default 64 KiB.
	OverlapLines            int                                           `json:"overlap_lines"`             // Positive context lines on either side; default 24.
	MaximumOverlapBytes     int                                           `json:"maximum_overlap_bytes"`     // Positive overlap byte cap; default 8 KiB.
	MaximumCoverageAtoms    int                                           `json:"maximum_coverage_atoms"`    // Positive byte-coverage census, not model calls; default 1024.
	MaximumLeafUnits        int                                           `json:"maximum_leaf_units"`        // Positive actual scan-call census per member; default 128.
	MergeFanIn              int                                           `json:"merge_fan_in"`              // At least two; default four children, no selective pruning.
	MaximumCalls            int                                           `json:"maximum_calls"`             // Positive combined two-member call census; default 384.
	MaximumRequestBytes     int                                           `json:"maximum_request_bytes"`     // Positive per-call HTTP bytes; default 4 MiB.
	MaximumResponseBytes    int                                           `json:"maximum_response_bytes"`    // Positive raw response bytes; default 16 MiB.
	MaximumObservationBytes int                                           `json:"maximum_observation_bytes"` // Positive structured output bytes; default 32 KiB.
	MaximumSummaryBytes     int                                           `json:"maximum_summary_bytes"`     // Positive summary bytes; default 4096.
	MaximumIssues           int                                           `json:"maximum_issues"`            // Positive new issues per call; default 32, overflow holds.
	MaximumIssueBytes       int                                           `json:"maximum_issue_bytes"`       // Positive explanation bytes; default 4096.
	MaximumOutputTokens     int                                           `json:"maximum_output_tokens"`     // Positive fixed allowance; default 32768.
	PackingTokenReserve     int                                           `json:"packing_token_reserve"`     // Positive prospective identity/render variance reserve; default 1024 tokens.
	MaximumOwnerDuration    time.Duration                                 `json:"maximum_owner_duration"`    // Positive prospective ceiling; default 24 hours.
}

// Protection must finish the current original even after provider cancellation.
// Subsequent replay is a separate read-only phase and never renews the owner.
type winnerMultipassCustodySettings struct {
	ProtectionTimeout       time.Duration `json:"protection_timeout"`         // Positive protection/readback/retirement allowance; default120 seconds. Non-positive refuses preparation.
	PostCommitReplayTimeout time.Duration `json:"post_commit_replay_timeout"` // Positive fresh exact-version replay allowance; default120 seconds. Non-positive refuses preparation.
}

// These are independent phase ceilings, not extensions of any model/owner clock.
func defaultWinnerMultipassCustodySettings() winnerMultipassCustodySettings {
	return winnerMultipassCustodySettings{ProtectionTimeout: 120 * time.Second, PostCommitReplayTimeout: 120 * time.Second}
}

// Original output allowance is preserved; all new aggregate bounds are explicit.
func defaultWinnerMultipassSettings() winnerMultipassSettings {
	return winnerMultipassSettings{TokenizationProfiles: map[string]winnerMultipassTokenizationProfile{}, Custody: defaultWinnerMultipassCustodySettings(), MaximumArtifactBytes: 2 * 1024 * 1024, MaximumBundleBytes: 32 * 1024 * 1024, MaximumArtifacts: 256,
		SpanBytes: 64 * 1024, OverlapLines: 24, MaximumOverlapBytes: 8 * 1024, MaximumCoverageAtoms: 1024, MaximumLeafUnits: 128, MergeFanIn: 4, MaximumCalls: 384,
		MaximumRequestBytes: 4 * 1024 * 1024, MaximumResponseBytes: 16 * 1024 * 1024, MaximumObservationBytes: 32 * 1024,
		MaximumSummaryBytes: 4096, MaximumIssues: 32, MaximumIssueBytes: 4096, MaximumOutputTokens: 32768, PackingTokenReserve: 1024, MaximumOwnerDuration: 24 * time.Hour}
}

// This clock is independent of the frozen submission deadline and historical
// one-call review checkpoint. Resumption uses these exact same timestamps.
type winnerMultipassOwner struct {
	Id        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	Deadline  time.Time `json:"deadline"`
}

// The trusted parent reads supplemental blobs from the authenticated frozen map.
// Broker SHA checks ensure consistency, not independent Git provenance.
type winnerMultipassSourceFile struct {
	Path    string `json:"path"`
	Commit  string `json:"commit"`
	Sha256  string `json:"sha256"`
	Content string `json:"content"`
}

// Touched and instrument censuses remain exact and separate. An independently
// reviewed required dependency census is mandatory, even when explicitly empty.
type winnerMultipassBundle struct {
	Request                   winnerReviewBrokerRequest   `json:"request"`
	SupplementalFiles         []winnerMultipassSourceFile `json:"supplemental_files"`
	RequiredSupplementalPaths []string                    `json:"required_supplemental_paths"`
	UnresolvedContext         []string                    `json:"unresolved_context"`
}

// Every artifact retains complete bytes, including absent-at-base declarations.
// Offsets in the plan refer to these bytes, never to an excerpt presented as a file.
type winnerMultipassArtifact struct {
	Id               string `json:"id"`
	Kind             string `json:"kind"`
	Path             string `json:"path"`
	Commit           string `json:"commit"`
	Sha256           string `json:"sha256"`
	Content          string `json:"content"`
	Absent           bool   `json:"absent"`
	DerivationSha256 string `json:"derivation_sha256"`
}

// The immutable ledger omits source bytes but commits to every complete artifact.
type winnerMultipassArtifactIdentity struct {
	Id               string `json:"id"`
	Kind             string `json:"kind"`
	Path             string `json:"path"`
	Commit           string `json:"commit"`
	Sha256           string `json:"sha256"`
	Bytes            int    `json:"bytes"`
	Absent           bool   `json:"absent"`
	DerivationSha256 string `json:"derivation_sha256"`
}

// Core spans partition every artifact exactly. Context spans may overlap but
// never conceal a gap; half-open byte offsets are authenticated independently.
type winnerMultipassSpan struct {
	ArtifactId       string `json:"artifact_id"`
	Start            int    `json:"start"`
	End              int    `json:"end"`
	ContextStart     int    `json:"context_start"`
	ContextEnd       int    `json:"context_end"`
	Sha256           string `json:"sha256"`
	ContextSha256    string `json:"context_sha256"`
	SplitDeclaration bool   `json:"split_declaration"`
}

// Units are common to both members; a reviewer cannot select easier coverage.
type winnerMultipassUnit struct {
	Id    string                `json:"id"`
	Spans []winnerMultipassSpan `json:"spans"`
}

// Ordered calls commit to all predecessors before inference. Synthesis payloads
// are mechanically derived from their original outputs, never operator-selected.
type winnerMultipassCall struct {
	Id      string           `json:"id"`
	Member  winnerSnowMember `json:"member"`
	Stage   string           `json:"stage"`
	UnitIds []string         `json:"unit_ids"`
	Parents []string         `json:"parents"`
}

// Contiguous coverage atoms may be packed differently for each tokenizer before
// the owner plan freezes. Every atom appears exactly once in each member's scans.
type winnerMultipassScanGroup struct {
	Member  string   `json:"member"`
	UnitIds []string `json:"unit_ids"`
}

// The plan is a self-contained coverage ledger, not a reviewer qualification.
type winnerMultipassPlan struct {
	Version      string                            `json:"version"`
	Owner        winnerMultipassOwner              `json:"owner"`
	Settings     winnerMultipassSettings           `json:"settings"`
	Provider     winnerSnowProviderSettings        `json:"provider"`
	BundleSha256 string                            `json:"bundle_sha256"`
	Artifacts    []winnerMultipassArtifactIdentity `json:"artifacts"`
	Units        []winnerMultipassUnit             `json:"units"`
	ScanGroups   []winnerMultipassScanGroup        `json:"scan_groups"`
	Calls        []winnerMultipassCall             `json:"calls"`
}

// Hash exact serialized bytes; callers persist those same bytes, not reprints.
func winnerMultipassSha256(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

// Settings cannot disable a guard or quietly reduce the original output budget.
func validateWinnerMultipassSettings(value winnerMultipassSettings) error {
	custodyCeiling := defaultWinnerMultipassCustodySettings()
	if value.Custody.ProtectionTimeout <= 0 || value.Custody.ProtectionTimeout > custodyCeiling.ProtectionTimeout || value.Custody.PostCommitReplayTimeout <= 0 || value.Custody.PostCommitReplayTimeout > custodyCeiling.PostCommitReplayTimeout {
		return errors.New("multi-pass custody phases lack positive prospectively bounded independent deadlines")
	}
	if value.MaximumArtifactBytes <= 0 || value.MaximumBundleBytes < value.MaximumArtifactBytes || value.MaximumArtifacts <= 0 ||
		value.SpanBytes <= 0 || value.SpanBytes > value.MaximumArtifactBytes || value.OverlapLines <= 0 || value.MaximumOverlapBytes <= 0 ||
		value.MaximumCoverageAtoms <= 0 || value.MaximumLeafUnits <= 0 || value.MergeFanIn < 2 || value.MaximumCalls <= 0 || value.MaximumRequestBytes <= 0 ||
		value.MaximumResponseBytes <= 0 || value.MaximumObservationBytes <= 0 || value.MaximumSummaryBytes <= 0 || value.MaximumIssues <= 0 ||
		value.MaximumIssueBytes <= 0 || value.MaximumOutputTokens != defaultWinnerSnowModelSettings().MaximumOutputTokens || value.PackingTokenReserve <= 0 || value.MaximumOwnerDuration <= 0 {
		return errors.New("multi-pass settings disable a guard or change the original output allowance")
	}
	ceiling := defaultWinnerMultipassSettings()
	if value.MaximumArtifactBytes > ceiling.MaximumArtifactBytes || value.MaximumBundleBytes > ceiling.MaximumBundleBytes || value.MaximumArtifacts > ceiling.MaximumArtifacts ||
		value.MaximumCoverageAtoms > ceiling.MaximumCoverageAtoms || value.MaximumLeafUnits > ceiling.MaximumLeafUnits || value.MergeFanIn > ceiling.MergeFanIn || value.MaximumCalls > ceiling.MaximumCalls ||
		value.MaximumRequestBytes > ceiling.MaximumRequestBytes || value.MaximumResponseBytes > ceiling.MaximumResponseBytes || value.MaximumObservationBytes > ceiling.MaximumObservationBytes ||
		value.MaximumSummaryBytes > ceiling.MaximumSummaryBytes || value.MaximumIssues > ceiling.MaximumIssues || value.MaximumIssueBytes > ceiling.MaximumIssueBytes || value.PackingTokenReserve > ceiling.PackingTokenReserve || value.MaximumOwnerDuration > ceiling.MaximumOwnerDuration {
		return errors.New("multi-pass settings exceed the prospectively approved protocol ceilings")
	}
	return nil
}

// Authenticate the complete existing input first, then derive artifacts. No
// source census, addendum, score projection or frozen identity is weakened.
func winnerMultipassArtifacts(bundle winnerMultipassBundle, settings winnerMultipassSettings) ([]winnerMultipassArtifact, error) {
	if err := validateWinnerMultipassSettings(settings); err != nil {
		return nil, err
	}
	if bundle.RequiredSupplementalPaths == nil || bundle.SupplementalFiles == nil || bundle.UnresolvedContext == nil {
		return nil, errors.New("multi-pass context census must be explicit")
	}
	if _, err := winnerModelInput(bundle.Request); err != nil {
		return nil, err
	}
	request := bundle.Request
	artifacts := []winnerMultipassArtifact{}
	knownPaths := map[string]bool{}
	for _, file := range request.SourceFiles {
		commit := request.Packet.BaseSha
		if request.Packet.PatchPolicy.PathBase != "" {
			repository, _, _ := strings.Cut(file.Path, "/")
			commit = request.Packet.SourceCommits[repository]
		}
		artifacts = append(artifacts, winnerMultipassArtifact{Id: "base:" + file.Path, Kind: "touched_base", Path: file.Path, Commit: commit,
			Sha256: file.Sha256, Content: file.Content, Absent: file.AbsentAtBase})
		knownPaths[file.Path] = true
	}
	for _, file := range request.WorkloadReview.InstrumentFiles {
		artifacts = append(artifacts, winnerMultipassArtifact{Id: "instrument:" + file.Path, Kind: "instrument", Path: file.Path,
			Commit: request.WorkloadReview.ServerCommit, Sha256: file.Sha256, Content: file.Content, Absent: file.AbsentAtBase})
		knownPaths[file.Path] = true
	}
	requiredPaths := append([]string{}, bundle.RequiredSupplementalPaths...)
	sort.Strings(requiredPaths)
	if len(slices.Compact(append([]string{}, requiredPaths...))) != len(requiredPaths) || len(requiredPaths) != len(bundle.SupplementalFiles) {
		return nil, errors.New("supplemental source census is duplicated or incomplete")
	}
	for _, file := range bundle.SupplementalFiles {
		repository, _, found := strings.Cut(file.Path, "/")
		if !found || knownPaths[file.Path] || !slices.Contains(requiredPaths, file.Path) || request.Packet.SourceCommits[repository] != file.Commit {
			return nil, errors.New("supplemental source is outside the explicit frozen dependency census")
		}
		knownPaths[file.Path] = true
		artifacts = append(artifacts, winnerMultipassArtifact{Id: "dependency:" + file.Path, Kind: "dependency", Path: file.Path,
			Commit: file.Commit, Sha256: file.Sha256, Content: file.Content})
	}
	artifacts = append(artifacts, winnerMultipassArtifact{Id: "patch", Kind: "patch", Sha256: winnerMultipassSha256(request.Packet.Patch), Content: string(request.Packet.Patch)},
		winnerMultipassArtifact{Id: "score", Kind: "score", Sha256: winnerMultipassSha256(request.Packet.Score), Content: string(request.Packet.Score)})
	candidates, err := deriveWinnerMultipassCandidates(request, artifacts)
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, candidates...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Id < artifacts[j].Id })
	return artifacts, validateWinnerMultipassArtifacts(artifacts, settings)
}

// This validates byte identities and bounds only. Git/packet authenticity is
// established by the trusted parent and winnerModelInput before this function.
func validateWinnerMultipassArtifacts(artifacts []winnerMultipassArtifact, settings winnerMultipassSettings) error {
	if len(artifacts) == 0 || len(artifacts) > settings.MaximumArtifacts {
		return errors.New("artifact census is empty or oversized")
	}
	total := 0
	ids := map[string]bool{}
	for _, artifact := range artifacts {
		if artifact.Id == "" || ids[artifact.Id] || !utf8.ValidString(artifact.Content) || len(artifact.Content) > settings.MaximumArtifactBytes ||
			artifact.Sha256 != winnerMultipassSha256([]byte(artifact.Content)) || artifact.Absent && artifact.Content != "" {
			return errors.New("artifact bytes are malformed, duplicated, unbound or oversized")
		}
		ids[artifact.Id] = true
		if artifact.Kind != "patch" && artifact.Kind != "score" {
			if artifact.Path == "" || path.Clean(artifact.Path) != artifact.Path || strings.HasPrefix(artifact.Path, "/") || strings.HasPrefix(artifact.Path, "../") ||
				!sourceGitShaPattern.MatchString(artifact.Commit) {
				return errors.New("artifact lacks canonical frozen source identity")
			}
		}
		if len(artifact.Content) > settings.MaximumBundleBytes-total {
			return errors.New("complete artifact bundle exceeds aggregate bound")
		}
		total += len(artifact.Content)
	}
	return nil
}

// Preparation is pure and deterministic; only the caller can establish a durable
// owner, qualify a release, authorize calls or submit a later operator decision.
func prepareWinnerMultipass(bundle winnerMultipassBundle, owner winnerMultipassOwner, settings winnerMultipassSettings, provider winnerSnowProviderSettings) (*winnerMultipassPlan, error) {
	if err := validateWinnerSnowProviderSettings(provider); err != nil {
		return nil, err
	}
	artifacts, err := winnerMultipassArtifacts(bundle, settings)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(owner.Id) == "" || owner.StartedAt.IsZero() || !owner.Deadline.After(owner.StartedAt) || owner.Deadline.Sub(owner.StartedAt) > settings.MaximumOwnerDuration {
		return nil, errors.New("multi-pass owner needs one immutable prospective bounded clock")
	}
	content, err := json.Marshal(bundle)
	if err != nil {
		return nil, err
	}
	plan := &winnerMultipassPlan{Version: winnerMultipassVersion, Owner: owner, Settings: settings, Provider: provider,
		BundleSha256: winnerMultipassSha256(content), Artifacts: []winnerMultipassArtifactIdentity{}, Units: []winnerMultipassUnit{}, Calls: []winnerMultipassCall{}}
	unitBytes := 0
	for _, artifact := range artifacts {
		plan.Artifacts = append(plan.Artifacts, winnerMultipassArtifactIdentity{Id: artifact.Id, Kind: artifact.Kind, Path: artifact.Path, Commit: artifact.Commit,
			Sha256: artifact.Sha256, Bytes: len(artifact.Content), Absent: artifact.Absent, DerivationSha256: artifact.DerivationSha256})
		spans, err := splitWinnerMultipassArtifact(artifact, settings)
		if err != nil {
			return nil, err
		}
		for _, span := range spans {
			spanBytes := span.ContextEnd - span.ContextStart
			if len(plan.Units) == 0 || unitBytes+spanBytes > settings.SpanBytes {
				plan.Units = append(plan.Units, winnerMultipassUnit{Id: fmt.Sprintf("unit-%04d", len(plan.Units)+1), Spans: []winnerMultipassSpan{}})
				unitBytes = 0
			}
			last := len(plan.Units) - 1
			plan.Units[last].Spans = append(plan.Units[last].Spans, span)
			unitBytes += spanBytes
			if len(plan.Units) > settings.MaximumCoverageAtoms {
				return nil, errors.New("complete source coverage exceeds coverage-atom bound")
			}
		}
	}
	groups := []winnerMultipassScanGroup{}
	// This is an offline layout only. A larger source census does not increase
	// the model-call allowance; prospective real token packing must admit it.
	atomsPerGroup := max(1, (len(plan.Units)+settings.MaximumLeafUnits-1)/settings.MaximumLeafUnits)
	for _, member := range provider.Members {
		for start := 0; start < len(plan.Units); start += atomsPerGroup {
			group := winnerMultipassScanGroup{Member: member.Name, UnitIds: []string{}}
			for _, unit := range plan.Units[start:min(start+atomsPerGroup, len(plan.Units))] {
				group.UnitIds = append(group.UnitIds, unit.Id)
			}
			groups = append(groups, group)
		}
	}
	if err := setWinnerMultipassScanGroups(plan, groups); err != nil {
		return nil, err
	}
	return plan, nil
}

// Rebuild the fixed synthesis DAG from a complete ordered grouping. This pure
// operation is allowed only during preparation, never after a durable call claim.
func setWinnerMultipassScanGroups(plan *winnerMultipassPlan, groups []winnerMultipassScanGroup) error {
	plan.Calls = []winnerMultipassCall{}
	wantIds := []string{}
	for _, unit := range plan.Units {
		wantIds = append(wantIds, unit.Id)
	}
	groupIndex := 0
	for _, member := range plan.Provider.Members {
		parents := []string{}
		gotIds := []string{}
		for groupIndex < len(groups) && groups[groupIndex].Member == member.Name {
			group := groups[groupIndex]
			if len(group.UnitIds) == 0 {
				return errors.New("multi-pass scan grouping contains an empty leaf")
			}
			gotIds = append(gotIds, group.UnitIds...)
			call := winnerMultipassCall{Id: fmt.Sprintf("%s-scan-%04d", member.Name, len(parents)+1), Member: member, Stage: "scan", UnitIds: append([]string{}, group.UnitIds...), Parents: []string{}}
			plan.Calls = append(plan.Calls, call)
			parents = append(parents, call.Id)
			if len(parents) > plan.Settings.MaximumLeafUnits {
				return errors.New("multi-pass member exceeds actual scan-leaf bound")
			}
			groupIndex++
		}
		if !slices.Equal(gotIds, wantIds) {
			return errors.New("multi-pass member scan grouping omits, duplicates or reorders complete coverage")
		}
		for level := 1; len(parents) > plan.Settings.MergeFanIn; level++ {
			next := []string{}
			for start := 0; start < len(parents); start += plan.Settings.MergeFanIn {
				end := min(start+plan.Settings.MergeFanIn, len(parents))
				call := winnerMultipassCall{Id: fmt.Sprintf("%s-merge-%03d-%04d", member.Name, level, len(next)+1), Member: member, Stage: "merge",
					UnitIds: []string{}, Parents: append([]string{}, parents[start:end]...)}
				plan.Calls = append(plan.Calls, call)
				next = append(next, call.Id)
			}
			parents = next
		}
		plan.Calls = append(plan.Calls, winnerMultipassCall{Id: member.Name + "-final", Member: member, Stage: "final", UnitIds: []string{}, Parents: parents})
	}
	if groupIndex != len(groups) || len(plan.Calls) > plan.Settings.MaximumCalls {
		return errors.New("complete two-member plan exceeds total call bound or has extra scan groups")
	}
	plan.ScanGroups = append([]winnerMultipassScanGroup{}, groups...)
	return nil
}

// Recompute rather than trust a hand-edited ledger, call order, clock or digest.
func validateWinnerMultipassPlan(plan *winnerMultipassPlan, bundle winnerMultipassBundle) error {
	if plan == nil {
		return errors.New("multi-pass plan is absent")
	}
	want, err := prepareWinnerMultipass(bundle, plan.Owner, plan.Settings, plan.Provider)
	if err != nil {
		return err
	}
	if err := setWinnerMultipassScanGroups(want, plan.ScanGroups); err != nil {
		return err
	}
	actualBytes, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	wantBytes, err := json.Marshal(want)
	if err != nil || string(actualBytes) != string(wantBytes) {
		return errors.New("multi-pass plan differs from complete deterministic coverage")
	}
	return nil
}
