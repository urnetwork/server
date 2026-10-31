package monitor

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const activePlanPrivateLiteral = "synthetic-active-private-literal"
const activePlanPrivateCell = "synthetic-active-private-cell"
const activePlanPrivateIndex = "synthetic_private_index_identity"

// Only the successful active and plan-wall source contracts are injected.
type activePlanPrivateFixture struct {
	healthy bool
	active  []Row
	first   []Row
	second  []Row
	stats   []Row
}

func activePlanPrivateValidFixture() activePlanPrivateFixture {
	return activePlanPrivateFixture{
		active: []Row{{"-73", "101", "-:-", "SELECT outcome FROM transfer_contract WHERE contract_id=$1"}},
		first: []Row{
			{"stmt", "-73", "10", "100"},
			{"idx", "transfer_contract_unresolved_source_pair_create_time", "10", "0"},
		},
		second: []Row{
			{"stmt", "-73", "12", "160"},
			{"idx", "transfer_contract_unresolved_source_pair_create_time", "13", "0"},
		},
		stats: []Row{{"2", "{f,t}", "0.5,0.5"}},
	}
}

// Call from a synctest bubble: the real 15s production interval is virtual.
// No fixture values, source errors, or alert evidence enter failure messages.
func activePlanPrivateObserve(t *testing.T, fixture activePlanPrivateFixture, ticks int) []Alert {
	t.Helper()
	summaryCalls, activeCalls, snapshotCalls, statsCalls := 0, 0, 0, 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "count(*) FILTER"):
			summaryCalls++
			if fixture.healthy {
				return []Row{{"6", "0", "0", "20"}}, nil
			}
			return []Row{{"101", "0", "0", "125"}}, nil
		case strings.Contains(query, "GROUP BY query_id ORDER BY backends DESC LIMIT 5"):
			activeCalls++
			return fixture.active, nil
		case strings.Contains(query, "FROM pg_stat_statements s"):
			snapshotCalls++
			if snapshotCalls == 1 {
				return fixture.first, nil
			}
			if snapshotCalls == 2 {
				return fixture.second, nil
			}
			t.Fatal("trip cache repeated its bounded snapshots")
		case strings.Contains(query, "FROM pg_stats WHERE tablename"):
			statsCalls++
			return fixture.stats, nil
		default:
			t.Fatal("unexpected synthetic query; real source fallback is prohibited")
		}
		return nil, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = time.Now
	monitor := NewWithSignals(settings, NewPostgresStateSignal())
	started := time.Now()
	observations := []Alert{}
	for tick := 0; tick < ticks; tick++ {
		alerts, err := monitor.Run(context.Background())
		if err != nil {
			t.Fatal("successful optional fixture became a whole-probe failure")
		}
		if fixture.healthy {
			if len(alerts) != 0 {
				t.Fatal("healthy core summary produced an alert")
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatal("successful optional fixture removed or multiplied the core alert")
		}
		alert := alerts[0]
		if alert.SignalID != "pg/active-pileup" || alert.Class != "active-pileup" ||
			alert.Target != "pg-1" || alert.Severity != SeverityPage || alert.Sustain != 2 ||
			alert.Observed != "active=101 idle_in_tx=0 total_client=125" {
			t.Fatal("successful optional fixture changed the core active condition")
		}
		if (tick > 0) != strings.Contains(alert.Evidence, "battery collected once at trip") {
			t.Fatal("successful optional fixture changed trip-cache provenance")
		}
		observations = append(observations, alert)
	}
	if summaryCalls != ticks {
		t.Fatal("summary cadence changed")
	}
	if fixture.healthy {
		if activeCalls != 0 || snapshotCalls != 0 || statsCalls != 0 || time.Since(started) != 0 {
			t.Fatal("healthy summary collected an optional diagnostic")
		}
	} else if activeCalls != 1 || snapshotCalls != 2 || statsCalls != 1 || time.Since(started) != 15*time.Second {
		t.Fatal("optional diagnostics changed the bounded collection sequence or virtual interval")
	}
	return observations
}

