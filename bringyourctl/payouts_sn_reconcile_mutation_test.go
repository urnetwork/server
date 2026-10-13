package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// A payout ledger with funded wallets and no history. The child never reads
// Circle: without stuck rows it lists nothing and reads only these balances.
type snReconcileTestLedger struct{}

func (snReconcileTestLedger) ListWalletTransactions(context.Context, string, time.Time) ([]controller.CircleTransaction, error) {
	return []controller.CircleTransaction{}, nil
}

func (snReconcileTestLedger) GetWalletTokenBalance(context.Context, string, string) (model.NanoCents, error) {
	return model.UsdToNanoCents(1), nil
}

func (snReconcileTestLedger) ListDeveloperWallets(context.Context) ([]controller.CircleWallet, error) {
	return []controller.CircleWallet{}, nil
}

// A fixture home for the child: copies of the test database and Redis
// resources this test runs with, a synthetic circle.yml, and nothing else.
func snReconcileTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, dir := range []string{"vault", "config", "site"} {
		if err := os.Mkdir(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir string, name string, data []byte) {
		if err := os.WriteFile(filepath.Join(home, dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for dir, resources := range map[string]struct {
		resolver *server.Resolver
		names    []string
	}{
		"vault":  {server.Vault, []string{"pg.yml", "pg_maintenance.yml", "redis.yml"}},
		"config": {server.Config, []string{"db.yml", "db_maintenance.yml", "redis.yml"}},
	} {
		for _, name := range resources.names {
			resource, err := resources.resolver.SimpleResource(name)
			if errors.Is(err, server.ErrResourceNotFound) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			write(dir, name, resource.Bytes())
		}
	}
	write("vault", "circle.yml", []byte("circle:\n  solana_wallet_id: synthetic-sol\n  polygon_wallet_id: synthetic-matic\n"+
		"  solana_usdc_address: synthetic-sol-usdc\n  polygon_usdc_address: synthetic-matic-usdc\n"))
	return home
}

// RC-18, A7: main hands the command line to SnReconcile unchanged. Without
// flags it is a dry run; --send runs the send with its --expect digest, which
// a wrong digest refuses. main exits, so each run is a child process on its
// own test database, whose WARP_HOME, vault and config homes are the fixture
// home and whose payout ledger is synthetic; the reaper drops its database.
func TestSnReconcileMainDispatch(t *testing.T) {
	if args := os.Getenv("SN_RECONCILE_TEST_MAIN_ARGS"); args != "" {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			controller.SetCirclePayoutLedger(snReconcileTestLedger{})
			os.Args = append([]string{"bringyourctl"}, strings.Fields(args)...)
			main()
		})
		t.Fatal("main returned")
	}
	home := snReconcileTestHome(t)
	run := func(args string) (int, string, string) {
		command := exec.Command(os.Args[0], "-test.run=^TestSnReconcileMainDispatch$")
		command.Env = append(os.Environ(), "SN_RECONCILE_TEST_MAIN_ARGS="+args, "WARP_HOME="+home, "WARP_VAULT_HOME="+filepath.Join(home, "vault"),
			"WARP_CONFIG_HOME="+filepath.Join(home, "config"), "WARP_SITE_HOME="+filepath.Join(home, "site"))
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		var exit *exec.ExitError
		if err := command.Run(); err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return command.ProcessState.ExitCode(), stdout.String(), stderr.String()
	}
	code, stdout, stderr := run("payouts sn-reconcile --json")
	var report controller.SnReconcileReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || code != 0 || report.Mode != "dry_run" {
		t.Fatal("dry run", code, err, stdout, stderr)
	}
	code, stdout, stderr = run("payouts sn-reconcile --send --expect=" + strings.Repeat("0", 64))
	if code != 1 || !strings.Contains(stdout, ", mode send,") || !strings.Contains(stderr, controller.ErrSnReconcileDigestMismatch.Error()+"\n") {
		t.Fatal("send with a wrong digest", code, stdout, stderr)
	}
}
