// The operator's settlement v2 epoch commands parse with the real usage and
// refuse what they cannot record.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// §6.4 (an explicit command naming the epoch), §8.6: v2-unrecoverable,
// v2-rebuild and v2-rebuild-withdraw each select their command alone with the
// epoch and who runs it; a malformed epoch is refused before anything is
// recorded, and without settlement v2 each command is refused.
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
	commands := map[string]func(docopt.Opts){"v2-unrecoverable": stV2Unrecoverable, "v2-rebuild": stV2Rebuild, "v2-rebuild-withdraw": stV2RebuildWithdraw}
	for command, run := range commands {
		opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"st", command, "--epoch=5", "--by=operator@example"}, "synthetic")
		if err != nil {
			t.Fatal(err)
		}
		for other := range commands {
			if selected, _ := opts.Bool(other); selected != (other == command) {
				t.Fatalf("%s parsed with %s %t", command, other, selected)
			}
		}
		epoch, _ := opts.String("--epoch")
		by, _ := opts.String("--by")
		if epoch != "5" || by != "operator@example" {
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

// §8.6: main dispatches st v2-rebuild-withdraw to the withdrawal, which clears
// epoch 5's pending rebuild request and prints who made it and who withdrew
// it. A second withdrawal is refused, which main reports by panicking with the
// reason, as for every v2 epoch command.
func TestStV2EpochCommandsDispatchTheRebuildWithdrawal(t *testing.T) {
	if _, err := server.Version(); err != nil {
		t.Setenv("WARP_VERSION", "0.0.0")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cfg := &controller.StConfig{
			Enabled: true, ChainId: 945, NoId: 1,
			ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"),
			SettlementV2:    common.HexToAddress("0x2000000000000000000000000000000000000006"),
			VaultV2:         common.HexToAddress("0x2000000000000000000000000000000000000007"),
		}
		controller.SetStConfig(cfg)
		defer controller.SetStConfig(nil)
		key := cfg.DeploymentKey()
		if err := model.RecordStLedgerV2EpochRows(ctx, key, 1, 5, []string{}); err != nil {
			t.Fatal(err)
		}
		if requested, err := model.RequestStLedgerV2EpochRebuild(ctx, key, 1, 5, "rebuilder@example"); !requested || err != nil {
			t.Fatalf("request: %t %v", requested, err)
		}
		run := func(args ...string) (out string, reason string) {
			defer func(args []string) { os.Args = args }(os.Args)
			os.Args = append([]string{"bringyourctl", "st"}, args...)
			out = captureStdout(t, func() {
				defer func() {
					if r := recover(); r != nil {
						reason = fmt.Sprint(r)
					}
				}()
				main()
			})
			return out, reason
		}

		if out, reason := run("v2-rebuild-withdraw", "--epoch=5", "--by=withdrawer@example"); out != "epoch 5 rebuild request by rebuilder@example withdrawn by withdrawer@example\n" || reason != "" {
			t.Fatalf("withdrawal printed %q, refused: %s", out, reason)
		}
		if r := model.GetStLedgerV2EpochRows(ctx, key, 1, 5, 5)[5]; r.RebuildRequestedBy != "" {
			t.Fatalf("epoch 5 record %+v", r)
		}
		if out, reason := run("v2-rebuild-withdraw", "--epoch=5", "--by=withdrawer@example"); out != "" || reason != controller.ErrStLedgerV2RebuildWithdrawRefused.Error()+": epoch 5 has no pending rebuild request" {
			t.Fatalf("second withdrawal printed %q, refused: %s", out, reason)
		}
	})
}

// §8.6: a refused v2-rebuild-withdraw exits non-zero with its reason: main
// panics, and Go exits 2. main runs in a child process, without settlement v2.
func TestStV2EpochCommandsRefusedWithdrawalExitsNonZero(t *testing.T) {
	if os.Getenv("ST_V2_EPOCH_COMMANDS_CHILD") == "1" {
		controller.SetStConfig(&controller.StConfig{Enabled: true})
		os.Args = []string{"bringyourctl", "st", "v2-rebuild-withdraw", "--epoch=5", "--by=withdrawer@example"}
		main()
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestStV2EpochCommandsRefusedWithdrawalExitsNonZero$")
	child.Env = append(os.Environ(), "ST_V2_EPOCH_COMMANDS_CHILD=1", "WARP_VERSION=0.0.0")
	var stderr bytes.Buffer
	child.Stderr = &stderr
	var exit *exec.ExitError
	if err := child.Run(); !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(stderr.String(), "panic: st ledger v2: not configured") {
		t.Fatalf("child: %v\n%s", err, stderr.String())
	}
}