func activePlanPrivateAssertWithheld(t *testing.T, alert Alert, markers ...string) {
	t.Helper()
	var jsonl bytes.Buffer
	if err := WriteAlertsJSONL(&jsonl, []Alert{alert}); err != nil {
		t.Fatal("synthetic JSONL rendering failed")
	}
	for _, rendered := range []struct {
		name string
		text string
	}{
		{name: "Alert evidence", text: alert.Evidence},
		{name: "Alert Markdown", text: alert.Markdown()},
		{name: "complete Markdown", text: AlertsMarkdown([]Alert{alert})},
		{name: "JSONL", text: jsonl.String()},
	} {
		for i, marker := range markers {
			if strings.Contains(rendered.text, marker) {
				t.Errorf("%s retained a private fixture marker at position %d", rendered.name, i)
			}
		}
	}
}

func TestPostgresActiveSuccessfulEvidenceRejectsRawSqlLiteral(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := activePlanPrivateValidFixture()
		fixture.active[0][3] = "SELECT '" + activePlanPrivateLiteral + "'"
		for _, alert := range activePlanPrivateObserve(t, fixture, 1) {
			activePlanPrivateAssertWithheld(t, alert, activePlanPrivateLiteral)
		}
	})
}

func TestPostgresActiveSuccessfulEvidenceRejectsCachedRawSqlLiteral(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := activePlanPrivateValidFixture()
		fixture.active[0][3] = "SELECT '" + activePlanPrivateLiteral + "'"
		for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
			activePlanPrivateAssertWithheld(t, alert, activePlanPrivateLiteral)
		}
	})
}

func TestPostgresActiveSuccessfulEvidenceRejectsMalformedRenderedCells(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, column := range []int{0, 1, 2} {
			fixture := activePlanPrivateValidFixture()
			fixture.active[0][column] = activePlanPrivateCell
			for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
				activePlanPrivateAssertWithheld(t, alert, activePlanPrivateCell)
			}
		}
	})
}

func TestPostgresPlanWallSuccessfulEvidenceRejectsFreeFormIndexLabel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := activePlanPrivateValidFixture()
		fixture.first[1][1], fixture.second[1][1] = activePlanPrivateIndex, activePlanPrivateIndex
		for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
			activePlanPrivateAssertWithheld(t, alert, activePlanPrivateIndex)
		}
	})
}

func TestPostgresPlanWallSuccessfulEvidenceRejectsMalformedQueryId(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := activePlanPrivateValidFixture()
		fixture.first[0][1], fixture.second[0][1] = activePlanPrivateCell, activePlanPrivateCell
		for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
			activePlanPrivateAssertWithheld(t, alert, activePlanPrivateCell)
		}
	})
}

func TestPostgresPlanWallSuccessfulEvidenceRejectsMalformedStatsCells(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, column := range []int{0, 1, 2} {
			fixture := activePlanPrivateValidFixture()
			fixture.stats[0][column] = activePlanPrivateCell
			for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
				activePlanPrivateAssertWithheld(t, alert, activePlanPrivateCell)
			}
		}
	})
}

// This is a distinct attribution guard, not proof of credential exposure.
func TestPostgresPlanWallSuccessfulPartialStatsCannotClaimHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := activePlanPrivateValidFixture()
		fixture.stats = []Row{{"2"}}
		for _, alert := range activePlanPrivateObserve(t, fixture, 1) {
			if strings.Contains(alert.Evidence, "healthy (both values present)") {
				t.Error("incomplete optional statistics claimed a healthy observation")
			}
		}
	})
}

func TestPostgresActiveSuccessfulParameterizedNumericControl(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := activePlanPrivateValidFixture()
		// NULL query IDs are legitimate for an active group and are not zero.
		fixture.active = append(fixture.active, Row{"", "1", "Client:ClientRead", "FETCH FROM synthetic_cursor"})
		for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
			for _, diagnostic := range []string{
				"qid=-73 backends=101 waits=none query=withheld",
				"qid=uncomputed backends=1 waits=withheld query=withheld",
				"backends=101",
				"calls_15s=2 current=30.0ms lifetime=13.3ms",
				"pg_stats transfer_contract.open: n_distinct=2",
				"both Boolean values present in sampled statistics; plan health unproved",
			} {
				if !strings.Contains(alert.Evidence, diagnostic) {
					t.Error("valid numeric or Boolean diagnostic was lost")
				}
			}
			if strings.Contains(alert.Evidence, "failed:") {
				t.Error("valid signed/NULL query ID or optional data became an observation failure")
			}
		}
	})
}

