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
	"time"

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

// SV-9 (operator path, §8.6): st v2-activation and st v2-activation-clear each
// select their command alone, the clear with every recorded value and who runs
// it; a malformed number is refused before anything is read, and without a
// selected deployment the show reports no armed latch and the clear is refused.
func TestStLedgerV2ActivationCommands(t *testing.T) {
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
	const vault, settlement = "0x00000000000000000000000000000000000000a2", "0x00000000000000000000000000000000000000a1"
	clearArgs := []string{"st", "v2-activation-clear", "--no_id=1", "--first_epoch=5", "--vault_v2_address=" + vault, "--settlement_v2_address=" + settlement, "--by=operator@example"}
	for command, args := range map[string][]string{"v2-activation": {"st", "v2-activation"}, "v2-activation-clear": clearArgs} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, args, "synthetic")
		if err != nil {
			t.Fatal(err)
		}
		for _, other := range []string{"v2-activation", "v2-activation-clear", "v2-unrecoverable", "v2-rebuild", "v2-rebuild-withdraw"} {
			if selected, _ := opts.Bool(other); selected != (other == command) {
				t.Fatalf("%s parsed with %s %t", command, other, selected)
			}
		}
	}
	opts, err := docopt.ParseArgs(bringyourctlUsage, clearArgs, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	for option, want := range map[string]string{"--no_id": "1", "--first_epoch": "5", "--vault_v2_address": vault, "--settlement_v2_address": settlement, "--by": "operator@example"} {
		if value, _ := opts.String(option); value != want {
			t.Fatalf("clear parsed %s as %q", option, value)
		}
	}
	if reason := refused(stV2ActivationClear, opts); reason != "st: not configured (st.yml missing or disabled)" {
		t.Fatalf("clear without a deployment: %s", reason)
	}
	for option, reason := range map[string]string{
		"--no_id":       `bad --no_id "one": strconv.ParseUint: parsing "one": invalid syntax`,
		"--first_epoch": `bad --first_epoch "one": strconv.ParseUint: parsing "one": invalid syntax`,
	} {
		malformed := docopt.Opts{"--no_id": "1", "--first_epoch": "5", "--vault_v2_address": vault, "--settlement_v2_address": settlement, "--by": "operator@example"}
		malformed[option] = "one"
		if got := refused(stV2ActivationClear, malformed); got != reason {
			t.Fatalf("clear with a malformed %s: %s", option, got)
		}
	}
	if out := captureStdout(t, func() { stV2Activation(docopt.Opts{}) }); out != `{"deployment_key":"","armed":false}`+"\n" {
		t.Fatalf("show without a deployment printed %q", out)
	}
}

// SV-9 (operator path, §8.6): main dispatches st v2-activation, which prints
// the armed latch with the recorded values, and st v2-activation-clear, which
// is refused unless it names them exactly, and then clears the record and
// prints who cleared it. A refusal is reported by panicking with the reason,
// as for every v2 command.
func TestStLedgerV2ActivationCommandsShowAndClearTheRecord(t *testing.T) {
	if _, err := server.Version(); err != nil {
		t.Setenv("WARP_VERSION", "0.0.0")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const vault, settlement = "0x2000000000000000000000000000000000000007", "0x2000000000000000000000000000000000000006"
		cfg := &controller.StConfig{
			Enabled: true, ChainId: 945, NoId: 1,
			ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"),
			SettlementV2:    common.HexToAddress(settlement),
			VaultV2:         common.HexToAddress(vault),
		}
		controller.SetStConfig(cfg)
		defer controller.SetStConfig(nil)
		key := cfg.DeploymentKey()
		if err := model.RecordStLedgerV2Activation(ctx, key, &model.StLedgerV2Activation{NoId: 1, FirstEpoch: 5, VaultV2Address: vault, SettlementV2Address: settlement}); err != nil {
			t.Fatal(err)
		}
		recorded := model.GetStLedgerV2Activation(ctx, key)
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

		armed := fmt.Sprintf(`{"deployment_key":"%s","armed":true,"no_id":1,"first_epoch":5,"vault_v2_address":"%s","settlement_v2_address":"%s","recorded_at":"%s"}`+"\n",
			key, vault, settlement, recorded.CreateTime.Format(time.RFC3339Nano))
		if out, reason := run("v2-activation"); out != armed || reason != "" {
			t.Fatalf("show printed %q, refused: %s", out, reason)
		}
		clear := func(firstEpoch string) (string, string) {
			return run("v2-activation-clear", "--no_id=1", "--first_epoch="+firstEpoch, "--vault_v2_address="+vault, "--settlement_v2_address="+settlement, "--by=operator@example")
		}
		if out, reason := clear("6"); out != "" || reason != controller.ErrStLedgerV2ActivationKept.Error()+": no_id, first_epoch, vault_v2_address and settlement_v2_address must name deployment "+string(key)+"'s record exactly" {
			t.Fatalf("a clear naming another first epoch printed %q, refused: %s", out, reason)
		}
		if again := model.GetStLedgerV2Activation(ctx, key); again == nil {
			t.Fatal("a refused clear removed the record")
		}
		if out, reason := clear("5"); out != "activation of pool 1 from epoch 5 cleared by operator@example\n" || reason != "" {
			t.Fatalf("the exact clear printed %q, refused: %s", out, reason)
		}
		if out, reason := run("v2-activation"); out != fmt.Sprintf(`{"deployment_key":"%s","armed":false}`+"\n", key) || reason != "" {
			t.Fatalf("show after the clear printed %q, refused: %s", out, reason)
		}
		if out, reason := clear("5"); out != "" || reason != controller.ErrStLedgerV2ActivationKept.Error()+": deployment "+string(key)+" has none recorded" {
			t.Fatalf("a second clear printed %q, refused: %s", out, reason)
		}
	})
}
