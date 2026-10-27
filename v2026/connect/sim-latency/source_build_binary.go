// Compiled artifacts are inspected as data, never executed by the control host.
// A successful compiler exit alone does not authenticate its resulting binary.
package main

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"runtime/debug"
)

// Require an executable for the authoritative Linux host with complete clean
// VCS metadata. Candidate-controlled tool output is not an identity receipt.
func verifySourceBuildBinary(content []byte, expectedCommit string) (string, error) {
	if !sourceGitShaPattern.MatchString(expectedCommit) {
		return "", errors.New("source binary requires an exact approved commit")
	}
	info, err := buildinfo.Read(bytes.NewReader(content))
	if err != nil {
		return "", errors.New("source compiler did not return the expected Go program")
	}
	if err := verifySourceBuildInfo(info, expectedCommit); err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}

// Kept separate so every malformed metadata branch has deterministic coverage.
func verifySourceBuildInfo(info *debug.BuildInfo, expectedCommit string) error {
	if info == nil || !sourceGitShaPattern.MatchString(expectedCommit) || info.Main.Path != "github.com/urnetwork/server/v2026" || info.Path != "github.com/urnetwork/server/connect/sim-latency" {
		return errors.New("source compiler did not return the expected Go program")
	}
	if info.GoVersion != "go1.27.3" {
		return errors.New("competition source binary requires Go 1.27.3")
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		if _, exists := settings[setting.Key]; exists {
			return errors.New("source binary has ambiguous build settings")
		}
		settings[setting.Key] = setting.Value
	}
	if settings["vcs"] != "git" || settings["vcs.revision"] != expectedCommit || settings["vcs.modified"] != "false" ||
		settings["GOOS"] != "linux" || settings["GOARCH"] != "amd64" || settings["CGO_ENABLED"] != "0" || settings["-buildmode"] != "exe" {
		return errors.New("source binary does not bind the approved clean host-compatible build")
	}
	return nil
}
