// Where warpctl finds its vault, for the commands that must work in the vault warpctl itself reads:
// `certs swap` on an lb host, whose vault warpctl mounts into each lb, and `certs renew` on the
// builder, whose vault `warpctl certs issue` writes and the lb build reads. warpctl's
// RequireVaultHome (warpctl/docker.go) takes warp.json's vaultHome, then $WARP_VAULT_HOME, then
// $WARP_HOME/vault, and so does this.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The vault warpctl would use with this environment. Where warpctl could find another vault this
// refuses: with no WARP_HOME, whose warp.json outranks the rest, with a warp.json it cannot read,
// which warpctl takes for none, and with a path that is not absolute, which names another
// directory to each process that reads it and which docker mounts into no container.
func findWarpVaultHome() (string, error) {
	warpHome := os.Getenv("WARP_HOME")
	if warpHome == "" {
		return "", errors.New("WARP_HOME is not set, so warp.json, which can name the vault warpctl uses, cannot be read")
	}
	vaultHome := ""
	warpJsonPath := filepath.Join(warpHome, "warp.json")
	warpJson, err := os.ReadFile(warpJsonPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil {
		// the file holds warpctl's docker hub token too, so only this setting is decoded, and a
		// syntax error, which would quote a byte of the file, gives only where it is
		warpSettings := struct {
			VaultHome *string `json:"vaultHome"`
		}{}
		if err := json.Unmarshal(warpJson, &warpSettings); err != nil {
			syntaxErr := &json.SyntaxError{}
			if errors.As(err, &syntaxErr) {
				return "", fmt.Errorf("%s is not json from byte %d", warpJsonPath, syntaxErr.Offset)
			}
			return "", fmt.Errorf("%s: %w", warpJsonPath, err)
		}
		if warpSettings.VaultHome != nil {
			if *warpSettings.VaultHome == "" {
				return "", fmt.Errorf("%s names an empty vaultHome", warpJsonPath)
			}
			vaultHome = *warpSettings.VaultHome
		}
	}
	if vaultHome == "" {
		vaultHome = os.Getenv("WARP_VAULT_HOME")
	}
	if vaultHome == "" {
		vaultHome = filepath.Join(warpHome, "vault")
	}
	if !filepath.IsAbs(vaultHome) {
		return "", fmt.Errorf("the vault home %s is not an absolute path, so it names another directory to each process that reads it, and no lb can mount it", vaultHome)
	}
	return vaultHome, nil
}
