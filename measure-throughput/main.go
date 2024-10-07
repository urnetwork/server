package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"bringyor.com/measure-throughput/bwestimator"
	"bringyor.com/measure-throughput/clientdevice"
	"bringyor.com/measure-throughput/datasource"
	"bringyor.com/measure-throughput/healthcheck"
	"bringyor.com/measure-throughput/jwtutil"
	"bringyour.com/bringyour"
	"github.com/jedib0t/go-pretty/v6/progress"
	md "github.com/nao1215/markdown"
	"github.com/urfave/cli/v2"
	"golang.org/x/sync/errgroup"
)

func main() {

	cfg := struct {
		reportFile     string
		logTCP         bool
		useMulticlient bool
	}{}
	app := &cli.App{
		Name: "measure-throughput",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "report-file",
				Destination: &cfg.reportFile,
				EnvVars:     []string{"REPORT_FILE"},
			},
			&cli.BoolFlag{
				Name:        "log-tcp",
				Destination: &cfg.logTCP,
				EnvVars:     []string{"LOG_TCP"},
			},
			&cli.BoolFlag{
				Name:        "use-multiclient",
				Destination: &cfg.useMulticlient,
				EnvVars:     []string{"USE_MULTICLIENT"},
			},
		},
		Action: func(c *cli.Context) (err error) {

			fmt.Println("using multiclient:", cfg.useMulticlient)

			f, err := os.Create("/tmp/measure-throughput.log")
			if err != nil {
				return fmt.Errorf("failed to create log file: %w", err)
			}

			defer f.Close()

			slog.SetDefault(
				slog.New(
					slog.NewTextHandler(f,
						&slog.HandlerOptions{
							AddSource: true,
							Level:     slog.LevelDebug,
						},
					),
				),
			)

			// flag.Set("logtostderr", "false")    // Log to standard error instead of files
			// flag.Set("stderrthreshold", "WARN") // Set the threshold level for logging to stderr
			// flag.Set("v", "2")                  // Set the verbosity level to 1

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill, syscall.SIGPIPE)
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

			fmt.Println("stderr file:", stderrFileName)

			defer stderr.Close()

			realStderr := os.Stderr

			os.Stderr = stderr

			bringyour.SetLogger(log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile))

			defer func() {
				bringyour.Logger()
				if err != nil {
					stderr.Seek(0, 0)
					io.Copy(realStderr, stderr)
				}
			}()

			// defer func() {
			// 	os.RemoveAll(vaultDir)
			// }()

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
			pw.SetNumTrackersExpected(7)
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
				err = datasource.Run(completeRunCtx, ":15080", pw, 512*1024, time.Second*60)
				if err != nil {
					return fmt.Errorf("failed to run API: %w", err)
				}
				return nil
			})

			servicesGroup.Go(func() (err error) {
				err = datasource.RunSink(completeRunCtx, ":15081", pw)
				if err != nil {
					return fmt.Errorf("failed to run API: %w", err)
				}
				return nil
			})

			servicesGroup.Go(func() (err error) {
				err = runGoMainProcess(
					completeRunCtx,
					"API",
					pw,
					filepath.Join(myMainDir, "..", "api"),
					vaultDir,
					"-p",
					"8080",
				)
				if err != nil {
					slog.Error("failed to run API", "err", err)
					return fmt.Errorf("failed to run API: %w", err)
				}
				return nil
			})

			servicesGroup.Go(func() (err error) {
				err = runGoMainProcess(
					completeRunCtx,
					"Connect",
					pw,
					filepath.Join(myMainDir, "..", "connect"),
					vaultDir,
					"-p",
					"7070",
				)
				if err != nil {
					slog.Error("failed to run Connect", "err", err)
					return fmt.Errorf("failed to run Connect: %w", err)
				}
				return nil
			})

			// time.Sleep(time.Second * 3)

			_, err = setupNewNetwork(completeRunCtx, pw)
			if err != nil {
				return fmt.Errorf("failed to setup network: %w", err)
			}

			providerJWT, err := authDevice(completeRunCtx, userAuth, userPassword)
			if err != nil {
				return fmt.Errorf("failed to authenticate device: %w", err)
			}

			providerID, err := jwtutil.ParseClientID(providerJWT)
			if err != nil {
				return fmt.Errorf("failed to parse provider id: %w", err)
			}

			clientJWT, err := authDevice(completeRunCtx, userAuth, userPassword)
			if err != nil {
				return fmt.Errorf("failed to authenticate device: %w", err)
			}

			balanceCode, err := createBalanceCode(completeRunCtx)
			if err != nil {
				return fmt.Errorf("failed to create balance code: %w", err)
			}

			err = redeemBalanceCode(completeRunCtx, balanceCode, clientJWT)
			if err != nil {
				return fmt.Errorf("failed to redeem balance code: %w", err)
			}

			err = healthcheck.WaitForEndpoint(
				completeRunCtx,
				"http://localhost:8080/status",
				func(statusCode int, body []byte) bool {
					return statusCode == http.StatusOK
				},
				time.Second*20,
			)

			err = healthcheck.WaitForEndpoint(
				completeRunCtx,
				"http://localhost:7070/status",
				func(statusCode int, body []byte) bool {
					return statusCode == http.StatusOK
				},
				time.Second*20,
			)

			if err != nil {
				return fmt.Errorf("failed to wait for Connect endpoint: %w", err)
			}

			servicesGroup.Go(func() (err error) {
				err = runProvider(completeRunCtx, providerJWT, pw, cfg.logTCP)
				if err != nil {
					return fmt.Errorf("failed to run provider: %w", err)
				}
				return nil
			})

			clientDev, err := clientdevice.Start(
				completeRunCtx,
				clientJWT,
				apiURL,
				connectURL,
				*providerID,
				cfg.logTCP,
				cfg.useMulticlient,
			)
			if err != nil {
				return fmt.Errorf("failed to start client device: %w", err)
			}

			time.Sleep(time.Second * 2)

			addrs, err := net.InterfaceAddrs()
			if err != nil {
				return fmt.Errorf("failed to get interface addresses: %w", err)
			}

			nonLocalAddrs := []net.IP{}

			for _, a := range addrs {
				ipNet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				responds := datasource.PingPort(completeRunCtx, ipNet.IP, 15081)
				if !ipNet.IP.IsLoopback() && responds {
					nonLocalAddrs = append(nonLocalAddrs, ipNet.IP)
				}
			}

			if len(nonLocalAddrs) == 0 {
				return fmt.Errorf("no non-local addresses found that respond on port 15081")
			}

			localAddress := nonLocalAddrs[0]

			tctx, tctxcancel := context.WithTimeout(completeRunCtx, time.Second*10)
			defer tctxcancel()

			hc := http.Client{
				Transport: clientDev.Transport(),
				Timeout:   time.Second * 10,
			}

			pw.Log("using address %s", localAddress)

			{
				req, err := http.NewRequestWithContext(tctx, http.MethodGet, fmt.Sprintf("http://%s:8080", localAddress), nil)
				if err != nil {
					return fmt.Errorf("failed to create request: %w", err)
				}

				res, err := hc.Do(req)
				if err != nil {
					return fmt.Errorf("failed to get google: %w", err)
				}

				_, err = io.ReadAll(res.Body)
				if err != nil {
					return fmt.Errorf("failed to read response: %w", err)
				}

				res.Body.Close()

				pw.Log("got http response")

			}

			slog.Info("starting download bandwidth estimation")

			dlConn, err := clientDev.DialContext(completeRunCtx, "tcp", fmt.Sprintf("%s:15080", localAddress))
			if err != nil {
				return fmt.Errorf("failed to dial: %w", err)
			}

			downloadBandwidth, _ := bwestimator.EstimateDownloadBandwidth(completeRunCtx, dlConn, time.Second*30)
			if err != nil {
				return fmt.Errorf("failed to estimate bandwidth: %w", err)
			}

			err = dlConn.Close()
			if err != nil {
				return fmt.Errorf("failed to close connection: %w", err)
			}

			pw.Log("estimated download bandwidth: %.2f Mbit/s", downloadBandwidth*8.0/1024.0/1024.0)

			slog.Info("starting upload bandwidth estimation")

			ulConn, err := clientDev.DialContext(completeRunCtx, "tcp", fmt.Sprintf("%s:15081", localAddress))
			if err != nil {
				return fmt.Errorf("failed to dial: %w", err)
			}

			uploadBandwidth, _ := bwestimator.EstimateUploadBandwidth(completeRunCtx, ulConn, time.Second*30)
			if err != nil {
				return fmt.Errorf("failed to estimate bandwidth: %w", err)
			}

			err = ulConn.Close()
			if err != nil {
				return fmt.Errorf("failed to close connection: %w", err)
			}

			pw.Log("estimated upload bandwidth: %.2f Mbit/s", uploadBandwidth*8.0/1024.0/1024.0)

			if cfg.reportFile != "" {

				rf, err := os.Create(cfg.reportFile)
				if err != nil {
					return fmt.Errorf("failed to create report file: %w", err)
				}

				defer rf.Close()

				md.NewMarkdown(rf).
					Table(md.TableSet{
						Header: []string{"Direction", "Bandwidth (Mbit/s)"},
						Rows: [][]string{
							{"Upload", fmt.Sprintf("%.2f", uploadBandwidth*8.0/1024.0/1024.0)},
							{"Download", fmt.Sprintf("%.2f", downloadBandwidth*8.0/1024.0/1024.0)},
						},
					}).
					Build()

			}

			slog.Info("done estimating bandwidth")

			cancel()

			err = servicesGroup.Wait()

			return err
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
