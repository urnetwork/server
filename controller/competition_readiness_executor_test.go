// Baseline production and private execution retain distinct qualified identities
// even when their heartbeats use the same local physical host id.
package controller

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The real register/read path must not lose readiness when the local private
// executor replaces its baseline-producer heartbeat after full qualification.
func TestCompetitionReadinessRecognizesFrozenPrivateExecutor(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, false)
		selected := fixture.selected(0)
		check := passingHostCheck(selected)
		check.HostId = selected.executorHostId
		check.QualificationSha256 = competitionExecutorQualificationSha256(selected)
		check.RebaselinePassed, check.RebaselineRoundId = true, &fixture.round.RoundId
		if check.QualificationSha256 == fixture.settings.EvaluationPolicy.HostQualificationSha256 {
			tb.Fatal("fixture collapsed private and producer identities")
		}
		if err := fixture.store.RegisterHost(tb.Context(), selected, check); err != nil {
			tb.Fatal(err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			hosts, artifacts, rebaselines, err := readCompetitionReadinessHosts(tb.Context(), conn, fixture.settings, fixture.clock.Now())
			if err != nil || hosts != 1 || artifacts != 1 || rebaselines != 1 {
				tb.Fatal("API readiness lost the genuine exact private executor heartbeat", hosts, artifacts, rebaselines, err)
			}
		})
	})
}

// Corrected identities require the actual immutable overlay; a foreign or old
// fingerprint cannot become ready merely by using an approved physical host id.
func TestCompetitionReadinessRecognizesOnlyAmendedPrivateExecutor(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, false)
		value := executorControlPgValue(fixture)
		if err := insertExecutorControlPgValue(tb.Context(), fixture, value); err != nil {
			tb.Fatal(err)
		}
		if err := fixture.store.loadExecutorControlAmendment(tb.Context(), fixture.settings, fixture.round); err != nil {
			tb.Fatal(err)
		}
		selected := fixture.selected(0)
		check := passingHostCheck(selected)
		check.HostId, check.QualificationSha256 = selected.executorHostId, competitionExecutorQualificationSha256(selected)
		check.RebaselinePassed, check.RebaselineRoundId = true, &fixture.round.RoundId
		if err := fixture.store.RegisterHost(tb.Context(), selected, check); err != nil {
			tb.Fatal(err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			hosts, _, rebaselines, err := readCompetitionReadinessHosts(tb.Context(), conn, fixture.settings, fixture.clock.Now())
			if err != nil || hosts != 1 || rebaselines != 1 {
				tb.Fatal("API readiness ignored the registered corrected identity", err)
			}
		})
		oldSettings := *selected
		oldSettings.executorControlAmendment = nil
		check.QualificationSha256 = value.Hosts[0].PreviousQualificationSha256
		if !check.Eligible(&oldSettings) {
			tb.Fatal("old eligible heartbeat counterfactual is invalid")
		}
		if err := fixture.store.RegisterHost(tb.Context(), &oldSettings, check); err != nil {
			tb.Fatal(err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			hosts, _, rebaselines, err := readCompetitionReadinessHosts(tb.Context(), conn, fixture.settings, fixture.clock.Now())
			if err != nil || hosts != 0 || rebaselines != 0 {
				tb.Fatal("old but eligible private heartbeat survived operational amendment", hosts, rebaselines, err)
			}
		})
		foreignSettings := *selected
		check.HostId, check.QualificationSha256 = "unapproved-executor", strings.Repeat("f", 64)
		foreignSettings.DistributedEvaluationPolicy = nil
		foreignSettings.executorHostId, foreignSettings.executorSlotId = "", 0
		foreignSettings.executorControlAmendment = nil
		// Even a legacy eligible row with the right image/hardware is not a
		// replacement for exact frozen/amended physical executor authority.
		foreignSettings.EvaluationPolicy.HostQualificationSha256 = check.QualificationSha256
		if !check.Eligible(&foreignSettings) {
			tb.Fatal("foreign legacy eligible heartbeat counterfactual is invalid")
		}
		if err := fixture.store.RegisterHost(tb.Context(), &foreignSettings, check); err != nil {
			tb.Fatal(err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			hosts, _, _, err := readCompetitionReadinessHosts(tb.Context(), conn, fixture.settings, fixture.clock.Now())
			if err != nil || hosts != 0 {
				tb.Fatal("unapproved heartbeat became grading readiness", hosts, err)
			}
		})
	})
}
