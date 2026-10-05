// Command stnativefees settles one retained operator transaction from actual
// native proof execution and independently provisioned denomination authority.
// It has no signing, broadcasting, RPC or imported verifier-report option.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/urfoundation/sn/nativefee"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, controller.SettleNativeTransactionFee)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type settleNativeFee func(context.Context, server.Id, nativefee.Reference, string, time.Duration) (*model.StTransactionNativeFeeSettlement, error)

func run(ctx context.Context, args []string, stdout io.Writer, settle settleNativeFee) error {
	if ctx == nil || settle == nil || len(args) == 0 || args[0] != "settle" {
		return errors.New("usage: stnativefees settle --intent ID --request ABSOLUTE_PATH --request-sha256 sha256:DIGEST --transaction 0xHASH [--budget 5m]")
	}
	flags := flag.NewFlagSet("stnativefees settle", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	intent := flags.String("intent", "", "original retained intent UUID")
	path := flags.String("request", "", "original native proof request")
	digest := flags.String("request-sha256", "", "exact request pin")
	transaction := flags.String("transaction", "", "original signed transaction hash")
	budget := flags.Duration("budget", 5*time.Minute, "complete proof and local settlement budget")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *budget < time.Minute || *budget > 15*time.Minute {
		return errors.New("native fee settlement requires no positional arguments and a 60s–15m budget")
	}
	intentId, err := server.ParseId(*intent)
	if err != nil || intentId == (server.Id{}) {
		return errors.New("native fee settlement requires its original nonzero intent UUID")
	}
	request := nativefee.Reference{Path: *path, Sha256: *digest}
	if err := request.Validate(); err != nil {
		return err
	}
	hash, err := hex.DecodeString(strings.TrimPrefix(*transaction, "0x"))
	if err != nil || len(hash) != 32 || !strings.HasPrefix(*transaction, "0x") || *transaction != strings.ToLower(*transaction) || *transaction == "0x"+strings.Repeat("0", 64) {
		return errors.New("native fee settlement requires exact canonical transaction hash")
	}
	owner, cancel := context.WithTimeout(ctx, *budget)
	defer cancel()
	if err := owner.Err(); err != nil {
		return err
	}
	result, err := settle(owner, intentId, request, *transaction, *budget)
	if err != nil {
		return err
	}
	if result == nil {
		return errors.New("native fee settlement returned no durable result")
	}
	// A lost stdout acknowledgment is recovered by exact idempotent replay;
	// it cannot revoke a completed durable original settlement.
	return json.NewEncoder(stdout).Encode(result)
}