func TestPostgresActiveSuccessfulEmptyDiagnosticsPreserveViolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, alert := range activePlanPrivateObserve(t, activePlanPrivateFixture{}, 2) {
			if strings.Contains(alert.Evidence, "failed:") || strings.Contains(alert.Evidence, "healthy (both values present)") {
				t.Error("legitimately empty optional observations became an error or healthy statistics")
			}
			if !strings.Contains(alert.Context, "separate snapshots") || !strings.Contains(alert.Context, "empty statement delta is unknown rather than healthy") {
				t.Error("empty optional observations lost their attribution limits")
			}
		}
	})
}

func TestPostgresActiveSuccessfulHealthySummarySkipsDiagnostics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := activePlanPrivateValidFixture()
		fixture.healthy = true
		activePlanPrivateObserve(t, fixture, 2)
	})
}

// A valid row before a malformed one must not become partial attribution.
func TestPostgresActiveSuccessfulInvalidProjectionPreservesCoreAndPlan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, row := range []Row{
			{"73", "1", "-:-"},
			{"73", "1", "-:-", "SELECT $1", "extra"},
			{"9223372036854775808", "1", "-:-", "SELECT $1"},
			{"73.5", "1", "-:-", "SELECT $1"},
			{"73", "-1", "-:-", "SELECT $1"},
			{"73", "1.5", "-:-", "SELECT $1"},
			{"73", "", "-:-", "SELECT $1"},
			{"73", "9223372036854775808", "-:-", "SELECT $1"},
			{activePlanPrivateCell, "1", "-:-", "SELECT $1"},
		} {
			fixture := activePlanPrivateValidFixture()
			fixture.active = append(fixture.active, row)
			for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
				activePlanPrivateAssertWithheld(t, alert, activePlanPrivateCell)
				if !strings.Contains(alert.Evidence, "active battery failed: error_class="+observationErrorClassInvalidResponse) ||
					strings.Contains(alert.Evidence, "top active query_ids:") ||
					!strings.Contains(alert.Evidence, "calls_15s=2 current=30.0ms lifetime=13.3ms") {
					t.Error("invalid active projection lost its fixed note, retained partial evidence, or erased independent plan evidence")
				}
			}
		}
	})
}

func TestPostgresActiveSuccessfulSignedIdAndWithheldLabelControls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := activePlanPrivateValidFixture()
		fixture.active = []Row{
			{"-9223372036854775808", "1", "-:-", "SELECT $1"},
			{"9223372036854775807", "2", activePlanPrivateCell, "SELECT $1"},
			{"", "0", "Client:ClientRead", "SELECT $1"},
		}
		for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
			activePlanPrivateAssertWithheld(t, alert, activePlanPrivateCell, "SELECT $1", "Client:ClientRead")
			for _, want := range []string{
				"qid=-9223372036854775808 backends=1 waits=none query=withheld",
				"qid=9223372036854775807 backends=2 waits=withheld query=withheld",
				"qid=uncomputed backends=0 waits=withheld query=withheld",
			} {
				if !strings.Contains(alert.Evidence, want) {
					t.Error("valid signed/NULL ID or fixed label projection was lost")
				}
			}
			if strings.Contains(alert.Evidence, "failed:") {
				t.Error("valid IDs or deliberately withheld labels became a source failure")
			}
		}
	})
}

