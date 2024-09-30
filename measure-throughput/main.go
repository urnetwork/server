package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"bringyour.com/bringyour"
	"github.com/pterm/pterm"
	"github.com/urfave/cli/v2"
)

func main() {
	app := &cli.App{
		Name: "measure-throughput",
		Action: func(c *cli.Context) (err error) {

			runDir, err := os.MkdirTemp("", "vault")
			if err != nil {
				return fmt.Errorf("failed to create temp dir: %w", err)
			}

			vaultDir := filepath.Join(runDir, "vault")
			err = os.Mkdir(vaultDir, 0755)
			if err != nil {
				return fmt.Errorf("failed to create vault dir: %w", err)
			}

			stderrFileName := filepath.Join(runDir, "stderr.log")
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
			// You can set other flags similarly, like "log_dir" for logging to a file

			// os.Stderr = new(bytes.Buffer)

			// Parse the flags after setting them
			flag.Parse()

			defer func() {
				os.RemoveAll(runDir)
			}()

			os.Setenv("WARP_VAULT_HOME", runDir)

			multi := pterm.DefaultMultiPrinter
			postgresWriter := multi.NewWriter()

			multi.Start()
			defer multi.Stop()

			pgCleanup, err := setupPostgres(runDir, postgresWriter)
			if err != nil {
				return fmt.Errorf("failed to setup postgres: %w", err)
			}

			defer pgCleanup()

			return nil
		},
	}
	app.RunAndExitOnError()
}
