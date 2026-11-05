// The operator's traffic-preservation clarification accompanies every review,
// including frozen epoch-one candidates. It changes neither scoring nor the
// public six-criterion contract. Missing evidence/checks never authorize a winner.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// This identifies immutable operator clarification text, not a new round policy.
const winnerWorkloadReviewVersion = "traffic-preservation/1"

// The same clarification is sent as trusted instructions to every provider.
// Thresholds remain in authenticated score/source evidence, never in this rule.
const winnerWorkloadReviewInstructions = "Mandatory traffic-preservation check (traffic-preservation/1), applying the existing measurement_integrity, workload_generalization and source_safety criteria: " +
	"Inspect whether the patch obtains a better score or satisfies a volume gate by deliberately abandoning useful traffic, reducing healthy-operation deadlines, refusing large or slow transfers, suppressing retries, or preventing descendant requests from being discovered or attempted. " +
	"This can be prohibited workload manipulation even without evaluator detection, forged telemetry or a benchmark-specific branch. Passing numerical gates is not permission to spend their tolerances on deliberate work suppression. " +
	"Trace the concrete changed code and its effect on intended work using context.workload_review.instrument_files, which are authenticated pristine instrument files bound to context.packet.instrument's activated source, image and frozen evaluation policy, not the moving control checkout or post-patch source. An instrument overlay's selected product parent is not its instrumented server commit. Consult the actual score, success denominator, timeout boundary, request/byte accounting, discovery and admission logic; do not assume a later instrument has epoch one's semantics. The supplied file census is not a complete transitive dependency graph; unavailable helpers require uncertainty or additional authenticated operator context. " +
	"In the historical discovery-gated crawl, an incomplete parent returns no child URLs: the attempted parent receives the configured failed-observation penalty, but never-started descendants have no request rows. Replacing fixed-population timings by timeout penalties cannot itself improve p95; reduced contention or a changed population can. A failure fraction below five percent does not guarantee a small p95 effect. " +
	"Examine baseline/candidate and per-replicate latency, success, attempted requests and received bytes together. A large latency gain with reduced success or work is an investigation signal, never proof or an automatic disqualification. " +
	"Distinguish deliberate benchmark-serving work suppression from incidental failures, ordinary measurement variation, legitimate bounded overload handling, cancellation, security refusals and genuine optimizations preserving service semantics. Do not invent a zero-loss rule or reject solely because success fell within the published tolerance. " +
	"Require concrete code/dataflow evidence for a failed check and a corresponding failed existing criterion with an applicable existing reason code; do not claim BENCHMARK_DETECTION without evidence of detection. Missing transitive context or uncertain causality requires uncertainty, not guessed approval or misconduct. " +
	"Return the mandatory workload_preservation subcheck with version traffic-preservation/1, disposition pass/fail/uncertain and a concise factual explanation citing the relevant changed code and preserved or suppressed work. Approval requires pass; fail must be supported by a failed measurement_integrity, workload_generalization or source_safety finding. Keep the public findings census at exactly six."

// Complete selected files keep local helpers visible. This is not a claim of
// complete transitive context; moved/new external helpers need a version update.
func winnerWorkloadInstrumentPaths() []string {
	return []string{
		"server/connect/sim-latency/client.go",
		"server/connect/sim-latency/config.go",
		"server/connect/sim-latency/results.go",
		"server/connect/sim-latency/run.go",
		"server/connect/sim-latency/score.go",
		"server/connect/sim-latency/score_work_coverage.go",
		"server/connect/sim-latency/site.go",
		"server/connect/sim-latency/stats_math.go",
		"server/connect/sim-latency/work_coverage.go",
		"server/connect/sim-latency/work_coverage_plan.go",
	}
}

// Prospective coverage helpers may be absent at a historical base. Absence is
// explicitly authenticated, never silently replaced by moving source content.
func winnerWorkloadInstrumentMayBeAbsent(path string) bool {
	return path == "server/connect/sim-latency/score_work_coverage.go" || path == "server/connect/sim-latency/work_coverage.go" || path == "server/connect/sim-latency/work_coverage_plan.go"
}

