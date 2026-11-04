// Cleanup must address the same privileged runtime selected by the frozen
// evaluator. A cleanup-only endpoint or formatter override is not sufficient.
package competitionagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// This pre-launch check performs no daemon request and changes no host state.
// Cleanup itself must remain usable if the daemon becomes unhealthy later.
func (self *Agent) verifyCleanupRuntime() error {
	if !self.protected {
		return nil
	}
	if self.config.SubmissionBuilderIdentity.Path != filepath.Join(filepath.Dir(self.config.Evaluator.Path), "submission-builder-identity.sh") {
		return errors.New("cleanup builder helper is not the evaluator's own protected release")
	}
	for name, pin := range map[string]Pin{"jq": self.config.JsonFormatter, "docker": self.config.Docker} {
		path, err := controlProgramPath(name)
		if err != nil || path != pin.Path {
			return errors.New("cleanup tool pin differs from the evaluator's fixed runtime search path")
		}
	}
	endpoint, err := filepath.EvalSymlinks("/var/run/docker.sock")
	if err != nil || endpoint != self.config.DockerSocketPath {
		return errors.New("default evaluator Docker socket differs from the cleanup endpoint")
	}
	root, err := user.LookupId("0")
	if err != nil || !filepath.IsAbs(root.HomeDir) || filepath.Clean(root.HomeDir) != root.HomeDir || root.HomeDir == "/" {
		return errors.New("cannot establish the sudo target's fixed Docker configuration home")
	}
	configPath := filepath.Join(root.HomeDir, ".docker", "config.json")
	if _, err := os.Lstat(configPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("cannot inspect the evaluator's default Docker configuration")
	}
	if err := protectedRootPath(configPath, false, false); err != nil {
		return err
	}
	content, err := readBounded(configPath, self.settings.ConfigBytes)
	if err != nil {
		return err
	}
	defer clear(content)
	return validateDefaultDockerContext(content)
}

// Lookups use the fresh child PATH, never this process's ambient PATH. The
// selected executable still undergoes its independent root/content pin check.
func controlProgramPath(name string) (string, error) {
	for _, directory := range strings.Split(strings.TrimPrefix(controlEnvironment()[0], "PATH="), ":") {
		path := filepath.Join(directory, name)
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return path, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("fixed evaluator executable resolution failed")
		}
	}
	return "", errors.New("fixed evaluator executable is absent")
}

// Docker CLI v29 configfile.CurrentContext selects a daemon when --host is
// absent. Permit only its local default and no command/builder aliases. Other
// fields (including registry authentication) are never exposed or used by this
// check, and the bounded input buffer is cleared immediately by its owner.
func validateDefaultDockerContext(content []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("default Docker configuration is not an object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		folded := strings.ToLower(key)
		if err != nil || !ok || seen[folded] {
			return errors.New("default Docker configuration has ambiguous fields")
		}
		seen[folded] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return errors.New("default Docker configuration is malformed")
		}
		switch folded {
		case "currentcontext":
			var selected *string
			if key != "currentContext" || json.Unmarshal(value, &selected) != nil || selected == nil || *selected != "" && *selected != "default" {
				clear(value)
				return errors.New("evaluator Docker context is not its fixed local default")
			}
		case "aliases":
			var aliases map[string]string
			if key != "aliases" || json.Unmarshal(value, &aliases) != nil || len(aliases) != 0 {
				clear(value)
				return errors.New("evaluator Docker aliases cannot substitute a builder or command")
			}
		}
		clear(value)
	}
	if last, err := decoder.Token(); err != nil || last != json.Delim('}') {
		return errors.New("default Docker configuration did not finish")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("default Docker configuration has trailing data")
	}
	return nil
}
