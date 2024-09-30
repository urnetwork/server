package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"bringyour.com/bringyour"
	"github.com/jedib0t/go-pretty/v6/progress"
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
			os.Setenv("WARP_ENV", "test")
			os.Setenv("WARP_VERSION", "0.0.1")
			os.Setenv("WARP_SERVICE", "bringyour")
			os.Setenv("WARP_BLOCK", "local")
			os.Setenv("WARP_PORTS", "5080:5080")

			err = createPrivateKey(vaultDir)
			if err != nil {
				return fmt.Errorf("failed to create private key: %w", err)
			}

			pw := progress.NewWriter()
			pw.SetAutoStop(false)
			pw.SetMessageLength(40)
			pw.SetNumTrackersExpected(4)
			pw.SetSortBy(progress.SortByNone)
			pw.SetStyle(progress.StyleDefault)
			pw.SetTrackerLength(25)
			pw.SetTrackerPosition(progress.PositionRight)
			pw.SetUpdateFrequency(time.Millisecond * 200)
			pw.Style().Colors = progress.StyleColorsExample
			pw.Style().Options.PercentFormat = "%4.1f%%"
			pw.Style().Visibility.ETA = false
			pw.Style().Visibility.ETAOverall = false
			pw.Style().Visibility.Percentage = true
			pw.Style().Visibility.Speed = false
			pw.Style().Visibility.SpeedOverall = false
			pw.Style().Visibility.Time = false
			pw.Style().Visibility.TrackerOverall = true
			pw.Style().Visibility.Value = true
			pw.Style().Visibility.Pinned = false

			go pw.Render()

			{
				// starting containers for postgres and redis
				// takes a while, so we use spinners
				// to show progress

				eg, egCtx := errgroup.WithContext(ctx)

				var pgCleanup func() error

				eg.Go(func() (err error) {
					pgCleanup, err = setupPostgres(egCtx, vaultDir, pw)
					if err != nil {
						return fmt.Errorf("failed to setup postgres: %w", err)
					}
					return nil
				})

				var redisCleanup func() error

				eg.Go(func() (err error) {
					redisCleanup, err = setupRedis(egCtx, vaultDir, pw)
					if err != nil {
						return fmt.Errorf("failed to setup redis: %w", err)
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

			servicesGroup, completeRunCtx := errgroup.WithContext(ctx)

			servicesGroup.Go(func() (err error) {
				err = runGoMainProcess(completeRunCtx, "API", pw, filepath.Join(myMainDir, "..", "api"), "-p", "8080")
				if err != nil {
					return fmt.Errorf("failed to run API: %w", err)
				}
				return nil
			})

			servicesGroup.Go(func() (err error) {
				err = runGoMainProcess(completeRunCtx, "Connect", pw, filepath.Join(myMainDir, "..", "connect"), "-p", "7070")
				if err != nil {
					return fmt.Errorf("failed to run API: %w", err)
				}
				return nil
			})

			defer func() {
				sgErr := servicesGroup.Wait()
				err = errors.Join(err, sgErr)
			}()

			<-completeRunCtx.Done()

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
