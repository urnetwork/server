package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The synthetic payout ledger, whose balance read also tries to journal a row
// and records in `path` whether PostgreSQL wrote it or refused it. Every run
// reads the balances, the dry run before it returns and the send before it
// compares the digest.
type snReconcileWriteProbeLedger struct {
	snReconcileTestLedger
	path string
}

func (self snReconcileWriteProbeLedger) GetWalletTokenBalance(ctx context.Context, walletId string, tokenAddress string) (model.NanoCents, error) {
	outcome := "written"
	func() {
		defer func() {
			if r := recover(); r != nil {
				outcome = fmt.Sprint(r)
			}
		}()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details)
				VALUES($1,$2,'sn_reconcile_write_probe',NULL)`, server.NewId(), server.NewId()))
		})
	}()
	if err := os.WriteFile(self.path, []byte(outcome), 0o600); err != nil {
		return 0, err
	}
	return self.snReconcileTestLedger.GetWalletTokenBalance(ctx, walletId, tokenAddress)
}

// SN-RECONCILE.md §2 (RC-18): the dry run writes nothing, whatever its census
// path does. Its process is read only, so PostgreSQL refuses a journal row
// written during the run; --send runs in a writable process. Each run is a
// child process on its own test database, as in TestSnReconcileMainDispatch.
func TestSnReconcileDryRunProcessIsReadOnly(t *testing.T) {
	if args := os.Getenv("SN_RECONCILE_TEST_PROBE_ARGS"); args != "" {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			controller.SetCirclePayoutLedger(snReconcileWriteProbeLedger{path: os.Getenv("SN_RECONCILE_TEST_PROBE_PATH")})
			os.Args = append([]string{"bringyourctl"}, strings.Fields(args)...)
			main()
		})
		t.Fatal("main returned")
	}
	home := snReconcileTestHome(t)
	probe := func(args string) string {
		path := filepath.Join(t.TempDir(), "probe")
		command := exec.Command(os.Args[0], "-test.run=^TestSnReconcileDryRunProcessIsReadOnly$")
		command.Env = append(os.Environ(), "SN_RECONCILE_TEST_PROBE_ARGS="+args, "SN_RECONCILE_TEST_PROBE_PATH="+path, "WARP_HOME="+home,
			"WARP_VAULT_HOME="+filepath.Join(home, "vault"), "WARP_CONFIG_HOME="+filepath.Join(home, "config"), "WARP_SITE_HOME="+filepath.Join(home, "site"))
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		var exit *exec.ExitError
		if err := command.Run(); err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		outcome, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("the run did not read a balance", err, stdout.String(), stderr.String())
		}
		return string(outcome)
	}
	if outcome := probe("payouts sn-reconcile --json"); !strings.Contains(outcome, "read-only transaction") {
		t.Fatal("the dry run's process wrote", outcome)
	}
	if outcome := probe("payouts sn-reconcile --send --expect=" + strings.Repeat("0", 64)); outcome != "written" {
		t.Fatal("the send's process refused a write", outcome)
	}
}
