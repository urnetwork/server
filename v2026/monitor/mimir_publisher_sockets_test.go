package monitor

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runSyntheticPublisherSocketOwners(t *testing.T, pid string, rows []string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "awk", "-v", "expected_pid="+pid,
		mimirPublisherSocketOwnersAwk+`{print publisherOwnsSocket($0,expected_pid)}`)
	command.Stdin = strings.NewReader(strings.Join(rows, "\n") + "\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic socket reducer failed: %v", err)
	}
	results := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(results) != len(rows) {
		t.Fatalf("socket reducer returned %d fixed results for %d rows", len(results), len(rows))
	}
	for _, result := range results {
		if result != "-1" && result != "0" && result != "1" {
			t.Fatal("socket reducer emitted something other than a fixed ownership enum")
		}
	}
	return results
}

func TestMimirPublisherSocketOwnersUsesStructuralPidOutsideQuotedNames(t *testing.T) {
	const prefix = "0 0 192.0.2.80:41000 192.0.2.10:3100 "
	cases := []struct {
		metadata string
		want     string
	}{
		{metadata: `users:(("fluent-bit",pid=77,fd=9))`, want: "1"},
		{metadata: `users:(("other",pid=177,fd=9))`, want: "0"},
		{metadata: `users:(("pid=77,",pid=177,fd=7))`, want: "0"},
		{metadata: `users:(("other,pid=77,fd=7",pid=177,fd=7))`, want: "0"},
		{metadata: `users:(("name \" ,pid=77,fd=7",pid=177,fd=7))`, want: "0"},
		{metadata: `users:(("name \\",pid=77,fd=0))`, want: "1"},
		{metadata: `users:(("users:((\"spoof\",pid=77,fd=7))",pid=177,fd=7))`, want: "0"},
		{metadata: `users:(("other",pid=177,fd=7),("fluent-bit",pid=77,fd=9))`, want: "1"},
		{metadata: `users:(("fluent-bit",pid=77,fd=9),("other",pid=177,fd=7))`, want: "1"},
		{metadata: `users:(("first",pid=177,fd=7),("second",pid=277,fd=9))`, want: "0"},
		{metadata: "users: ( ( \"name with space\" , pid=77 , fd=9 ) , ( \"other\",pid=177,fd=7 ) ) \t", want: "1"},
		{metadata: `users:(("",pid=77,fd=9))`, want: "1"},
	}
	rows := make([]string, len(cases))
	for i, test := range cases {
		rows[i] = prefix + test.metadata
	}
	results := runSyntheticPublisherSocketOwners(t, "77", rows)
	for i, test := range cases {
		if results[i] != test.want {
			t.Errorf("case %d: ownership=%s want=%s", i, results[i], test.want)
		}
	}
}

func TestMimirPublisherSocketOwnersRejectsIncompleteAmbiguousAndOversizedMetadata(t *testing.T) {
	const valid = `users:(("fluent-bit",pid=77,fd=9))`
	const prefix = "0 0 [2001:db8::80]:41000 [2001:db8::10]:3100 "
	metadata := []string{
		"", "users:", "users:()", "users:((fluent-bit,pid=77,fd=9))",
		`users:(("unterminated,pid=77,fd=9))`, `users:(("trailing escape\`,
		`users:(("name",pid=77,fd=9)`, `users:(("name",pid=77,fd=9)),`,
		`users:(("name",pid=77,fd=9),)`, `users:(("name",pid=77,fd=9)("other",pid=177,fd=7))`,
		`users:(("name",pid=077,fd=9))`, `users:(("name",pid=0,fd=9))`,
		`users:(("name",pid=-77,fd=9))`, `users:(("name",pid=77x,fd=9))`,
		`users:(("name",pid=77,fd=-9))`, `users:(("name",pid=77,fd=09))`,
		`users:(("name",pid=77,fd=9,uid=7))`, `users:(("name",fd=9,pid=77))`,
		`users:(("name",pid=77,pid=177,fd=9))`, `users:(("name",pid=77,fd=))`,
		valid + " " + valid, valid + " unrecognized-tail",
		"not-" + valid,
		`users:(("` + strings.Repeat("x", 4096) + `",pid=77,fd=9))`,
		"users:(" + strings.Repeat(`("other",pid=177,fd=7),`, 64) + `("fluent-bit",pid=77,fd=9))`,
		`users:(("name",pid=999999999999999999999,fd=9))`,
	}
	rows := make([]string, len(metadata))
	for i, value := range metadata {
		rows[i] = prefix + value
	}
	results := runSyntheticPublisherSocketOwners(t, "77", rows)
	for i, result := range results {
		if result != "-1" {
			t.Errorf("case %d: incomplete ownership became observable: %s", i, result)
		}
	}
}

func TestMimirPublisherSocketOwnersUsesExactPidStringAndBoundsTupleCount(t *testing.T) {
	const pid = "9999999999999999999"
	results := runSyntheticPublisherSocketOwners(t, pid, []string{
		`users:(("name",pid=9999999999999999999,fd=9))`,
		`users:(("other",pid=9999999999999999998,fd=9))`,
		"users:(" + strings.Repeat(`("other",pid=177,fd=7),`, 63) + `("name",pid=9999999999999999999,fd=9))`,
	})
	for i, want := range []string{"1", "0", "1"} {
		if results[i] != want {
			t.Errorf("case %d: exact string ownership=%s want=%s", i, results[i], want)
		}
	}
	for _, invalid := range []string{"", "077", "0", "-77", "77x", strings.Repeat("9", 21)} {
		if got := runSyntheticPublisherSocketOwners(t, invalid, []string{`users:(("name",pid=77,fd=9))`})[0]; got != "-1" {
			t.Errorf("invalid live PID %q became observable", invalid)
		}
	}
}

func TestMimirPublisherSocketOwnersRejectsEmbeddedRecordBoundaries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, boundary := range []string{"\n", "\r"} {
		line := `users:(("name` + boundary + `",pid=77,fd=9))`
		program := mimirPublisherSocketOwnersAwk + "BEGIN {print publisherOwnsSocket(" + strconv.Quote(line) + ",\"77\")}"
		output, err := exec.CommandContext(ctx, "awk", program).CombinedOutput()
		if err != nil || string(output) != "-1\n" {
			t.Fatalf("embedded record boundary did not remain unknown: err=%v result=%q", err, output)
		}
	}
}
