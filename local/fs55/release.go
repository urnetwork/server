package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The real releases: server binaries built from git, run against a scratch
// database on the private stack. N is a base ref built with no tag. N+1 is the
// same ref with sidecar-migrations promote run for every pending group up to
// and including the run's own, built with no tag: what the group's Gn-out
// commit builds. Both build in temporary git worktrees, so the checkout is never
// touched, and with a direct go build, never the whole-suite scripts.

// The binaries FS-55 builds (§23.6's row; OPS-M1-02).
var fs55Binaries = []string{"api", "taskworker", "bringyourctl"}

// Where each binary's main is in a server tree.
func fs55BinaryPackage(binary string) string {
	if binary == "bringyourctl" {
		return "./bringyourctl"
	}
	return "./cli/" + binary
}

// Runs a command in dir and returns its combined output in the error.
func runFs55Command(ctx context.Context, dir string, env []string, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	if env != nil {
		command.Env = env
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, output)
	}
	return nil
}

// The environment a go build runs in: the caller's, with no tag carried in
// GOFLAGS, so N and N+1 link exactly what their trees register.
func fs55BuildEnv() []string {
	env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		return strings.HasPrefix(variable, "GOFLAGS=") || strings.HasPrefix(variable, "GOWORK=")
	})
	return append(env, "GOFLAGS=-trimpath", "GOWORK=off", "CGO_ENABLED=0")
}

// Builds one release's binaries from ref, after promoting groups in order, in a
// worktree under workDirectory, and returns the directory holding them. server's
// replaces name its siblings by relative path, so the worktree sits in a
// directory that links every sibling of the checkout beside it. The worktree is
// removed once the binaries are built.
func buildFs55Binaries(ctx context.Context, serverDirectory string, workDirectory string, name string, ref string, promoteGroups []string) (string, error) {
	root := filepath.Join(workDirectory, name+"-root")
	tree := filepath.Join(root, filepath.Base(serverDirectory))
	binaryDirectory := filepath.Join(workDirectory, name+"-bin")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	siblings, err := os.ReadDir(filepath.Dir(serverDirectory))
	if err != nil {
		return "", err
	}
	for _, sibling := range siblings {
		if sibling.Name() == filepath.Base(serverDirectory) || strings.HasPrefix(sibling.Name(), ".") {
			continue
		}
		if err := os.Symlink(filepath.Join(filepath.Dir(serverDirectory), sibling.Name()), filepath.Join(root, sibling.Name())); err != nil {
			return "", err
		}
	}
	if err := runFs55Command(ctx, serverDirectory, nil, "git", "worktree", "add", "--detach", tree, ref); err != nil {
		return "", err
	}
	defer func() {
		_ = runFs55Command(context.Background(), serverDirectory, nil, "git", "worktree", "remove", "--force", tree)
	}()
	for _, groupName := range promoteGroups {
		// the tool from this checkout, run on the worktree's files
		if err := runFs55Command(ctx, serverDirectory, fs55BuildEnv(), "go", "run", "./local/sidecar-migrations", "promote", "-server", tree, groupName); err != nil {
			return "", fmt.Errorf("promoting %s for %s: %w", groupName, name, err)
		}
	}
	for _, binary := range fs55Binaries {
		output := filepath.Join(binaryDirectory, binary)
		if err := runFs55Command(ctx, tree, fs55BuildEnv(), "go", "build", "-tags", "", "-o", output, fs55BinaryPackage(binary)); err != nil {
			return "", fmt.Errorf("building %s's %s: %w", name, binary, err)
		}
	}
	return binaryDirectory, nil
}

// The groups N+1 promotes to register groupName: every pending group up to and
// including it, in order.
func fs55GroupsThrough(groupName string, pendingGroups []string) ([]string, error) {
	index := slices.Index(pendingGroups, groupName)
	if index < 0 {
		return nil, fmt.Errorf("%s is not pending in the base ref, whose pending groups are %v", groupName, pendingGroups)
	}
	return slices.Clone(pendingGroups[:index+1]), nil
}

// One built release, run as WARP_ENV=local processes on the scratch database
// that vaultHome's pg.yml names, through the attestation the caller's env holds.
type fs55BuiltRelease struct {
	name            string
	binaryDirectory string
	vaultHome       string
	configHome      string
	// a fuller config home whose all/ and missing local/ files fill the base's
	extraConfigHome string
	workDirectory   string
	queryShapes     []string
	settings        *fs55Settings
	startIndex      int
	stateLock       sync.Mutex
}

func (self *fs55BuiltRelease) Name() string {
	return self.name
}

func (self *fs55BuiltRelease) QueryShapes() []string {
	return self.queryShapes
}

// The environment of a release's process: the caller's, under which test-env.sh
// attested the private stack, with this release's identity and homes.
func (self *fs55BuiltRelease) processEnv(service string, configHome string) []string {
	env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		for _, name := range []string{"WARP_ENV=", "WARP_SERVICE=", "WARP_BLOCK=", "WARP_VERSION=", "WARP_HOST=", "WARP_VAULT_HOME=", "WARP_CONFIG_HOME=", "GOFLAGS="} {
			if strings.HasPrefix(variable, name) {
				return true
			}
		}
		return false
	})
	return append(env,
		"WARP_ENV=local",
		"WARP_SERVICE="+service,
		"WARP_BLOCK=local",
		"WARP_VERSION=0.0.0-fs55",
		"WARP_HOST=127.0.0.1",
		"WARP_VAULT_HOME="+self.vaultHome,
		"WARP_CONFIG_HOME="+configHome,
	)
}

