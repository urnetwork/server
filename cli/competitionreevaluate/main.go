// Grants one audited recovery only for a proved evaluator defect. Private
// authorization and runtime evidence never become command-line payloads.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"golang.org/x/sys/unix"
)

// Limits belong to this command, not to the scoring instrument or job budget.
type recoverySettings struct {
	// Total control transaction lifetime; non-positive refuses execution.
	Timeout time.Duration
	// Maximum private document bytes; non-positive refuses a read.
	DocumentBytes int64
	// Maximum raw token bytes, including an optional final newline.
	TokenBytes int64
}

// Control metadata remains small and cannot consume a submission's clock.
func defaultRecoverySettings() recoverySettings {
	return recoverySettings{Timeout: 30 * time.Second, DocumentBytes: 1024 * 1024, TokenBytes: 1025}
}

// Directory descriptors pin every traversed ancestor. No symlink or writable
// ancestor can redirect a root read between validation and opening the leaf.
func openRecoveryDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("private path is not canonical and absolute")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("cannot open protected root")
	}
	current := os.NewFile(uintptr(fd), "/")
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		info, err := current.Stat()
		if err != nil || !recoveryProtectedInfo(info, 0, true) {
			current.Close()
			return nil, errors.New("private path ancestor is not protected")
		}
		if component == "" {
			continue
		}
		next, err := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		current.Close()
		if err != nil {
			return nil, errors.New("cannot open protected directory")
		}
		current = os.NewFile(uintptr(next), component)
	}
	info, err := current.Stat()
	if err != nil || !recoveryProtectedInfo(info, 0, true) {
		current.Close()
		return nil, errors.New("private directory is not protected")
	}
	return current, nil
}

// Files require private permissions and a single link; directories may be
// readable but not writable by another principal. The owner is explicit for
// isolated tests; production always supplies root.
func recoveryProtectedInfo(info os.FileInfo, owner uint32, directory bool) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != owner || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if directory {
		return info.IsDir() && info.Mode().Perm()&0o022 == 0
	}
	return info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 && stat.Nlink == 1
}

