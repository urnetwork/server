// Collect bounded command output before admitting host inventory identities.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

const captureInspectTemplate = `{"id":{{json .Id}},"pid":{{json .State.Pid}},"image":{{json .Image}},"started_at":{{json .State.StartedAt}},"environment":{{json (index .Config.Labels "warp.env")}},"service":{{json (index .Config.Labels "warp.service")}},"block":{{json (index .Config.Labels "warp.block")}},"version":{{json (index .Config.Labels "version")}}`

// A private buffer prevents ReadFrom promotion from bypassing Write's bound
// while the owned Docker command drains its output pipe.
type captureBoundedOutput struct {
	buffer bytes.Buffer
	limit  int
}

// Excess output is rejected before growing the retained inventory.
func (self *captureBoundedOutput) Write(raw []byte) (int, error) {
	if len(raw) > self.limit-self.buffer.Len() {
		return 0, invalid
	}
	return self.buffer.Write(raw)
}

// The caller borrows the bounded result after the command has joined.
func (self *captureBoundedOutput) Bytes() []byte { return self.buffer.Bytes() }

// Own and join the command before exposing its bounded inventory bytes.
func captureDocker(ctx context.Context, executable string, args ...string) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(bounded, executable, args...)
	command.Stderr = nil
	command.WaitDelay = time.Second
	output := &captureBoundedOutput{limit: 256 * 1024}
	command.Stdout = output
	if command.Run() != nil || bounded.Err() != nil {
		return nil, invalid
	}
	return output.Bytes(), nil
}

func captureHostContainers(ctx context.Context, docker, environment string) ([]hostContainer, error) {
	// Name and source labels are both checked. A legacy/malformed running
	// service cannot disappear just because a current label is missing.
	data, err := captureDocker(ctx, docker, "ps", "--no-trunc", "--format", "{{.ID}}", "--filter", "name=^/"+environment+"-(connect|taskworker)-")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(data))
	if len(ids) > 64 {
		return nil, invalid
	}
	seen := map[string]bool{}
	for _, id := range ids {
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 32 || seen[id] {
			return nil, invalid
		}
		seen[id] = true
	}
	if len(ids) == 0 {
		return []hostContainer{}, nil
	}
	args := append([]string{"inspect", "--format", captureInspectTemplate}, ids...)
	data, err = captureDocker(ctx, docker, args...)
	if err != nil {
		return nil, err
	}
	rows := []hostContainer{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	for {
		var row hostContainer
		err := decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil || !seen[row.ID] || row.PID <= 0 || row.StartedAt.IsZero() || len(rows) >= len(ids) {
			return nil, invalid
		}
		delete(seen, row.ID)
		rows = append(rows, row)
	}
	if len(seen) != 0 {
		return nil, invalid
	}
	slices.SortFunc(rows, func(a, b hostContainer) int { return strings.Compare(a.ID, b.ID) })
	return rows, nil
}

func captureProcessEndpoint(row hostContainer, hostname, environment, procRoot string) hostCaptureProcess {
	result := hostCaptureProcess{Container: row, Reason: "runtime_unqualified"}
	if row.Environment != environment || (row.Service != "connect" && row.Service != "taskworker") || row.Block == "" {
		return result
	}
	process := filepath.Join(procRoot, strconv.Itoa(row.PID))
	exePath := filepath.Join(process, "exe")
	exe, err := os.Open(exePath)
	if err != nil {
		return result
	}
	defer exe.Close()
	info, err := exe.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<20 {
		return result
	}
	hash := sha256.New()
	if n, err := io.Copy(hash, io.LimitReader(exe, (128<<20)+1)); err != nil || n != info.Size() {
		return result
	}
	result.ExecutableSHA256 = hex.EncodeToString(hash.Sum(nil))
	build, err := buildinfo.Read(exe)
	if err != nil {
		return result
	}
	var revision string
	var modified bool
	hasModified := false
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			var err error
			modified, err = strconv.ParseBool(setting.Value)
			hasModified = err == nil
		}
	}
	if revision == "" || !hasModified {
		return result
	}
	base := filepath.Join(process, "root", "tmp", "arin-shadow")
	dir, err := os.Open(base)
	if err != nil {
		result.Reason = "endpoint_absent"
		return result
	}
	entries, readErr := dir.ReadDir(65)
	dir.Close()
	if readErr != nil && readErr != io.EOF || len(entries) > 64 {
		return result
	}
	role := "connect"
	if row.Service == "taskworker" {
		role = "native"
	}
	var found *captureEndpoint
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), role+"-") {
			continue
		}
		path := filepath.Join(base, entry.Name())
		data, err := readProtectedCaptureFile(filepath.Join(path, "identity.json"), 4096)
		if err != nil {
			return result
		}
		var identity server.ArinShadowRPCIdentity
		if decode(data, &identity) != nil || !captureIdentityMatches(identity, row, hostname, environment, revision, modified, server.NowUtc()) {
			return result
		}
		socket := filepath.Join(path, "capture.sock")
		socketInfo, err := os.Lstat(socket)
		if err != nil || socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0600 || found != nil {
			return result
		}
		found = &captureEndpoint{Identity: identity, Socket: socket}
	}
	after, err := os.Stat(exePath)
	if err != nil || !os.SameFile(info, after) {
		return result
	}
	if found == nil {
		result.Reason = "endpoint_absent"
		return result
	}
	result.Endpoint = found
	result.Reason = "qualified"
	return result
}