func TestPostgresPlanWallSuccessfulInvalidSnapshotPreservesStats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, row := range []Row{
			{"stmt", "73", "1"},
			{"stmt", "73", "1", "1", "extra"},
			{"stmt", "", "1", "1"},
			{"stmt", "9223372036854775808", "1", "1"},
			{"stmt", activePlanPrivateCell, "1", "1"},
			{"stmt", "73", activePlanPrivateCell, "1"},
			{"stmt", "73", "1", activePlanPrivateCell},
			{"stmt", "73", "-1", "1"},
			{"stmt", "73", "1.5", "1"},
			{"stmt", "73", "9223372036854775808", "1"},
			{"stmt", "73", "1", "-1"},
			{"stmt", "73", "1", "1.5"},
			{"stmt", "73", "1", "NaN"},
			{"stmt", "73", "1", "+Inf"},
			{"idx", "", "1", "0"},
			{"idx", activePlanPrivateIndex, "1", "1"},
			{activePlanPrivateCell, "73", "1", "0"},
		} {
			for _, first := range []bool{true, false} {
				fixture := activePlanPrivateValidFixture()
				component := "second"
				if first {
					fixture.first = append(fixture.first, row)
					component = "first"
				} else {
					fixture.second = append(fixture.second, row)
				}
				for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
					activePlanPrivateAssertWithheld(t, alert, activePlanPrivateCell, activePlanPrivateIndex)
					if !strings.Contains(alert.Evidence, "snapshot delta failed: "+component+"_error_class="+observationErrorClassInvalidResponse) ||
						strings.Contains(alert.Evidence, "pg_stat_statements 15s delta") ||
						!strings.Contains(alert.Evidence, "pg_stats transfer_contract.open: n_distinct=2") {
						t.Error("malformed snapshot lost its owning note, retained partial delta, or erased independent statistics")
					}
				}
			}
		}
	})
}

func TestPostgresPlanWallSuccessfulFixedIndexRoles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, test := range []struct{ name, role string }{
			{name: "transfer_contract_unresolved_source_pair_create_time", role: "source-pair"},
			{name: "transfer_contract_unresolved_destination_pair_create_time", role: "destination-pair"},
			{name: "transfer_contract_unresolved_payer_transfer_byte_count", role: "payer"},
			{name: "transfer_contract_pair_open_create_time", role: "legacy-pair"},
			{name: "transfer_contract_open_partial_create_time", role: "legacy-open-create-time"},
			{name: activePlanPrivateIndex, role: "other-name-withheld"},
			{name: "transfer_contract_unresolved_source_pair_create_time_" + activePlanPrivateIndex, role: "other-name-withheld"},
		} {
			fixture := activePlanPrivateValidFixture()
			fixture.first[1][1], fixture.second[1][1] = test.name, test.name
			for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
				activePlanPrivateAssertWithheld(t, alert, test.name)
				if !strings.Contains(alert.Evidence, "rank=1 role="+test.role+" delta=3") || strings.Contains(alert.Evidence, "failed:") {
					t.Error("fixed index role projection changed its numeric delta or inferred a role from a name prefix")
				}
			}
		}
	})
}

// Resets and a missing first snapshot keep their existing delta semantics;
// this privacy projection does not certify interval/generation comparability.
func TestPostgresPlanWallSuccessfulCounterSemanticsUnchanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := activePlanPrivateValidFixture()
		fixture.second[0][2], fixture.second[0][3], fixture.second[1][2] = "8", "80", "7"
		for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
			if !strings.Contains(alert.Evidence, "calls_15s=-2 current=0.0ms lifetime=10.0ms") ||
				!strings.Contains(alert.Evidence, "rank=1 role=source-pair delta=-3") {
				t.Error("counter-reset arithmetic changed outside the rendering repair")
			}
		}
		fixture = activePlanPrivateValidFixture()
		fixture.first = nil
		for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
			if strings.Contains(alert.Evidence, "calls_15s=") || strings.Contains(alert.Evidence, "failed:") ||
				!strings.Contains(alert.Evidence, "rank=1 role=source-pair delta=13") {
				t.Error("legitimate empty first snapshot changed its existing unknown/delta behavior")
			}
		}
	})
}

func TestPostgresPlanWallSuccessfulFiniteNumericProjection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := activePlanPrivateValidFixture()
		fixture.first[0][3], fixture.second[0][3] = "1e3", "1060"
		for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
			if !strings.Contains(alert.Evidence, "calls_15s=2 current=30.0ms lifetime=88.3ms") || strings.Contains(alert.Evidence, "failed:") {
				t.Error("valid finite rounded duration changed during numeric projection")
			}
		}
	})
}

