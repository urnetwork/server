// Target-refusal totals carry no independent-peer authority. These controls
// exercise actual scoring, term weights, and public solve results.
package solve

import (
	"math"
	"testing"
)

// A complete, consistent synthetic graph. Twenty sources let one outlier
// exceed the default exclusion threshold without modifying the policy.
func refusalAuthorityProblemInputs(settings *Settings) ([]Node, []Term, map[string]Refusals) {
	nodes := make([]Node, 0, 20)
	nodeIdNodes := map[string]Node{}
	nodeIdRefusals := map[string]Refusals{}
	for i := 0; i < 20; i += 1 {
		id := syntheticNodeId(i)
		node := Node{Id: id, Genesis: onEquator(float64(i) * 100), RadiusKm: 5}
		nodes = append(nodes, node)
		nodeIdNodes[id] = node
		nodeIdRefusals[id] = Refusals{PingsAsPinger: 400, PingsAsTarget: 400}
	}
	terms := make([]Term, 0, len(nodes)*(len(nodes)-1))
	for _, source := range nodes {
		for _, target := range nodes {
			if source.Id != target.Id {
				terms = append(terms, termWithResidual(settings, nodeIdNodes, source.Id, target.Id, 0))
			}
		}
	}
	return nodes, terms, nodeIdRefusals
}

// One reporter can concentrate repeated allegations on an otherwise consistent
// target. Only that reporter's own evidence may change its term weights.
func TestReputationTargetRefusalConcentratedReporter(t *testing.T) {
	settings := DefaultSettings()
	nodes, terms, nodeIdRefusals := refusalAuthorityProblemInputs(settings)
	baseline, baselineScores := scoredProblem(nodes, terms, nodeIdRefusals, settings)
	baseline.reweight(baselineScores)

	reporterId := nodes[0].Id
	targetId := nodes[1].Id
	reporter := nodeIdRefusals[reporterId]
	reporter.AsPinger = 400
	nodeIdRefusals[reporterId] = reporter
	target := nodeIdRefusals[targetId]
	target.AsTarget = 400
	nodeIdRefusals[targetId] = target
	problem, scores := scoredProblem(nodes, terms, nodeIdRefusals, settings)
	problem.reweight(scores)
	targetIndex := problem.nodeIdIndexes[targetId]
	if scores.excluded[targetIndex] || scores.q[targetIndex] != baselineScores.q[targetIndex] {
		t.Fatalf("one reporter's unsigned target allegations changed target authority: q=%v baseline=%v excluded=%t", scores.q[targetIndex], baselineScores.q[targetIndex], scores.excluded[targetIndex])
	}
	for k, term := range problem.terms {
		if problem.nodes[term.source].Id != reporterId && term.weight != baseline.terms[k].weight {
			t.Fatalf("allegations changed an unrelated source's term weight: source=%s got=%v baseline=%v", problem.nodes[term.source].Id, term.weight, baseline.terms[k].weight)
		}
	}
	if !scores.excluded[problem.nodeIdIndexes[reporterId]] {
		t.Fatal("reporter-side evidence was removed with uncorroborated target allegations")
	}
}

// Repeated aggregate counts are not distinct, independent peers. Increasing
// their volume must not manufacture corroboration or a target penalty.
func TestReputationTargetRefusalVolumeDoesNotCorroborate(t *testing.T) {
	settings := DefaultSettings()
	for _, count := range []int{20, 400, 1000000} {
		nodes, terms, nodeIdRefusals := refusalAuthorityProblemInputs(settings)
		for id, refusal := range nodeIdRefusals {
			refusal.PingsAsTarget = count
			nodeIdRefusals[id] = refusal
		}
		targetId := nodes[1].Id
		target := nodeIdRefusals[targetId]
		target.AsTarget = count
		nodeIdRefusals[targetId] = target
		problem, scores := scoredProblem(nodes, terms, nodeIdRefusals, settings)
		i := problem.nodeIdIndexes[targetId]
		if scores.excluded[i] || scores.q[i] != 1 {
			t.Fatalf("aggregate volume %d was treated as target corroboration: q=%v excluded=%t", count, scores.q[i], scores.excluded[i])
		}
	}
}