func captureIdentityMatches(identity server.ArinShadowRPCIdentity, row hostContainer, hostname, environment, revision string, modified bool, now time.Time) bool {
	role := "connect"
	if row.Service == "taskworker" {
		role = "native"
	}
	return identity.ProcessNonce != (server.Id{}) && identity.Role == role && identity.Revision == revision && identity.Modified == modified && identity.ImageDigest == row.Image && identity.Environment == environment && identity.Host == hostname && identity.Block == row.Block && strings.ReplaceAll(identity.Version, "+", "-") == row.Version && !identity.StartedAt.Before(row.StartedAt.Add(-2*time.Second)) && !identity.StartedAt.After(now.Add(2*time.Second))
}

// Host-local, read-only Docker/proc metadata. It never reads container env,
// Vault, logs, IPs or provider keys. The exact hostname is checked before Docker
// or privileged proc inspection; private output is created exclusively.
func runCaptureHostInventory(output io.Writer, args []string) error {
	flags := flag.NewFlagSet("capture-host-inventory", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	host := flags.String("hostname", "", "")
	environment := flags.String("env", "", "")
	docker := flags.String("docker", "/usr/bin/docker", "")
	directory := flags.String("output", "", "")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *host == "" || *environment != "main" || !filepath.IsAbs(*docker) || !filepath.IsAbs(*directory) {
		return invalid
	}
	actual, err := os.Hostname()
	if err != nil || actual != *host {
		return invalid
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inventory := hostCaptureInventory{Hostname: *host, Environment: *environment, StartedAt: server.NowUtc(), Complete: true, Processes: []hostCaptureProcess{}}
	before, err := captureHostContainers(ctx, *docker, *environment)
	if err != nil {
		return invalid
	}
	for _, row := range before {
		if ctx.Err() != nil {
			return invalid
		}
		process := captureProcessEndpoint(row, *host, *environment, "/proc")
		inventory.Processes = append(inventory.Processes, process)
		inventory.Complete = inventory.Complete && process.Reason == "qualified"
	}
	after, err := captureHostContainers(ctx, *docker, *environment)
	if err != nil {
		return invalid
	}
	inventory.Complete = inventory.Complete && len(before) != 0 && slices.Equal(before, after)
	inventory.FinishedAt = server.NowUtc()
	if ctx.Err() != nil || os.Mkdir(*directory, 0700) != nil {
		return invalid
	}
	endpoints := []struct {
		ProcessNonce server.Id `json:"process_nonce"`
		Socket       string    `json:"socket"`
	}{}
	for _, process := range inventory.Processes {
		if process.Endpoint != nil {
			endpoints = append(endpoints, struct {
				ProcessNonce server.Id `json:"process_nonce"`
				Socket       string    `json:"socket"`
			}{process.Endpoint.Identity.ProcessNonce, process.Endpoint.Socket})
		}
	}
	for name, value := range map[string]any{"host-inventory.json": inventory, "bridge-inventory.json": map[string]any{"endpoints": endpoints}} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return invalid
		}
		file, err := os.OpenFile(filepath.Join(*directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return invalid
		}
		_, writeErr := file.Write(encoded)
		syncErr := file.Sync()
		file.Close()
		if writeErr != nil || syncErr != nil {
			return invalid
		}
	}
	dir, err := os.Open(*directory)
	if err != nil {
		return invalid
	}
	syncErr := dir.Sync()
	dir.Close()
	if syncErr != nil {
		return invalid
	}
	return json.NewEncoder(output).Encode(map[string]any{"complete": inventory.Complete, "processes": len(before), "qualified_endpoints": len(endpoints), "policy_activated": false, "private_artifacts": 2})
}
