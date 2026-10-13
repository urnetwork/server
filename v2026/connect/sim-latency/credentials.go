package main

// Credential changes are deliberately offline. A rotation adds the new token
// before an old token is revoked, so callers can deploy and verify the new
// credential without creating an authentication outage.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026"
)

const (
	credentialDeliverySchema = 1
	credentialRandomBytes    = 32
	credentialVaultLimit     = 1024 * 1024
)

var credentialNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type credentialToken struct {
	Name   string `yaml:"name"`
	Role   string `yaml:"role"`
	Sha256 string `yaml:"sha256"`
}

type credentialVault struct {
	SeedKeyBase64 string            `yaml:"seed_key_base64"`
	Tokens        []credentialToken `yaml:"tokens"`
}

type deliveredCredential struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Token  string `json:"token"`
	Sha256 string `json:"sha256"`
}

type credentialDelivery struct {
	Schema      int                   `json:"schema"`
	GeneratedAt time.Time             `json:"generated_at"`
	Credentials []deliveredCredential `json:"credentials"`
}

type credentialReceipt struct {
	Action   string            `json:"action"`
	Vault    string            `json:"vault"`
	Delivery string            `json:"delivery,omitempty"`
	Tokens   []credentialToken `json:"tokens,omitempty"`
}

func runCredentials(opts docopt.Opts) {
	vaultPath := optString(opts, "--vault", "")
	if vaultPath == "" {
		fatalf("credentials: --vault is required")
	}
	var receipt *credentialReceipt
	var err error
	switch {
	case optBool(opts, "generate"):
		receipt, err = generateCredentialVault(
			vaultPath,
			optString(opts, "--delivery", ""),
			optString(opts, "--submitter-name", ""),
			optString(opts, "--operator-name", ""),
			rand.Reader,
			server.NowUtc(),
		)
	case optBool(opts, "rotate-token"):
		receipt, err = rotateCredentialToken(
			vaultPath,
			optString(opts, "--delivery", ""),
			optString(opts, "--name", ""),
			optString(opts, "--role", ""),
			rand.Reader,
			server.NowUtc(),
		)
	case optBool(opts, "rotate-seed"):
		if !optBool(opts, "--confirm-no-unrevealed-rounds") {
			fatalf("credentials: seed rotation requires --confirm-no-unrevealed-rounds")
		}
		receipt, err = rotateCredentialSeed(vaultPath, rand.Reader)
	case optBool(opts, "revoke"):
		receipt, err = revokeCredentialToken(vaultPath, optString(opts, "--name", ""))
	default:
		err = errors.New("credential action is required")
	}
	if err != nil {
		fatalf("credentials: %s", err)
	}
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		fatalf("credentials receipt: %s", err)
	}
	fmt.Printf("%s\n", encoded)
}

func generateCredentialVault(
	vaultPath string,
	deliveryPath string,
	submitterName string,
	operatorName string,
	random io.Reader,
	now time.Time,
) (*credentialReceipt, error) {
	if deliveryPath == "" || filepath.Clean(deliveryPath) == filepath.Clean(vaultPath) {
		return nil, errors.New("--delivery must be a distinct new private file")
	}
	if _, err := os.Lstat(vaultPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return nil, errors.New("credential vault already exists")
		}
		return nil, err
	}
	if _, err := os.Lstat(deliveryPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return nil, errors.New("credential delivery already exists")
		}
		return nil, err
	}
	seed, err := randomCredentialBytes(random)
	if err != nil {
		return nil, err
	}
	submitter, err := generateCredentialToken(submitterName, "submitter", random)
	if err != nil {
		return nil, err
	}
	operator, err := generateCredentialToken(operatorName, "operator", random)
	if err != nil {
		return nil, err
	}
	vault := &credentialVault{
		SeedKeyBase64: base64.StdEncoding.EncodeToString(seed),
		Tokens: []credentialToken{
			{Name: submitter.Name, Role: submitter.Role, Sha256: submitter.Sha256},
			{Name: operator.Name, Role: operator.Role, Sha256: operator.Sha256},
		},
	}
	if err := validateCredentialVault(vault); err != nil {
		return nil, err
	}
	delivery := &credentialDelivery{
		Schema:      credentialDeliverySchema,
		GeneratedAt: now.UTC(),
		Credentials: []deliveredCredential{submitter, operator},
	}
	if err := writeCredentialDelivery(deliveryPath, delivery); err != nil {
		return nil, err
	}
	if err := withCredentialVaultLock(vaultPath, func() error {
		if _, err := os.Lstat(vaultPath); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return errors.New("credential vault was created concurrently")
			}
			return err
		}
		return writeCredentialVault(vaultPath, vault)
	}); err != nil {
		_ = os.Remove(deliveryPath)
		return nil, err
	}
	return credentialActionReceipt("generate", vaultPath, deliveryPath, vault.Tokens), nil
}