func (self *fs55BuiltRelease) Migrate(ctx context.Context) error {
	configHome, err := self.configHomeWith("")
	if err != nil {
		return err
	}
	return runFs55Command(ctx, self.workDirectory, self.processEnv("bringyourctl", configHome),
		filepath.Join(self.binaryDirectory, "bringyourctl"), "db", "migrate")
}

// A config home for one start: the base home's entries, with the extra home's
// all/ and any local/ file the base lacks, as the test stack's portable config
// holds only what tests read and api reads more (arindb, among others); then
// sidecar.yml under local/ when given. Every entry is a link, so nothing is
// copied, and sidecar.yml is never linked from either home.
func (self *fs55BuiltRelease) configHomeWith(sidecarYaml string) (string, error) {
	self.stateLock.Lock()
	self.startIndex += 1
	home := filepath.Join(self.workDirectory, fmt.Sprintf("%s-config-%d", self.name, self.startIndex))
	self.stateLock.Unlock()
	if err := os.MkdirAll(filepath.Join(home, "local"), 0o700); err != nil {
		return "", err
	}
	linked := map[string]bool{}
	link := func(source string, name string) error {
		if linked[name] || filepath.Base(name) == "sidecar.yml" {
			return nil
		}
		linked[name] = true
		return os.Symlink(source, filepath.Join(home, name))
	}
	homes := []string{self.configHome}
	if self.extraConfigHome != "" {
		homes = append(homes, self.extraConfigHome)
	}
	// the extra home's all/ first: the base's, where it has one, holds less
	if self.extraConfigHome != "" {
		if _, err := os.Stat(filepath.Join(self.extraConfigHome, "all")); err == nil {
			if err := link(filepath.Join(self.extraConfigHome, "all"), "all"); err != nil {
				return "", err
			}
		}
	}
	for _, configHome := range homes {
		entries, err := os.ReadDir(configHome)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			if entry.Name() == "local" || strings.HasPrefix(entry.Name(), ".") || configHome != self.configHome {
				continue
			}
			if err := link(filepath.Join(configHome, entry.Name()), entry.Name()); err != nil {
				return "", err
			}
		}
		localEntries, err := os.ReadDir(filepath.Join(configHome, "local"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		for _, entry := range localEntries {
			if err := link(filepath.Join(configHome, "local", entry.Name()), filepath.Join("local", entry.Name())); err != nil {
				return "", err
			}
		}
	}
	if sidecarYaml == "" {
		return home, nil
	}
	return home, os.WriteFile(filepath.Join(home, "local", "sidecar.yml"), []byte(sidecarYaml), 0o600)
}

// A free loopback port for api to listen on.
func freeFs55Port() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func (self *fs55BuiltRelease) Start(ctx context.Context, sidecarYaml string) (fs55Process, error) {
	configHome, err := self.configHomeWith(sidecarYaml)
	if err != nil {
		return nil, err
	}
	port, err := freeFs55Port()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(self.workDirectory, fmt.Sprintf("%s-api-%d.log", self.name, port))
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	command := exec.Command(filepath.Join(self.binaryDirectory, "api"), "-p", strconv.Itoa(port))
	command.Dir = self.workDirectory
	command.Env = self.processEnv("api", configHome)
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	process := &fs55BuiltProcess{command: command, logFile: logFile, baseUrl: fmt.Sprintf("http://127.0.0.1:%d", port), exited: make(chan struct{}), stopTimeout: self.settings.StopTimeout}
	go func() {
		process.waitErr = command.Wait()
		close(process.exited)
	}()
	// ready once /status answers 200; a process that exits first failed to start
	client := &http.Client{Timeout: self.settings.RouteTimeout}
	for {
		select {
		case <-process.exited:
			logFile.Close()
			return nil, fmt.Errorf("%s's api exited before it was ready (%v), its log ending: %s", self.name, process.waitErr, fs55LogTail(logPath))
		case <-ctx.Done():
			process.Stop()
			return nil, fmt.Errorf("%s's api was not ready before its phase ended; its log is %s", self.name, logPath)
		case <-time.After(self.settings.ReadyPollInterval):
		}
		if response, err := client.Get(process.baseUrl + "/status"); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return process, nil
			}
		}
	}
}

// The last lines of a release's log that say why it stopped: its error and panic
// lines, or its last lines when it has none. The server's logs mask credentials.
func fs55LogTail(logPath string) string {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	telling := []string{}
	for _, line := range lines {
		if strings.HasPrefix(line, "E") || strings.HasPrefix(line, "F") || strings.Contains(line, "panic") {
			telling = append(telling, line)
		}
	}
	if len(telling) == 0 {
		telling = lines
	}
	if 12 < len(telling) {
		telling = telling[len(telling)-12:]
	}
	return strings.Join(telling, " / ")
}

// A running api.
type fs55BuiltProcess struct {
	command *exec.Cmd
	logFile *os.File
	baseUrl string
	exited  chan struct{}
	waitErr error
	// how long it may drain before it is killed
	stopTimeout time.Duration
}

func (self *fs55BuiltProcess) BaseUrl() string {
	return self.baseUrl
}

// Asks it to stop with SIGTERM, its drain signal, and kills it if it has not
// exited within the stop timeout.
func (self *fs55BuiltProcess) Stop() error {
	defer self.logFile.Close()
	_ = self.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-self.exited:
		return nil
	case <-time.After(self.stopTimeout):
		_ = self.command.Process.Kill()
		<-self.exited
		return fmt.Errorf("api did not exit within %s of SIGTERM, and was killed", self.stopTimeout)
	}
}
