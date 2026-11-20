package monitor

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// The current SQL projects 25 cells. Missing progress/horizon rows are already
// coalesced to zero or empty text; cohort totals stay present for plain tables.
func vacuumHealthCurrentSourceRow(dead, threshold string) Row {
	over := "0"
	if threshold == "0" || threshold == "-1" {
		over = "1"
	}
	return Row{
		"synthetic_vacuum_table", dead, "never", threshold,
		"", "0", "0", "0", "0", "0", "0", "0",
		"0", "0", "", "", "0", "", "", "", "",
		"synthetic_vacuum_table", dead, "1", over,
	}
}

// Exercise the registered public signal through its real visibility reducer,
// so an invalid observation cannot pass as either health or proven dead tuples.
func TestVacuumHealthMalformedCurrentSourceIsUnknown(t *testing.T) {
	for _, test := range []struct {
		name   string
		column int
		value  string
	}{
		{"dead absent", 1, ""},
		{"dead nonnumeric", 1, "synthetic-private-dead-count"},
		{"dead negative", 1, "-1"},
		{"dead fractional", 1, "11000000.5"},
		{"dead overflow", 1, "9223372036854775808"},
		{"threshold absent", 3, ""},
		{"threshold nonnumeric", 3, "synthetic-private-threshold"},
		{"threshold fractional", 3, "25000000.5"},
		{"threshold overflow", 3, "9223372036854775808"},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := vacuumHealthCurrentSourceRow("11000000", "25000000")
			row[test.column] = test.value
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{row}, nil }}
			alerts, _ := NewWithSignals(syntheticSettings(source), NewVacuumHealthSignal()).Run(context.Background())
			if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
				t.Fatal("invalid current vacuum observation became health or a proven dead-tuple finding")
			}
			requireAlertOmits(t, alerts[0], "synthetic-private-dead-count", "synthetic-private-threshold")
		})
	}
}

// Shape is part of the current query contract, not implicit empty defaults.
func TestVacuumHealthMissingCurrentColumnsAreUnknown(t *testing.T) {
	for _, columns := range []int{0, 1, 3} {
		t.Run(strconv.Itoa(columns), func(t *testing.T) {
			row := vacuumHealthCurrentSourceRow("11000000", "25000000")[:columns]
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{row}, nil }}
			alerts, _ := NewWithSignals(syntheticSettings(source), NewVacuumHealthSignal()).Run(context.Background())
			if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
				t.Fatal("missing vacuum columns became health or a proven finding")
			}
		})
	}
}

// Keep the real empty result, absent progress/horizon, fixed floor, explicit
// cascade policy, bigint counts, and grouped partition shape compatible.
func TestVacuumHealthCurrentSourceHealthyAndViolatingControls(t *testing.T) {
	for _, test := range []struct {
		name string
		row  Row
		want string
	}{
		{"empty result", nil, ""},
		{"explicit cascade threshold with absent optional joins", vacuumHealthCurrentSourceRow("11000000", "25000000"), ""},
		{"default threshold above floor", vacuumHealthCurrentSourceRow("11000000", "0"), "dead-tuples"},
		{"minus-one threshold floor compatibility", vacuumHealthCurrentSourceRow("11000000", "-1"), "dead-tuples"},
		{"bigint dead count", vacuumHealthCurrentSourceRow("3000000000", "0"), "dead-tuples"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows := []Row{}
			if test.row != nil {
				rows = append(rows, test.row)
			}
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
			alerts, err := NewVacuumHealthSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal("valid current source shape failed")
			}
			if test.want == "" {
				if len(alerts) != 0 {
					t.Fatal("valid healthy control alerted")
				}
			} else if len(alerts) != 1 || alerts[0].Class != test.want {
				t.Fatal("valid violation lost its dead-tuple finding")
			}
		})
	}
	row := vacuumHealthCurrentSourceRow("11000000", "0")
	row[0], row[21], row[22], row[23], row[24] = "synthetic_vacuum_partition", "transfer_contract", "22000000", "2", "2"
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return []Row{row}, nil }}
	alerts, err := NewVacuumHealthSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 1 || alerts[0].Class != "dead-tuples" {
		t.Fatal("complete grouped partition control lost its finding")
	}
}

// A malformed source row must neither erase another row's proved debt nor
// disclose its private cells. Both source orders use the same stable finding.
func TestVacuumHealthMixedRowsKeepValidDebtAndUnknown(t *testing.T) {
	for _, invalidFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(invalidFirst), func(t *testing.T) {
			valid := vacuumHealthCurrentSourceRow("11000000", "0")
			invalid := vacuumHealthCurrentSourceRow("11000000", "25000000")
			invalid[0], invalid[1] = "synthetic-private-table", "synthetic-private-dead-count"
			rows := []Row{valid, invalid}
			if invalidFirst {
				rows[0], rows[1] = rows[1], rows[0]
			}
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
			alerts, err := NewWithSignals(syntheticSettings(source), NewVacuumHealthSignal()).Run(context.Background())
			if err != nil || len(alerts) != 2 {
				t.Fatal("mixed source rows lost independent debt or visibility")
			}
			requireAlertClass(t, alerts, "dead-tuples")
			unknown := requireAlertClass(t, alerts, "cannot-observe")
			if unknown.Frame != "vacuum-health-required-fields" {
				t.Fatal("vacuum unknown lost its fixed leaf identity")
			}
			encoded, err := json.Marshal(alerts)
			if err != nil {
				t.Fatal("encode mixed source findings")
			}
			var jsonLines strings.Builder
			if err := alerts.WriteJSONL(&jsonLines); err != nil {
				t.Fatal("render mixed source findings")
			}
			for _, public := range []string{alerts.Markdown(), string(encoded), jsonLines.String()} {
				for _, private := range []string{"synthetic-private-table", "synthetic-private-dead-count"} {
					if strings.Contains(public, private) {
						t.Fatal("mixed source output disclosed a private source cell")
					}
				}
			}
		})
	}
}
