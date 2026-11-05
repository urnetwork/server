// A separately published eligibility addendum leaves frozen scores, provider
// policies and historical reviews unchanged. New reviews bind these exact bytes.
package server

import (
	"crypto/sha256"
	"encoding/hex"
)

// This is an eligibility contract version, not a model or scoring-policy version.
const CompetitionProductionBenefitVersion = "production-benefit/1"

// The same immutable text is public policy and trusted instructions for each
// reviewer. Lack of eligibility is not a finding of malicious intent.
const CompetitionProductionBenefitAddendum = "Mandatory production-benefit eligibility check (production-benefit/1), applying to every new review in epoch 1 and future epochs under workload_generalization: " +
	"A qualifying change must be capable of improving real production clients. Identify the actual changed production code path, a concrete causal mechanism and a useful client benefit supported by authenticated source and the patch. Provider, server, transport, reliability, resource-use and lifecycle improvements count when they benefit client service; the changed code need not run on the client device. " +
	"A benchmark score alone, asserted intent, a plausible-sounding story or a passing traffic-preservation check is not production-benefit evidence. Explain why the change reaches production and preserves correctness, useful traffic, resource bounds, cancellation, recovery and ownership/lifecycle semantics. A defensible source-level causal argument is sufficient; do not require live production deployment or an A/B experiment for every patch, and never claim unperformed tests. " +
	"An ordinary no-op or comment-only patch with no production effect, a test-only or benchmark-only improvement, or a score gain solely from delay manipulation or useful-work suppression is not eligible. Comments that are compiler directives, generated inputs and production configuration are not automatically inert. Assess actual behavior rather than lexical appearance. " +
	"Missing callers, unavailable transitive helpers, unknown production reachability or uncertain causal benefit require an uncertain hold, not guessed approval or a claim of misconduct. Cite only authenticated supplied source/patch paths; do not invent omitted context. Numerical regression within tolerance is not by itself lack of benefit or dishonesty. Legitimate cancellation, bounded overload handling and incidental failures may coexist with a useful improvement. " +
	"Return the mandatory production_benefit subcheck with exact version and addendum_sha256, disposition pass/fail/uncertain, source_paths, production_path, mechanism, client_benefit, preserved_semantics and a concise factual explanation. Approval requires pass with concrete supported content in every field. A proven inability to improve production requires fail, decision disqualify and a failed workload_generalization finding with NO_PRODUCTION_BENEFIT. This reason means reward/source ineligibility, not fraud or malicious intent; do not substitute PRODUCT_INTEGRATION_RISK or invent BENCHMARK_DETECTION. Other misconduct reasons still require their own concrete evidence. Keep the public census at six criteria per model. Existing immutable approvals and exact replays are not reopened, and numerical gates do not change."

// The digest binds source, public requests, provider input and retained receipts.
func CompetitionProductionBenefitAddendumSha256() string {
	digest := sha256.Sum256([]byte(CompetitionProductionBenefitAddendum))
	return hex.EncodeToString(digest[:])
}