// Leaf reads are bounded and descriptor-relative. The same inode must remain
// at the protected name and retain its size, permissions and modification time.
func readRecoveryLeaf(directory *os.File, name string, owner uint32, limit int64) ([]byte, error) {
	if directory == nil || name == "" || name == "." || name == ".." || filepath.Base(name) != name || limit <= 0 {
		return nil, errors.New("invalid private file read")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("cannot open private file")
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !recoveryProtectedInfo(before, owner, false) || before.Size() > limit {
		return nil, errors.New("private file ownership, permissions or size rejected")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	after, statErr := file.Stat()
	var named unix.Stat_t
	nameErr := unix.Fstatat(int(directory.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil || statErr != nil || nameErr != nil || !recoveryProtectedInfo(after, owner, false) ||
		int64(len(content)) != before.Size() || int64(len(content)) > limit || !os.SameFile(before, after) ||
		!before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("private file changed during read")
	}
	opened := after.Sys().(*syscall.Stat_t)
	if uint64(opened.Dev) != uint64(named.Dev) || opened.Ino != named.Ino {
		return nil, errors.New("private file name changed during read")
	}
	return content, nil
}

// A raw-file pin authenticates bytes before any decoding; it is independent
// of the canonical semantic digest recorded in the recovery authorization.
func readRecoveryFile(path, expected string, limit int64) ([]byte, error) {
	if path == "/" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("private file path rejected")
	}
	directory, err := openRecoveryDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	content, err := readRecoveryLeaf(directory, filepath.Base(path), 0, limit)
	if err != nil {
		return nil, err
	}
	if expected != "" {
		if !recoveryFileDigestMatches(content, expected) {
			return nil, errors.New("private file digest mismatch")
		}
	}
	return content, nil
}

// Raw-byte pins cannot be replaced with a semantically equivalent JSON digest.
func recoveryFileDigestMatches(content []byte, expected string) bool {
	digest := sha256.Sum256(content)
	return len(expected) == 64 && hex.EncodeToString(digest[:]) == expected
}

// Typed keys remain exact lowercase authority fields. Only the root terminal
// snapshot's descendants are case-sensitive data; duplicates still refuse.
func decodeRecoveryDocument(content []byte, result any, limit int64) error {
	if limit <= 0 || !utf8.Valid(content) || int64(len(content)) > limit {
		return errors.New("private document encoding or size rejected")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	var value func(int, bool) error
	value = func(depth int, snapshotData bool) error {
		if 64 < depth {
			return errors.New("private document nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, container := token.(json.Delim)
		if depth == 0 && (!container || delimiter != '{') {
			return errors.New("private document must be an object")
		}
		if container {
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for decoder.More() {
					key, err := decoder.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || seen[name] {
						return errors.New("duplicate private document field")
					}
					if !snapshotData {
						for _, character := range name {
							if character != '_' && (character < 'a' || 'z' < character) && (character < '0' || '9' < character) {
								return errors.New("private protocol field is not exact lowercase")
							}
						}
					}
					seen[name] = true
					childSnapshotData := snapshotData || depth == 0 && name == "expected_terminal_snapshot"
					if err := value(depth+1, childSnapshotData); err != nil {
						return err
					}
				}
				if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
					return errors.New("private document object is malformed")
				}
			case '[':
				for decoder.More() {
					if err := value(depth+1, snapshotData); err != nil {
						return err
					}
				}
				if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
					return errors.New("private document array is malformed")
				}
			default:
				return errors.New("unexpected private document delimiter")
			}
		}
		return nil
	}
	if err := value(0, false); err != nil {
		return errors.New("private document JSON rejected")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("private document has trailing content")
	}
	decoder = json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return errors.New("private document fields rejected")
	}
	return nil
}

// Bind the independently pinned current runtime without preempting an exact
// historical receipt lookup. The store enforces authorization/runtime equality
// for a new grant, after checking whether that request already committed.
func bindRecoveryRuntime(settings *controller.Settings, proof model.CompetitionJobReevaluationRuntime) error {
	digest, err := model.CompetitionAutomationDigest(proof)
	if err != nil || controller.BindCompetitionJobReevaluationRuntime(settings, proof, digest) != nil {
		return errors.New("runtime qualification binding rejected")
	}
	return nil
}

// Only root can invoke the control operation. Tokens are read from a private
// file, never accepted on argv, and errors cannot echo private/backend payloads.
func runRecovery(args []string, output io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("reevaluation requires root")
	}
	flags := flag.NewFlagSet("competitionreevaluate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	authPath := flags.String("authorization", "", "root-owned authorization JSON")
	authSha := flags.String("authorization-file-sha256", "", "exact authorization file digest")
	runtimePath := flags.String("runtime", "", "root-owned runtime qualification JSON")
	runtimeSha := flags.String("runtime-file-sha256", "", "exact runtime qualification file digest")
	tokenPath := flags.String("token-file", "", "root-owned scoped control token")
	idempotencyKey := flags.String("idempotency-key", "", "single-use operator request identity")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || len(*authSha) != 64 || len(*runtimeSha) != 64 || *idempotencyKey == "" {
		return errors.New("reevaluation arguments rejected")
	}
	if server.RequireEnv() != "main" {
		return errors.New("reevaluation requires the main control environment")
	}
	limits := defaultRecoverySettings()
	authContent, err := readRecoveryFile(*authPath, *authSha, limits.DocumentBytes)
	if err != nil {
		return err
	}
	runtimeContent, err := readRecoveryFile(*runtimePath, *runtimeSha, limits.DocumentBytes)
	if err != nil {
		return err
	}
	var auth model.CompetitionJobReevaluationAuthorization
	var runtime model.CompetitionJobReevaluationRuntime
	if decodeRecoveryDocument(authContent, &auth, limits.DocumentBytes) != nil || decodeRecoveryDocument(runtimeContent, &runtime, limits.DocumentBytes) != nil {
		return errors.New("reevaluation documents rejected")
	}
	token, err := readRecoveryFile(*tokenPath, "", limits.TokenBytes)
	if err != nil {
		return err
	}
	defer clear(token)
	token = bytes.TrimSuffix(token, []byte("\n"))
	if len(token) == 0 || len(token) > 1024 || bytes.ContainsAny(token, " \t\r\n") {
		return errors.New("scoped token format rejected")
	}
	settings, err := controller.LoadSettings()
	if err != nil {
		return errors.New("competition configuration unavailable")
	}
	request := &http.Request{Header: http.Header{}}
	request.Header.Set("Authorization", "Bearer "+string(token))
	principal, ok := controller.Authenticate(request, settings)
	request.Header.Del("Authorization")
	if !ok {
		return errors.New("scoped operator authentication rejected")
	}
	if err := bindRecoveryRuntime(settings, runtime); err != nil {
		return err
	}
	authorizationDigest, err := model.CompetitionAutomationDigest(auth)
	if err != nil || limits.Timeout <= 0 {
		return errors.New("authorization binding rejected")
	}
	parent, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(parent, limits.Timeout)
	defer cancel()
	receipt, err := (controller.PostgresStore{}).AuthorizeJobReevaluation(ctx, settings, principal, model.CompetitionJobReevaluationRequest{
		IdempotencyKey: *idempotencyKey, AuthorizationSha256: authorizationDigest, Authorization: auth,
	})
	if err != nil {
		return errors.New("audited reevaluation refused; inspect restricted operator evidence")
	}
	return json.NewEncoder(output).Encode(receipt)
}

// Standard output contains only the authenticated, message-free receipt.
func main() {
	if err := runRecovery(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "competition reevaluation:", err)
		os.Exit(1)
	}
}
