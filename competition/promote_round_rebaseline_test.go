package competition

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPromoteRoundRebaselineScriptIsFailClosed(t *testing.T) {
	const path = "promote-round-rebaseline.sh"
	if output, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %s: %s", err, output)
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source := string(bytes)
	required := []string{
		`[ "$(id -u)" -eq 0 ] || die "promotion must run as root"`,
		`((keys | sort) == $keys)`,
		`[ "$declared_attempt_directory" = "$attempt_directory" ]`,
		`"$PROMOTE_CONTAINMENT"`,
		`--evaluation-dir "$attempt_directory"`,
		`((.score.gates | keys | sort) == $score_gate_keys)`,
		`rebaseline_evaluation_sha256:$result_sha`,
		`containment_promotion_sha256:$containment_sha`,
		`.rebaseline_manifest_sha256 = $marker_sha`,
		`taskset -c "$management_cpus" "$self_check" --json`,
		`.rebaseline_round_id == $round_id`,
		`([.checks[]] | all)`,
	}
	for _, expected := range required {
		if !strings.Contains(source, expected) {
			t.Errorf("promotion script is missing %q", expected)
		}
	}
	for _, forbidden := range []string{"round_seed_hex", "seed_key_base64", "bearer_token"} {
		if strings.Contains(source, forbidden) {
			t.Errorf("promotion script references secret field %q", forbidden)
		}
	}
}
