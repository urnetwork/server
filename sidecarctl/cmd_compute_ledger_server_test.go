//go:build sidecar_pending

// `sidecarctl compute promo create` against the env as server reaches it (BAL-M12-13): MG-C1's compute_promo_code on
// the private test stack's Postgres, so this builds under sidecar_pending and runs through .stack/server-test.sh
// (server.DefaultTestEnv), under its WARP_ENV. A code made keeps its keyed hash alone and redeems through the ledger
// under the same key; the env's vault without the ledger's keys, and another env, write nothing.

package main

import (
	"bytes"
	"context"
	"regexp"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
	"github.com/urnetwork/server/sidecarhost"
)

// The ledger's keys for this test: synthetic, test only.
type sidecarctlTestLedgerSecrets struct{}

// A fixed payer key.
func (self sidecarctlTestLedgerSecrets) PayerHashKey() ([]byte, error) {
	return []byte("synthetic sidecarctl payer hash key, test only"), nil
}

// A fixed promotion key.
func (self sidecarctlTestLedgerSecrets) PromoHashKey() ([]byte, error) {
	return []byte("synthetic sidecarctl promo hash key, test only"), nil
}

// The command as the binary runs it against the env, with the secrets given.
func runServerComputePromoCreate(secrets sidecarcontroller.ComputeLedgerSecrets, args ...string) (int, string, string) {
	settings := defaultComputeLedgerCommandSettings()
	if secrets != nil {
		settings.secrets = func() sidecarcontroller.ComputeLedgerSecrets { return secrets }
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	err := runComputePromoCreateWithSettings(args, settings, stdout)
	code := 0
	if err != nil {
		code = 1
		stderr.WriteString(err.Error())
	}
	return code, stdout.String(), stderr.String()
}

// A code made against the env keeps only its keyed hash and redeems under the same key; without the vault's keys, or for
// another env, nothing is written; the same code made twice is refused.
func TestSidecarctlPromoCreate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		env := server.RequireEnv()
		lastDay := time.Now().UTC().AddDate(0, 0, 30).Format(time.DateOnly)
		db := core.NewDb()
		queryInt := func(sql string, args ...any) int64 {
			value := int64(0)
			err := db.Db(ctx, func(conn operator.DbConn) error {
				rows, err := conn.Query(ctx, sql, args...)
				if err != nil {
					return err
				}
				defer rows.Close()
				if rows.Next() {
					if err := rows.Scan(&value); err != nil {
						return err
					}
				}
				return rows.Err()
			})
			if err != nil {
				tb.Fatal(err)
			}
			return value
		}
		count := func() int64 {
			return queryInt(`SELECT count(*) FROM compute_promo_code`)
		}
		line := []string{env, "--campaign=launch", "--value=5", "--cap=1", "--last-day=" + lastDay, "--by=operator.example", "--reason=launch week"}

		// the env's vault holds no ledger keys: nothing is written
		if code, stdout, _ := runServerComputePromoCreate(nil, line...); code != 1 || stdout != "" || count() != 0 {
			tb.Fatalf("without the vault's keys: exit %d, %q, %d codes", code, stdout, count())
		}
		// another env than the process's: nothing is written
		other := append([]string{"another-env"}, line[1:]...)
		if code, _, _ := runServerComputePromoCreate(sidecarctlTestLedgerSecrets{}, other...); code != 1 || count() != 0 {
			tb.Fatalf("another env: exit %d, %d codes", code, count())
		}

		code, stdout, stderr := runServerComputePromoCreate(sidecarctlTestLedgerSecrets{}, line...)
		if code != 0 {
			tb.Fatalf("exit %d: %s", code, stderr)
		}
		match := regexp.MustCompile(`(?m)^code: ([A-Z2-9]{16})$`).FindStringSubmatch(stdout)
		if match == nil || count() != 1 {
			tb.Fatalf("the output %q, %d codes", stdout, count())
		}
		promoCode := match[1]
		// the row keeps the code's keyed hash, never the code
		if 0 < queryInt(`SELECT count(*) FROM compute_promo_code WHERE position($1 IN row_to_json(compute_promo_code)::text) > 0 OR position($2 IN row_to_json(compute_promo_code)::text) > 0`,
			promoCode, string(bytes.ToLower([]byte(promoCode)))) {
			tb.Fatal("compute_promo_code holds the code")
		}

		// it redeems through the ledger under the same key
		ledgerEnv, err := core.NewEnv(&sidecar.ServiceHost{Redis: sidecarhost.NewRedis(nil)}, &core.Grants{Db: true})
		if err != nil {
			tb.Fatal(err)
		}
		networkId := server.NewId()
		answer, err := sidecarcontroller.ComputeRedeemPromo(ledgerEnv, ctx, model.DefaultComputeLedgerSettings(), sidecarctlTestLedgerSecrets{}, &sidecarcontroller.ComputeRedeemRequest{
			NetworkId: networkId, Code: promoCode,
		})
		if err != nil || !answer.Redeemed || answer.Lot.AmountNanoUsd != 5*model.ComputeCreditNanoUsd {
			tb.Fatalf("the code's redemption: %+v, %v", answer, err)
		}
		// the same code made again is taken
		if code, _, stderr := runServerComputePromoCreate(sidecarctlTestLedgerSecrets{}, append(line, "--code="+promoCode)...); code != 1 || count() != 1 {
			tb.Fatalf("the code made twice: exit %d: %s", code, stderr)
		}
	})
}
