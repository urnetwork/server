// The operator's read-only sweep status parses with the real usage and prints
// one JSON object.
package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026/controller"
)

// st v1-sweep-status selects its command alone. With the sweep refused it
// prints configured false and the refusal's reason, the config_error that
// runbook D3 reads.
func TestStV1SweepStatusCommand(t *testing.T) {
	opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"st", "v1-sweep-status"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if selected, _ := opts.Bool("v1-sweep-status"); !selected {
		t.Fatalf("parsed as %v", opts)
	}
	for _, other := range []string{"v2-activation", "v2-activation-clear", "v2-rebuild", "v2-unrecoverable", "finalize"} {
		if selected, _ := opts.Bool(other); selected {
			t.Fatalf("st v1-sweep-status parsed with %s", other)
		}
	}
	reason := "st.yml mainnet sweep keys refused, the sweep is disabled: v1_sweep_recovery_epochs [4] must ascend strictly below the sweep epoch 4"
	controller.SetStConfig(&controller.StConfig{Enabled: true, V1SweepConfigError: reason})
	defer controller.SetStConfig(nil)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	func() {
		defer func() { os.Stdout = stdout }()
		stV1SweepStatus(opts)
	}()
	write.Close()
	out, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(out, &status); err != nil {
		t.Fatalf("not one JSON object: %q", out)
	}
	if status["configured"] != false || status["config_error"] != reason {
		t.Fatalf("status %s", out)
	}
}
