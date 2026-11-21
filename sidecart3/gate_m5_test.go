package sidecart3

// M5's sweeps on server's side (TEST-M5-02; sidecar's gate_m5.go). None of M5's own rows has a registered builder, so
// nothing of them runs through this package's Run: each T3 run is where its owner wrote it, in api (FS-25's dns
// route), tlsedgehost (FS-34, the kill switch through a loss), cli/dns (FS-42's dns step, FS-52), socketshost,
// sidecarrestore, quotahost, model and controller, and the hold cases, which keep the convention here, are
// TEST-M10-06's. local/sidecar-gate's TestGateSweepsNameServerTestsDeclaredUnderTheirTags holds the rows' entries;
// this holds the items', so one that is renamed, or whose file's build tags drift from its entry's, fails here before
// its item reads missing at the gate. It also holds the local stack's Pebble to §22.9's 5% bad nonces, which the ACME
// item says is the stack's own and this sweep never sets. It reads source files and needs no store.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestGateM5ItemsNameServerTestsDeclaredUnderTheirTags(t *testing.T) {
	serverDirectory, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	checked := []string{}
	for _, gate := range []string{"M5-in", "M5"} {
		for _, row := range fault.GateRows(fault.DefaultCatalog(), gate) {
			if row.Item == "" {
				continue
			}
			for _, test := range row.Tests {
				if test.Repo != "server" {
					continue
				}
				for _, err := range fault.GateTestDeclared(serverDirectory, test) {
					t.Errorf("%s's %q: %v", gate, row.Item, err)
				}
				for _, name := range test.Names {
					checked = append(checked, test.Package+" "+name)
				}
			}
		}
	}
	for _, want := range []string{
		"./sidecart3 TestGateT3RunsTheNightlySeeds",
		"./sidecart3 TestT3EdgeRecordsOfServersTlsStoreAreScanned",
		"./sidecart3 TestT3EveryEdgeClassCatchesItsLeak",
		"./tlsedgehost TestT3TlsedgePassthroughEndsOnTheDeviceKey",
		"./tlsedgehost TestT3TlsedgeKillSwitchEndsNewAndOpenConnections",
		"./socketshost TestT3FindIsOneScriptWithPostgresRefused",
		"./cli/dns TestDnsT3BothNsAnswerAlikeAtOneSerial",
		"./cli/dns TestDnsConformanceUnboundValidatesAgainstTheKsk",
		"./cli/mxedge TestS7AbuseMailWithNoSniIsDroppedUnreadAndKeptNowhere",
		"./mxedgehost TestRunHostOnThePrivateStores",
		"./controller TestAbuseComposedPathFromTheFormToASecondReviewersGrant",
		"./model TestAbuseRetentionSkipsAHeldNetworkUntilItsHoldEnds",
		"./model TestLegalHoldRefusesUntilEnabled",
		"./quotahost TestTlsMeterT3MoneyPath",
		"./monitor TestFs50M5ConditionsAt0200ReachTheRota",
		"./local/sidecar-stack/pebble TestPebbleServesItsDirectoryAndRoots",
	} {
		if !slices.Contains(checked, want) {
			t.Errorf("%q was not checked; checked %v", want, checked)
		}
	}
}

// A test on rows a pending migration group holds skips without the group linked, and a skip reads open at the gate, so
// each such entry of M5's rows and items carries sidecar_pending: every server entry on the stores but socketshost's,
// whose world needs no pending table, and this package's convention entries, which carry it already.
func TestGateM5ServerEntriesOnTheStoresLinkThePendingGroups(t *testing.T) {
	entries := 0
	for _, row := range fault.GateRows(fault.DefaultCatalog(), "M5") {
		for _, test := range row.Tests {
			if test.Repo != "server" || !test.Stores || test.Package == "./socketshost" {
				continue
			}
			entries += 1
			if !slices.Contains(strings.Split(test.Tags, ","), "sidecar_pending") {
				t.Errorf("%s%s's %v run on the stores with tags %q", row.Row, row.Item, test.Names, test.Tags)
			}
		}
	}
	if entries < 20 {
		t.Errorf("M5 names %d server entries on the stores", entries)
	}
}

var gateM5NonceRejectDefaultPattern = regexp.MustCompile(`(?m)^\s*NonceRejectPercent:\s*(\d+),`)

// The local stack's Pebble refuses 5 good nonces in 100 unless told otherwise, and its compose file tells it 5:
// §22.9's "Pebble with 5% bad nonces" on T3, as sidecartest.StartPebble's default is on T2.
func TestGateM5LocalStackPebbleRunsAtFivePercentBadNonces(t *testing.T) {
	directory := filepath.Join("..", "local", "sidecar-stack", "pebble")
	source, err := os.ReadFile(filepath.Join(directory, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if match := gateM5NonceRejectDefaultPattern.FindSubmatch(source); match == nil || string(match[1]) != "5" {
		t.Errorf("the local stack's pebble defaults to rejecting %q%% of good nonces, §22.9's 5", match)
	}
	if !strings.Contains(string(source), `"PEBBLE_WFE_NONCEREJECT": strconv.Itoa(settings.NonceRejectPercent)`) {
		t.Error("the local stack's pebble no longer hands its rate to Pebble")
	}
	compose, err := os.ReadFile(filepath.Join(directory, "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(compose), "\n")
	flagged := false
	for index, line := range lines {
		if strings.TrimSpace(line) != `- "-nonce-reject"` {
			continue
		}
		flagged = true
		if index+1 == len(lines) || strings.TrimSpace(lines[index+1]) != `- "5"` {
			t.Errorf("the compose file's -nonce-reject is followed by %q, §22.9's 5", lines[min(index+1, len(lines)-1)])
		}
	}
	if !flagged {
		t.Error("the compose file passes no -nonce-reject, so the rate is the binary's default alone")
	}
}
