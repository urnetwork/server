// Command sn25golive produces and checks the SN25 mainnet operator-server
// configuration with the server's own types and validators.
//
//	sn25golive gas-approver-keygen --out KEY.json
//	sn25golive gas-policy --params PARAMS.json --dry-run [--now RFC3339]
//	sn25golive gas-policy --params PARAMS.json --approver-key KEY.json \
//	    --out-policy POLICY.yml --out-authority operator-gas-authority.yml [--now RFC3339]
//	sn25golive verify-config --policy policy-v1.yml --expect-policy-hash 0x... --out verify.yml
//	sn25golive st-check --st st.yml [--profile mainnet] [--authority operator-gas-authority.yml]
//	    [--sn sn.yml] [--now RFC3339]
//
// Every output is created exclusively and never overwritten. Secret outputs
// (the approver key and verify.yml) are written 0600 and their secrets are
// never printed. Results go to stdout; the server packages log to stderr.
//
// Resource isolation: before main runs, the server package's init reads
// settings.yml from the config and site homes (never the vault). main then
// points WARP_HOME and every resolver home at a new empty private directory,
// so the vault-backed loaders exercised by verify-config only ever see the
// candidate bytes this command injects, never an installed vault.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const usage = `usage:
  sn25golive gas-approver-keygen --out KEY.json
  sn25golive gas-policy --params PARAMS.json --dry-run [--now RFC3339]
  sn25golive gas-policy --params PARAMS.json --approver-key KEY.json --out-policy POLICY.yml --out-authority AUTHORITY.yml [--now RFC3339]
  sn25golive verify-config --policy policy-v1.yml --expect-policy-hash 0xHASH --out verify.yml
  sn25golive st-check --st st.yml [--profile mainnet] [--authority AUTHORITY.yml] [--sn sn.yml] [--now RFC3339]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cleanup, err := isolateResolverHomes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sn25golive: %v\n", err)
		os.Exit(1)
	}
	commands := map[string]func([]string, io.Writer) error{
		"gas-approver-keygen": runGasApproverKeygen,
		"gas-policy":          runGasPolicy,
		"verify-config":       runVerifyConfig,
		"st-check":            runStCheck,
	}
	command, ok := commands[os.Args[1]]
	if !ok {
		cleanup()
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	err = command(os.Args[2:], os.Stdout)
	cleanup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sn25golive %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}

// The server's resolvers stat the vault, config and site homes before they
// consult an injected override. A fresh empty directory makes every lookup
// after this point independent of WARP_HOME or an installed vault.
func isolateResolverHomes() (func(), error) {
	empty, err := os.MkdirTemp("", "sn25golive-empty-homes-")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"WARP_HOME", "WARP_VAULT_HOME", "WARP_CONFIG_HOME", "WARP_SITE_HOME"} {
		if err := os.Setenv(name, empty); err != nil {
			os.RemoveAll(empty)
			return nil, err
		}
	}
	return func() { os.RemoveAll(empty) }, nil
}

// Exclusive creation: an existing path, including a symlink, is never
// replaced. A failed write removes the partial file it created.
func writeNewFile(path string, data []byte, mode os.FileMode) (err error) {
	if path == "" {
		return errors.New("output path is required")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(path)
		}
	}()
	if err = file.Chmod(mode); err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func readBounded(path string, limit int64) ([]byte, error) {
	if path == "" {
		return nil, errors.New("input path is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), limit)
	}
	return data, nil
}

func parseNow(value string) (time.Time, error) {
	if value == "" {
		return time.Now().UTC(), nil
	}
	now, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("--now must be RFC3339: %w", err)
	}
	return now.UTC(), nil
}

func unixUtc(seconds int64) string {
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}
