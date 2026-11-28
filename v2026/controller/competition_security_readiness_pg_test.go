// Public readiness authenticates the durable compiler exception independently
// of the private coordinator file, preserving historical control provenance.
package controller

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Exercise the real register/read boundary with a new image and fingerprint.
// The old frozen-image query deterministically cannot see this eligible row.
func TestCompetitionSecurityToolchainReadinessPgAuthenticatesExecutionHeartbeat(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, baseline := securityToolchainPgFixture(t, tb)
		selected := f.selected(0)
		approval := f.settings.securityToolchainException
		check := passingHostCheck(selected)
		check.HostId = selected.executorHostId
		check.ImageDigest = approval.EvaluatorImageDigest
		check.QualificationSha256 = approval.Hosts[0].QualificationSha256
		check.RebaselinePassed, check.RebaselineRoundId = true, &f.round.RoundId
		if !check.Eligible(selected) {
			tb.Fatal("synthetic execution heartbeat is not eligible")
		}
		if err := f.store.RegisterHost(tb.Context(), selected, check); err != nil {
			tb.Fatal(err)
		}
		api := *f.settings
		api.securityToolchainException = nil
		counts := func(settings *Settings) (int, int, int) {
			tb.Helper()
			var hosts, artifacts, rebaselines int
			server.Db(tb.Context(), func(conn server.PgConn) {
				var err error
				hosts, artifacts, rebaselines, err = readCompetitionReadinessHosts(tb.Context(), conn, settings, f.clock.Now())
				server.Raise(err)
			})
			return hosts, artifacts, rebaselines
		}
		if err := f.store.loadCompetitionReadinessSecurityToolchain(tb.Context(), &api, f.round, baseline); err != nil {
			tb.Fatal(err)
		}
		if h, a, r := counts(&api); h != 0 || a != 0 || r != 0 {
			tb.Fatal("unregistered private profile authorized public readiness", h, a, r)
		}
		if err := f.store.recordCompetitionSecurityToolchainException(tb.Context(), f.settings, f.round, baseline); err != nil {
			tb.Fatal(err)
		}
		if err := f.store.loadCompetitionReadinessSecurityToolchain(tb.Context(), &api, f.round, baseline); err != nil {
			tb.Fatal(err)
		}
		if h, a, r := counts(&api); h != 1 || a != 1 || r != 1 {
			tb.Fatal("authenticated execution image lost API readiness", h, a, r)
		}
		// Info and Readyz first call Store.Readiness with the API-purpose
		// settings; the later lifecycle projection owns a different copy.
		// This composed boundary must independently authenticate the decision.
		outer := api
		outer.readinessSecurityToolchainException = nil
		outer.intakeFirstActivation = &model.CompetitionSourceIntakeActivation{TargetRoundId: f.round.RoundId, TargetEpoch: 2}
		service := newServiceWithImageDigest(&outer, f.store, testApiImageDigest(), nil)
		checks, err := service.readinessChecks(tb.Context(), &outer)
		if err != nil || !checks["authoritative_evaluator_host"] || !checks["artifact_storage"] || !checks["host_rebaseline"] {
			tb.Fatal("outer public readiness lost the genuine execution heartbeat", checks, err)
		}
		if outer.readinessSecurityToolchainException != nil || outer.EvaluatorImageDigest != approval.PreviousEvaluatorImageDigest {
			tb.Fatal("shared readiness mutated its caller's frozen settings")
		}
		if api.EvaluatorImageDigest != approval.PreviousEvaluatorImageDigest || api.securityToolchainException != nil || api.EvaluationPolicy.SimulatorSha256 != approval.PreviousInstrumentSha256 {
			tb.Fatal("heartbeat authority rewrote the frozen control or enabled execution")
		}
		wrongRound := api
		wrongRound.automationRoundId = server.NewId()
		if h, _, _ := counts(&wrongRound); h != 0 {
			tb.Fatal("another round borrowed hotfix readiness")
		}
		// An old but eligible report cannot stand in for the new runtime.
		old := *selected
		old.securityToolchainException = nil
		oldCheck := passingHostCheck(&old)
		oldCheck.HostId = check.HostId
		oldCheck.QualificationSha256 = approval.Hosts[0].PreviousQualificationSha256
		oldCheck.RebaselinePassed, oldCheck.RebaselineRoundId = true, &f.round.RoundId
		if !oldCheck.Eligible(&old) {
			tb.Fatal("old-image counterfactual is not eligible")
		}
		if err := f.store.RegisterHost(tb.Context(), &old, oldCheck); err != nil {
			tb.Fatal(err)
		}
		if h, _, _ := counts(&api); h != 0 {
			tb.Fatal("historical runtime satisfied hotfix readiness")
		}
		// The new image with an old fingerprint must also fail closed, even
		// when a separately configured synthetic reporter marks it eligible.
		old.EvaluatorImageDigest = approval.EvaluatorImageDigest
		oldCheck.ImageDigest = approval.EvaluatorImageDigest
		if !oldCheck.Eligible(&old) {
			tb.Fatal("old-fingerprint counterfactual is not eligible")
		}
		if err := f.store.RegisterHost(tb.Context(), &old, oldCheck); err != nil {
			tb.Fatal(err)
		}
		if h, _, _ := counts(&api); h != 0 {
			tb.Fatal("new image borrowed the old host fingerprint")
		}
	})
}

// Correct immutable decision bytes cannot authorize a different frozen source,
// baseline, round, image or prior host, nor leave a previous success cached.
func TestCompetitionSecurityToolchainReadinessPgRejectsChangedScope(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, baseline := securityToolchainPgFixture(t, tb)
		if err := f.store.recordCompetitionSecurityToolchainException(tb.Context(), f.settings, f.round, baseline); err != nil {
			tb.Fatal(err)
		}
		for _, change := range []string{"baseline", "source", "image", "round", "prior-host"} {
			api := *f.settings
			api.securityToolchainException = nil
			copyBaseline := *baseline
			switch change {
			case "baseline":
				copyBaseline.Sha256 = strings.Repeat("b", 64)
			case "source":
				api.AutomationPolicy.SourceLockSha256 = strings.Repeat("c", 64)
			case "image":
				api.EvaluatorImageDigest = "sha256:" + strings.Repeat("d", 64)
			case "round":
				api.automationRoundId = server.NewId()
			case "prior-host":
				policy := *api.DistributedEvaluationPolicy
				policy.Hosts = append([]CompetitionExecutorHostPolicy(nil), policy.Hosts...)
				policy.Hosts[0].QualificationSha256 = strings.Repeat("e", 64)
				api.DistributedEvaluationPolicy = &policy
			}
			api.readinessSecurityToolchainException = f.settings.securityToolchainException
			if err := f.store.loadCompetitionReadinessSecurityToolchain(tb.Context(), &api, f.round, &copyBaseline); err == nil || api.readinessSecurityToolchainException != nil {
				tb.Fatal("invalid scope kept or created hotfix heartbeat authority", change, err)
			}
		}
	})
}