func TestPostgresPlanWallSuccessfulBooleanStatisticsMatrix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, test := range []struct {
			row         Row
			bothValues  bool
			singleValue bool
		}{
			{row: Row{"2", "{f,t}", "0.5,0.5"}, bothValues: true},
			{row: Row{"2", "{t,f}", "0.75,0.25"}, bothValues: true},
			{row: Row{"2", "{f,t}", "0.33333334,0.6666667"}, bothValues: true},
			{row: Row{"1", "{f}", "1"}, singleValue: true},
			{row: Row{"1", "{t}", "1"}, singleValue: true},
			{row: Row{"2", "{f}", "0.75"}},
			{row: Row{"1", "{f}", "0.75"}},
			{row: Row{"2", "{f,t}", "1,0"}},
			{row: Row{"2", "-", "-"}},
			{row: Row{"0", "-", "-"}},
			{row: Row{"-1", "-", "-"}},
			{row: Row{"-0.5", "{f,t}", "0.5,0.5"}},
			{row: Row{"0", "{}", ""}},
		} {
			fixture := activePlanPrivateValidFixture()
			fixture.stats = []Row{test.row}
			for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
				if strings.Contains(alert.Evidence, "failed:") || strings.Contains(alert.Evidence, "healthy (both values present)") ||
					strings.Contains(alert.Evidence, "both Boolean values present in sampled statistics; plan health unproved") != test.bothValues ||
					strings.Contains(alert.Evidence, "single-valued Boolean sample (2.3)") != test.singleValue ||
					strings.Contains(alert.Evidence, "unknown (Boolean statistics") != (!test.bothValues && !test.singleValue) {
					t.Error("valid Boolean, NULL, or partial statistics were misattributed")
				}
			}
		}
	})
}

func TestPostgresPlanWallSuccessfulMalformedStatisticsRetainCoreAndDelta(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, rows := range [][]Row{
			{{"2"}},
			{{"2", "{f,t}", "0.5,0.5", "extra"}},
			{{"2", "{f,t}", "0.5,0.5"}, {"2", "{f,t}", "0.5,0.5"}},
			{{activePlanPrivateCell, "{f,t}", "0.5,0.5"}},
			{{"2", activePlanPrivateCell, "0.5,0.5"}},
			{{"2", "{f,t}", activePlanPrivateCell}},
			{{"NaN", "-", "-"}},
			{{"+Inf", "-", "-"}},
			{{"-2", "-", "-"}},
			{{"3", "-", "-"}},
			{{"1.5", "-", "-"}},
			{{"2", "-", "0.5"}},
			{{"2", "{f,t}", "-"}},
			{{"2", "{f,f}", "0.5,0.5"}},
			{{"1", "{f,t}", "0.5,0.5"}},
			{{"0", "{f}", "1"}},
			{{"2", "{f,t}", "0.5"}},
			{{"2", "{f,t}", "NaN,0.5"}},
			{{"2", "{f,t}", "0.5,+Inf"}},
			{{"2", "{f,t}", "-0.1,0.5"}},
			{{"2", "{f,t}", "1.1,0.5"}},
			{{"2", "{f,t}", "0.75,0.75"}},
		} {
			fixture := activePlanPrivateValidFixture()
			fixture.stats = rows
			for _, alert := range activePlanPrivateObserve(t, fixture, 2) {
				activePlanPrivateAssertWithheld(t, alert, activePlanPrivateCell)
				if !strings.Contains(alert.Evidence, "pg_stats check failed: error_class="+observationErrorClassInvalidResponse) ||
					strings.Contains(alert.Evidence, "pg_stats transfer_contract.open:") ||
					!strings.Contains(alert.Evidence, "calls_15s=2 current=30.0ms lifetime=13.3ms") {
					t.Error("malformed statistics lost the fixed note, retained a verdict, or erased the independent delta")
				}
			}
		}
	})
}
