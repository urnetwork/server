// The operator's settlement v2 epoch commands parse with the real usage and
// refuse what they cannot record.
package main

import (
	"fmt"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/controller"
)

// §6.4 (an explicit command naming the epoch): v2-unrecoverable and
// v2-rebuild each select their command with the epoch and who runs it; a
// malformed epoch is refused before anything is recorded, and without
// settlement v2 each command is refused.
func TestStV2EpochCommands(t *testing.T) {
	controller.SetStConfig(&controller.StConfig{Enabled: true})
	defer controller.SetStConfig(nil)
	refused := func(run func(docopt.Opts), opts docopt.Opts) string {
		var reason string
		func() {
			defer func() { reason = fmt.Sprint(recover()) }()
			run(opts)
		}()
		return reason
	}
	for command, run := range map[string]func(docopt.Opts){"v2-unrecoverable": stV2Unrecoverable, "v2-rebuild": stV2Rebuild} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"st", command, "--epoch=5", "--by=operator@example"}, "synthetic")
		if err != nil {
			t.Fatal(err)
		}
		selected, _ := opts.Bool(command)
		epoch, _ := opts.String("--epoch")
		by, _ := opts.String("--by")
		if !selected || epoch != "5" || by != "operator@example" {
			t.Fatalf("%s parsed as %v", command, opts)
		}
		if reason := refused(run, opts); reason != "st ledger v2: not configured" {
			t.Fatalf("%s without settlement v2: %s", command, reason)
		}
		if reason := refused(run, docopt.Opts{"--epoch": "five", "--by": "operator@example"}); reason != `bad --epoch "five": strconv.ParseUint: parsing "five": invalid syntax` {
			t.Fatalf("%s with a malformed epoch: %s", command, reason)
		}
	}
}