func rotateCredentialToken(
	vaultPath string,
	deliveryPath string,
	name string,
	role string,
	random io.Reader,
	now time.Time,
) (*credentialReceipt, error) {
	if deliveryPath == "" || filepath.Clean(deliveryPath) == filepath.Clean(vaultPath) {
		return nil, errors.New("--delivery must be a distinct new private file")
	}
	if _, err := os.Lstat(deliveryPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return nil, errors.New("credential delivery already exists")
		}
		return nil, err
	}
	generated, err := generateCredentialToken(name, role, random)
	if err != nil {
		return nil, err
	}
	delivery := &credentialDelivery{
		Schema:      credentialDeliverySchema,
		GeneratedAt: now.UTC(),
		Credentials: []deliveredCredential{generated},
	}
	if err := writeCredentialDelivery(deliveryPath, delivery); err != nil {
		return nil, err
	}
	var updated []credentialToken
	err = withCredentialVaultLock(vaultPath, func() error {
		vault, err := readCredentialVault(vaultPath)
		if err != nil {
			return err
		}
		for _, token := range vault.Tokens {
			if token.Name == name || token.Sha256 == generated.Sha256 {
				return errors.New("credential name or digest already exists")
			}
		}
		vault.Tokens = append(vault.Tokens, credentialToken{Name: generated.Name, Role: generated.Role, Sha256: generated.Sha256})
		sortCredentialTokens(vault.Tokens)
		if err := validateCredentialVault(vault); err != nil {
			return err
		}
		if err := writeCredentialVault(vaultPath, vault); err != nil {
			return err
		}
		updated = append([]credentialToken(nil), vault.Tokens...)
		return nil
	})
	if err != nil {
		_ = os.Remove(deliveryPath)
		return nil, err
	}
	return credentialActionReceipt("rotate-token", vaultPath, deliveryPath, updated), nil
}