// Separate bounds prevent untouched instrument context from consuming the
// touched-file budget. Nonpositive values refuse context rather than truncate it.
type winnerWorkloadReviewSettings struct {
	// Per-file bytes; default 1 MiB accommodates the complete frozen scorer.
	MaximumFileBytes int
	// Aggregate bytes; default 2 MiB bounds complete instrument context.
	MaximumBytes int
	// Per-check explanation bytes; default 4096 matches existing findings.
	MaximumExplanationBytes int
}

// The context budget is independent of candidate input and output limits.
func defaultWinnerWorkloadReviewSettings() winnerWorkloadReviewSettings {
	return winnerWorkloadReviewSettings{MaximumFileBytes: 1024 * 1024, MaximumBytes: 2 * 1024 * 1024, MaximumExplanationBytes: 4096}
}

// The supervisor authenticates Git blobs before handing them to the tool-free
// broker. The entire object enters the input digest and non-clock cache identity.
type winnerWorkloadReviewContext struct {
	Version         string                   `json:"version"`
	Addendum        string                   `json:"addendum"`
	AddendumSha256  string                   `json:"addendum_sha256"`
	ServerCommit    string                   `json:"server_commit"`
	InstrumentFiles []winnerReviewSourceFile `json:"instrument_files"`
}

// A model must explicitly perform this subcheck, without adding a seventh
// public criterion or converting a metric correlation into a native verdict.
type winnerWorkloadPreservationFinding struct {
	Version     string `json:"version"`
	Disposition string `json:"disposition"`
	Explanation string `json:"explanation"`
}

// The digest makes the fixed clarification independently visible in receipts.
func winnerWorkloadReviewAddendumSha256() string {
	digest := sha256.Sum256([]byte(winnerWorkloadReviewInstructions))
	return hex.EncodeToString(digest[:])
}

// Read the protected instrument even when none of its files are patch targets.
// No worktree contents, candidate execution or network access is involved.
func loadWinnerWorkloadReviewContext(ctx context.Context, repositoriesRoot string, packet *controller.CompetitionSourceReviewPacket, settings winnerWorkloadReviewSettings) (*winnerWorkloadReviewContext, error) {
	if packet == nil || !sourceGitShaPattern.MatchString(packet.BaseSha) || settings.MaximumFileBytes <= 0 || settings.MaximumBytes <= 0 {
		return nil, errors.New("traffic-preservation review requires bounded frozen instrument context")
	}
	if err := controller.ValidateCompetitionReviewInstrument(packet); err != nil {
		return nil, err
	}
	if packet.PatchPolicy.PathBase != "" {
		lock, err := model.CompetitionSourceLockDigestForMode(packet.SourceCommits, packet.Seal.Staging)
		if packet.PatchPolicy.PathBase != "mono_root_v1" || err != nil || lock != packet.Entry.BaseSourceLockSha256 || packet.SourceCommits["server"] != packet.BaseSha {
			return nil, errors.New("traffic-preservation context differs from the frozen source lock")
		}
	}
	result := &winnerWorkloadReviewContext{Version: winnerWorkloadReviewVersion, Addendum: winnerWorkloadReviewInstructions,
		AddendumSha256: winnerWorkloadReviewAddendumSha256(), ServerCommit: packet.Instrument.ServerCommit}
	contextBytes := 0
	for _, path := range winnerWorkloadInstrumentPaths() {
		absent := false
		var err error
		if winnerWorkloadInstrumentMayBeAbsent(path) {
			absent, err = sourceGitPathAbsentAtCommit(ctx, filepath.Join(repositoriesRoot, "server"), packet.Instrument.ServerCommit, strings.TrimPrefix(path, "server/"), settings.MaximumFileBytes)
			if err != nil {
				return nil, err
			}
		}
		var content []byte
		if !absent {
			content, err = sourceGitBlobAtCommit(ctx, filepath.Join(repositoriesRoot, "server"), packet.Instrument.ServerCommit, strings.TrimPrefix(path, "server/"), settings.MaximumFileBytes)
			if err != nil {
				return nil, err
			}
		}
		contextBytes += len(content)
		if !absent && len(content) == 0 || contextBytes > settings.MaximumBytes {
			return nil, errors.New("traffic-preservation instrument is empty or exceeds its byte bound")
		}
		digest := sha256.Sum256(content)
		result.InstrumentFiles = append(result.InstrumentFiles, winnerReviewSourceFile{Path: path, Content: string(content), Sha256: hex.EncodeToString(digest[:]), AbsentAtBase: absent})
	}
	return result, nil
}

