// Protected launch credential leaves support existing bearer tokens and the
// deployed Grafana/Mimir Basic roles without URL/userinfo or plaintext logging.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Authentication is private process state and cannot be JSON-serialized.
type launchHttpCredential struct{ bearerToken, username, password string }

// HTTP lifetime/body bounds are separate from secret-leaf parsing bounds.
type launchHttpReadSettings struct {
	MaximumResponseBytes int64
	RequestTimeout       time.Duration
}

// Retains the historical limits without a package-global timing/size knob.
func defaultLaunchHttpReadSettings() launchHttpReadSettings {
	return launchHttpReadSettings{MaximumResponseBytes: 4 * 1024 * 1024, RequestTimeout: 20 * time.Second}
}

// Credential input bounds belong to the monitoring proof reader.
type launchHttpCredentialSettings struct{ MaximumBytes int64 }

// A small secret leaf is sufficient; source/config/vault parents are not inputs.
func defaultLaunchHttpCredentialSettings() launchHttpCredentialSettings {
	return launchHttpCredentialSettings{MaximumBytes: 8 * 1024}
}

// A typed object selects Basic explicitly; an old raw leaf remains Bearer.
// All errors intentionally omit raw bytes, parsed secrets and response bodies.
func readLaunchHttpCredential(path string, settings launchHttpCredentialSettings) (launchHttpCredential, error) {
	var credential launchHttpCredential
	resolved, resolveErr := filepath.EvalSymlinks(path)
	info, err := os.Lstat(path)
	if err != nil || resolveErr != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || resolved != path || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || settings.MaximumBytes <= 0 || info.Size() < 0 || info.Size() > settings.MaximumBytes {
		return credential, errors.New("monitoring credential must be a bounded private canonical regular file")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return credential, errors.New("monitoring credential unavailable")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() || opened.Mode() != info.Mode() {
		return credential, errors.New("monitoring credential changed during open")
	}
	content, err := io.ReadAll(io.LimitReader(file, settings.MaximumBytes+1))
	defer clear(content)
	after, statErr := file.Stat()
	if err != nil || statErr != nil || int64(len(content)) != info.Size() || after.Size() != info.Size() || after.ModTime() != info.ModTime() || after.Mode() != info.Mode() {
		return credential, errors.New("monitoring credential changed or exceeded its bound")
	}
	value := bytes.TrimSpace(content)
	if len(value) > 0 && value[0] == '{' {
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) != nil || len(fields) != 4 {
			return credential, errors.New("private monitoring Basic credential fields are incomplete")
		}
		for _, key := range []string{"schema", "kind", "username", "password"} {
			if raw, present := fields[key]; !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return credential, errors.New("private monitoring Basic credential fields are incomplete")
			}
		}
		var configured struct {
			Schema   int    `json:"schema"`
			Kind     string `json:"kind"`
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if decodeStrictJSONBytes(value, &configured, "private monitoring credential") != nil || configured.Schema != 1 || configured.Kind != "sim-latency-launch-basic-credential" || configured.Username == "" || configured.Password == "" || strings.ContainsAny(configured.Username, ":\r\n\x00") || strings.ContainsAny(configured.Password, "\r\n\x00") {
			return credential, errors.New("private monitoring Basic credential is malformed")
		}
		credential.username, credential.password = configured.Username, configured.Password
	} else {
		credential.bearerToken = string(value)
		if credential.bearerToken == "" || strings.ContainsAny(credential.bearerToken, "\r\n\x00") {
			return launchHttpCredential{}, errors.New("private monitoring bearer credential is malformed")
		}
	}
	return credential, nil
}

// A caller cannot silently combine a typed credential with a legacy token.
func selectLaunchHttpCredential(explicit *launchHttpCredential, legacy string) (launchHttpCredential, error) {
	if explicit == nil {
		return launchHttpCredential{bearerToken: legacy}, nil
	}
	if legacy != "" {
		return launchHttpCredential{}, errors.New("monitoring credential sources are ambiguous")
	}
	return *explicit, nil
}

// No preformatted auth header or credential-bearing URL is accepted.
func (self launchHttpCredential) apply(request *http.Request) error {
	if self.bearerToken != "" && (self.username != "" || self.password != "") {
		return errors.New("monitoring authentication methods are ambiguous")
	}
	if self.username != "" || self.password != "" {
		if self.username == "" || self.password == "" {
			return errors.New("monitoring Basic credential is incomplete")
		}
		request.SetBasicAuth(self.username, self.password)
	} else if self.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+self.bearerToken)
	}
	return nil
}
