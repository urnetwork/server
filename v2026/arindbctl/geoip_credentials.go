// Vault YAML is converted to a short-lived native config for geoipupdate.
// Secret values never enter argv, diagnostics, or the published database bundle.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const maximumMaxMindCredentialBytes int64 = 64 * 1024

type maxMindCredentials struct {
	AccountId  uint64   `yaml:"account_id"`
	LicenseKey string   `yaml:"license_key"`
	EditionIds []string `yaml:"edition_ids"`
}

// Reject parser diagnostics because they can include the rejected secret.
func decodeMaxMindCredentials(reader io.Reader) (maxMindCredentials, error) {
	input, err := io.ReadAll(io.LimitReader(reader, maximumMaxMindCredentialBytes+1))
	if err != nil || int64(len(input)) > maximumMaxMindCredentialBytes {
		return maxMindCredentials{}, errors.New("MaxMind credential file is unreadable or too large")
	}
	var credential maxMindCredentials
	decoder := yaml.NewDecoder(bytes.NewReader(input))
	decoder.KnownFields(true)
	if err := decoder.Decode(&credential); err != nil {
		return maxMindCredentials{}, errors.New("MaxMind credential YAML is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return maxMindCredentials{}, errors.New("MaxMind credential file must contain one document")
	}
	// The native format is line-oriented; reject whitespace and delimiters
	// rather than letting a value become another updater setting.
	validToken := regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	if credential.AccountId == 0 || !validToken.MatchString(credential.LicenseKey) ||
		!slices.Contains(credential.EditionIds, "GeoLite2-City") {
		return maxMindCredentials{}, errors.New("MaxMind account, license, and GeoLite2-City edition are required")
	}
	seen := map[string]bool{}
	for _, edition := range credential.EditionIds {
		if !validToken.MatchString(edition) || seen[edition] {
			return maxMindCredentials{}, errors.New("MaxMind editions contain an invalid or duplicate identifier")
		}
		seen[edition] = true
	}
	return credential, nil
}

// The callback sees only a protected temporary filename. Cleanup finishes
// before refresh can validate or publish the output directory.
func withMaxMindNativeConfig(ctx context.Context, path string, run func(string) error) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("MaxMind credential file cannot be read")
	}
	credential, err := decodeMaxMindCredentials(file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return errors.New("MaxMind credential file could not be closed")
	}
	native, err := os.CreateTemp("", "arindbctl-geoip-*.conf")
	if err != nil {
		return errors.New("cannot create protected MaxMind updater configuration")
	}
	nativePath := native.Name()
	defer func() {
		if err := os.Remove(nativePath); err != nil {
			result = errors.New("protected MaxMind updater configuration could not be removed")
		}
	}()
	_, writeErr := fmt.Fprintf(native, "AccountID %d\nLicenseKey %s\nEditionIDs %s\n", credential.AccountId, credential.LicenseKey, strings.Join(credential.EditionIds, " "))
	closeErr = native.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("cannot write protected MaxMind updater configuration")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := run(nativePath); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("geoipupdate failed; check the protected MaxMind configuration and network access")
	}
	return nil
}