func rotateCredentialSeed(vaultPath string, random io.Reader) (*credentialReceipt, error) {
	seed, err := randomCredentialBytes(random)
	if err != nil {
		return nil, err
	}
	var updated []credentialToken
	err = withCredentialVaultLock(vaultPath, func() error {
		vault, err := readCredentialVault(vaultPath)
		if err != nil {
			return err
		}
		vault.SeedKeyBase64 = base64.StdEncoding.EncodeToString(seed)
		if err := validateCredentialVault(vault); err != nil {
			return err
		}
		if err := writeCredentialVault(vaultPath, vault); err != nil {
			return err
		}
		updated = append([]credentialToken(nil), vault.Tokens...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return credentialActionReceipt("rotate-seed", vaultPath, "", updated), nil
}

func revokeCredentialToken(vaultPath string, name string) (*credentialReceipt, error) {
	if !credentialNamePattern.MatchString(name) {
		return nil, errors.New("credential name is invalid")
	}
	var updated []credentialToken
	err := withCredentialVaultLock(vaultPath, func() error {
		vault, err := readCredentialVault(vaultPath)
		if err != nil {
			return err
		}
		found := false
		kept := make([]credentialToken, 0, len(vault.Tokens)-1)
		for _, token := range vault.Tokens {
			if token.Name == name {
				found = true
				continue
			}
			kept = append(kept, token)
		}
		if !found {
			return errors.New("credential does not exist")
		}
		vault.Tokens = kept
		if err := validateCredentialVault(vault); err != nil {
			return fmt.Errorf("refusing revocation: %w", err)
		}
		if err := writeCredentialVault(vaultPath, vault); err != nil {
			return err
		}
		updated = append([]credentialToken(nil), vault.Tokens...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return credentialActionReceipt("revoke", vaultPath, "", updated), nil
}

func generateCredentialToken(name string, role string, random io.Reader) (deliveredCredential, error) {
	if !credentialNamePattern.MatchString(name) {
		return deliveredCredential{}, errors.New("credential name is invalid")
	}
	if role != "submitter" && role != "operator" {
		return deliveredCredential{}, errors.New("credential role must be submitter or operator")
	}
	raw, err := randomCredentialBytes(random)
	if err != nil {
		return deliveredCredential{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(token))
	return deliveredCredential{Name: name, Role: role, Token: token, Sha256: hex.EncodeToString(digest[:])}, nil
}

func randomCredentialBytes(random io.Reader) ([]byte, error) {
	if random == nil {
		return nil, errors.New("credential entropy source is nil")
	}
	result := make([]byte, credentialRandomBytes)
	if _, err := io.ReadFull(random, result); err != nil {
		return nil, fmt.Errorf("generate credential entropy: %w", err)
	}
	return result, nil
}

func readCredentialVault(filePath string) (*credentialVault, error) {
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || info.Size() <= 0 || credentialVaultLimit < info.Size() {
		return nil, fmt.Errorf("credential vault %s must be a nonempty private regular file", filePath)
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	defer clear(content)
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	vault := &credentialVault{}
	if err := decoder.Decode(vault); err != nil {
		return nil, fmt.Errorf("decode credential vault: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("credential vault contains multiple YAML documents")
	}
	if err := validateCredentialVault(vault); err != nil {
		return nil, err
	}
	return vault, nil
}

func validateCredentialVault(vault *credentialVault) error {
	if vault == nil {
		return errors.New("credential vault is nil")
	}
	seed, err := base64.StdEncoding.DecodeString(vault.SeedKeyBase64)
	if err != nil || len(seed) != credentialRandomBytes {
		return errors.New("seed_key_base64 must encode exactly 32 bytes")
	}
	names := map[string]bool{}
	digests := map[string]bool{}
	roles := map[string]int{}
	for _, token := range vault.Tokens {
		if !credentialNamePattern.MatchString(token.Name) || names[token.Name] {
			return errors.New("credential names must be valid and unique")
		}
		if token.Role != "submitter" && token.Role != "operator" {
			return fmt.Errorf("credential %s has an invalid role", token.Name)
		}
		decoded, err := hex.DecodeString(token.Sha256)
		if err != nil || len(decoded) != sha256.Size || digests[token.Sha256] {
			return fmt.Errorf("credential %s has an invalid or duplicate SHA-256", token.Name)
		}
		names[token.Name] = true
		digests[token.Sha256] = true
		roles[token.Role]++
	}
	if roles["submitter"] == 0 || roles["operator"] == 0 {
		return errors.New("at least one submitter and one operator credential are required")
	}
	return nil
}

func sortCredentialTokens(tokens []credentialToken) {
	sort.Slice(tokens, func(i int, j int) bool {
		if tokens[i].Role == tokens[j].Role {
			return tokens[i].Name < tokens[j].Name
		}
		return tokens[i].Role < tokens[j].Role
	})
}

func writeCredentialVault(filePath string, vault *credentialVault) error {
	content, err := yaml.Marshal(vault)
	if err != nil {
		return err
	}
	defer clear(content)
	return writeAtomicPrivateFile(filePath, content, true)
}

func writeCredentialDelivery(filePath string, delivery *credentialDelivery) error {
	content, err := json.MarshalIndent(delivery, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	defer clear(content)
	return writeAtomicPrivateFile(filePath, content, false)
}

func writeAtomicPrivateFile(filePath string, content []byte, replace bool) error {
	if strings.TrimSpace(filePath) == "" {
		return errors.New("private output path is required")
	}
	directory := filepath.Dir(filePath)
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("private output directory %s is unavailable", directory)
	}
	if !replace {
		if _, err := os.Lstat(filePath); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("private output %s already exists", filePath)
			}
			return err
		}
	}
	temporary, err := os.OpenFile(filePath+".new", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	succeeded := false
	defer func() {
		_ = temporary.Close()
		if !succeeded {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(content); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if !replace {
		if _, err := os.Lstat(filePath); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("private output %s was created concurrently", filePath)
			}
			return err
		}
	}
	if err := os.Rename(temporaryPath, filePath); err != nil {
		return err
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryFile.Close()
	if err := directoryFile.Sync(); err != nil {
		return err
	}
	succeeded = true
	return nil
}

func withCredentialVaultLock(vaultPath string, operation func() error) error {
	lockPath := vaultPath + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	return operation()
}

func credentialActionReceipt(action string, vaultPath string, deliveryPath string, tokens []credentialToken) *credentialReceipt {
	vaultAbsolute, _ := filepath.Abs(vaultPath)
	deliveryAbsolute := ""
	if deliveryPath != "" {
		deliveryAbsolute, _ = filepath.Abs(deliveryPath)
	}
	result := &credentialReceipt{Action: action, Vault: vaultAbsolute, Delivery: deliveryAbsolute}
	result.Tokens = append(result.Tokens, tokens...)
	return result
}