// The public multi-round solve preserves target diagnostics but never applies
// them as target authority in a later reputation round.
func TestReputationTargetRefusalPublicSolvePreservesAuthority(t *testing.T) {
	settings := DefaultSettings()
	nodes, terms, nodeIdRefusals := refusalAuthorityProblemInputs(settings)
	baseline := Solve(nodes, terms, nodeIdRefusals, nil, settings)
	targetId := nodes[1].Id
	target := nodeIdRefusals[targetId]
	target.AsTarget = target.PingsAsTarget
	nodeIdRefusals[targetId] = target
	result := Solve(nodes, terms, nodeIdRefusals, nil, settings)
	got := result.Node(targetId)
	want := baseline.Node(targetId)
	if got == nil || want == nil {
		t.Fatal("synthetic target missing from public result")
	}
	if got.Excluded || got.Q != want.Q || got.SourceTermCount != want.SourceTermCount || got.PingCount != want.PingCount {
		t.Fatalf("target allegations changed the public solve's authority: q=%v baseline=%v excluded=%t terms=%d/%d pings=%d/%d", got.Q, want.Q, got.Excluded, got.SourceTermCount, want.SourceTermCount, got.PingCount, want.PingCount)
	}
	assertNear(t, "public target refusal rate", got.RefusalRateAsTarget, 1, 1e-12)
	assertNear(t, "public target refusal z", got.RefusalAsTargetZ, math.Sqrt(19), 1e-12)
	if result.Population.RefusalRateAsTarget.Count != len(nodes) {
		t.Fatal("target diagnostic population disappeared")
	}
}

// Diagnostic rates, z-scores, and population remain available even when they
// are not admissible as evidence against the target.
func TestReputationTargetRefusalDiagnosticsRemain(t *testing.T) {
	settings := DefaultSettings()
	nodes, terms, nodeIdRefusals := refusalAuthorityProblemInputs(settings)
	targetId := nodes[1].Id
	target := nodeIdRefusals[targetId]
	target.AsTarget = target.PingsAsTarget
	nodeIdRefusals[targetId] = target
	problem, scores := scoredProblem(nodes, terms, nodeIdRefusals, settings)
	i := problem.nodeIdIndexes[targetId]
	var result NodeResult
	scores.fill(int(i), &result)
	assertNear(t, "diagnostic rate", result.RefusalRateAsTarget, 1, 1e-12)
	assertNear(t, "diagnostic z", result.RefusalAsTargetZ, math.Sqrt(19), 1e-12)
	population := scores.population().RefusalRateAsTarget
	if population.Count != len(nodes) {
		t.Fatalf("target diagnostic count=%d want=%d", population.Count, len(nodes))
	}
	assertNear(t, "diagnostic population mean", population.Mean, 1.0/20, 1e-12)
}

// The reporter remains responsible for its own attestations. This control
// prevents removing every refusal input while fixing the target boundary.
func TestReputationTargetRefusalReporterEvidenceStillExcludes(t *testing.T) {
	settings := DefaultSettings()
	nodes, terms, nodeIdRefusals := refusalAuthorityProblemInputs(settings)
	reporterId := nodes[0].Id
	reporter := nodeIdRefusals[reporterId]
	reporter.AsPinger = reporter.PingsAsPinger
	nodeIdRefusals[reporterId] = reporter
	problem, scores := scoredProblem(nodes, terms, nodeIdRefusals, settings)
	problem.reweight(scores)
	i := problem.nodeIdIndexes[reporterId]
	if !scores.excluded[i] {
		t.Fatalf("reporter evidence was weakened: excluded=%t q=%v", scores.excluded[i], scores.q[i])
	}
	assertNear(t, "reporter evidence weight", scores.q[i], settings.MinQ, 1e-12)
	for _, term := range problem.terms {
		if term.source == i && term.weight != 0 {
			t.Fatal("excluded reporter retained weighted source terms")
		}
	}
}

// Geometric inconsistency is independent evidence. Ignoring unsigned target
// allegations must not protect a source whose own co-signed terms disagree.
func TestReputationTargetRefusalGeometricEvidenceStillExcludes(t *testing.T) {
	settings := DefaultSettings()
	nodes, terms, _ := refusalAuthorityProblemInputs(settings)
	inconsistentId := nodes[0].Id
	for k := range terms {
		if terms[k].Source == inconsistentId {
			terms[k].RttMs += 100 / settings.KmPerMs
		}
	}
	problem, scores := scoredProblem(nodes, terms, nil, settings)
	i := problem.nodeIdIndexes[inconsistentId]
	if !scores.excluded[i] {
		t.Fatalf("geometric evidence was weakened: excluded=%t q=%v scatter=%v bias=%v", scores.excluded[i], scores.q[i], scores.z[statisticScatter][i], scores.z[statisticBias][i])
	}
	assertNear(t, "geometric evidence weight", scores.q[i], settings.MinQ, 1e-12)
}

// A healthy, consistent graph with no refusal history stays fully weighted.
func TestReputationTargetRefusalAbsentHistoryHealthy(t *testing.T) {
	settings := DefaultSettings()
	nodes, terms, _ := refusalAuthorityProblemInputs(settings)
	problem, scores := scoredProblem(nodes, terms, nil, settings)
	for i, node := range problem.nodes {
		if scores.excluded[i] || scores.q[i] != 1 || scores.has[statisticRefusalAsTarget][i] {
			t.Fatalf("healthy source %s changed without refusal data: q=%v excluded=%t", node.Id, scores.q[i], scores.excluded[i])
		}
	}
}