// Native validation checks the complete trusted-parent envelope before any
// provider call; a content hash alone is not an independent Git authentication.
func validateWinnerWorkloadReviewContext(request winnerReviewBrokerRequest) error {
	value, settings := request.WorkloadReview, defaultWinnerWorkloadReviewSettings()
	if err := controller.ValidateCompetitionReviewInstrument(request.Packet); err != nil {
		return err
	}
	if request.Packet == nil || value == nil || value.Version != winnerWorkloadReviewVersion || value.Addendum != winnerWorkloadReviewInstructions || value.AddendumSha256 != winnerWorkloadReviewAddendumSha256() ||
		!sourceGitShaPattern.MatchString(value.ServerCommit) || value.ServerCommit != request.Packet.Instrument.ServerCommit {
		return errors.New("traffic-preservation addendum or frozen instrument identity is missing or changed")
	}
	paths := winnerWorkloadInstrumentPaths()
	if len(value.InstrumentFiles) != len(paths) {
		return errors.New("traffic-preservation instrument context is incomplete")
	}
	contextBytes := 0
	for index, file := range value.InstrumentFiles {
		digest := sha256.Sum256([]byte(file.Content))
		contextBytes += len(file.Content)
		if file.Path != paths[index] || (file.AbsentAtBase && (!winnerWorkloadInstrumentMayBeAbsent(file.Path) || file.Content != "")) || (!file.AbsentAtBase && len(file.Content) == 0) || settings.MaximumFileBytes < len(file.Content) || settings.MaximumBytes < contextBytes || file.Sha256 != hex.EncodeToString(digest[:]) {
			return errors.New("traffic-preservation instrument context is substituted, unbounded or unauthenticated")
		}
	}
	return nil
}

// The check is evidence, not a score gate. A missing or uncertain check cannot
// approve; a failed check needs a concrete supporting public criterion.
func validateWinnerWorkloadPreservation(judgment winnerModelJudgment) error {
	check := judgment.WorkloadPreservation
	if check == nil || check.Version != winnerWorkloadReviewVersion || strings.TrimSpace(check.Explanation) == "" || len(check.Explanation) > defaultWinnerWorkloadReviewSettings().MaximumExplanationBytes {
		return errors.New("mandatory traffic-preservation finding is missing or invalid")
	}
	if check.Disposition != "pass" && check.Disposition != "fail" && check.Disposition != "uncertain" {
		return errors.New("traffic-preservation finding has an invalid disposition")
	}
	if judgment.Decision == "approve" && check.Disposition != "pass" {
		return errors.New("unresolved traffic-preservation finding cannot authorize approval")
	}
	if check.Disposition == "fail" {
		supported := false
		for _, finding := range judgment.Findings {
			if finding.Disposition != "fail" || (finding.Criterion != "measurement_integrity" && finding.Criterion != "workload_generalization" && finding.Criterion != "source_safety") {
				continue
			}
			for _, reason := range winnerSnowDispositionReasons("disqualify", judgment.ReasonCodes) {
				// Benign ineligibility alone is not proof of traffic suppression.
				supported = supported || reason != "NO_PRODUCTION_BENEFIT" && finding.ReasonCode == reason
			}
		}
		if !supported {
			return errors.New("traffic-preservation failure lacks a supporting existing criterion")
		}
	}
	return nil
}
