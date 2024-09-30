package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"

	"bringyour.com/bringyour"
	"github.com/pterm/pterm"
	"github.com/urfave/cli/v2"
	"golang.org/x/sync/errgroup"
)

func main() {
	app := &cli.App{
		Name: "measure-throughput",
		Action: func(c *cli.Context) (err error) {

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
			defer cancel()

			myMainDir, err := getMyMainDir()
			if err != nil {
				return fmt.Errorf("failed to get main dir: %w", err)
			}

			fmt.Println("my main dir:", myMainDir)

			vaultDir, err := os.MkdirTemp("", "vault")
			if err != nil {
				return fmt.Errorf("failed to create temp dir: %w", err)
			}

			stderrFileName := filepath.Join(vaultDir, "stderr.log")
			stderr, err := os.OpenFile(stderrFileName, os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return fmt.Errorf("failed to create stderr file: %w", err)
			}

			defer stderr.Close()

			realStderr := os.Stderr

			os.Stderr = stderr

			bringyour.SetLogger(log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile))

			// fmt.Println("replaced stderr")
			defer func() {
				bringyour.Logger()
				if err != nil {
					stderr.Seek(0, 0)
					io.Copy(realStderr, stderr)
				}
			}()

			flag.Set("logtostderr", "false")    // Log to standard error instead of files
			flag.Set("stderrthreshold", "WARN") // Set the threshold level for logging to stderr
			flag.Set("v", "0")                  // Set the verbosity level to 1

			flag.Parse()

			defer func() {
				os.RemoveAll(vaultDir)
			}()

			os.Setenv("WARP_VAULT_HOME", vaultDir)

			err = createPrivateKey(vaultDir)
			if err != nil {
				return fmt.Errorf("failed to create private key: %w", err)
			}

			// starting contaiers for postgres and redis
			// takes a while, so we use spinners
			// to show progress
			multi := pterm.DefaultMultiPrinter
			postgresWriter := multi.NewWriter()
			redisWriter := multi.NewWriter()
			apiWriter := multi.NewWriter()

			multi.Start()
			defer multi.Stop()

			{

				eg, egCtx := errgroup.WithContext(ctx)

				var pgCleanup func() error

				eg.Go(func() (err error) {
					pgCleanup, err = setupPostgres(egCtx, vaultDir, postgresWriter)
					if err != nil {
						return fmt.Errorf("failed to setup postgres: %w", err)
					}
					return nil
				})

				var redisCleanup func() error

				eg.Go(func() (err error) {
					redisCleanup, err = setupRedis(egCtx, vaultDir, redisWriter)
					if err != nil {
						return fmt.Errorf("failed to setup redis: %w", err)
					}
					return nil
				})

				eg.Go(func() (err error) {
					err = runGoMainProcess(egCtx, "API", apiWriter, filepath.Join(myMainDir, "..", "api"))
					if err != nil {
						return fmt.Errorf("failed to run API: %w", err)
					}
					return nil
				})

				err = eg.Wait()
				defer runIfNotNil(pgCleanup)
				defer runIfNotNil(redisCleanup)
				if err != nil {
					return fmt.Errorf("failed to setup services: %w", err)
				}

			}

			// {
			// 	eg, egCtx := errgroup.WithContext(ctx)

			// }

			return nil
		},
	}
	app.RunAndExitOnError()
}

func runIfNotNil(fn func() error) error {
	if fn != nil {
		return fn()
	}
	return nil
}
