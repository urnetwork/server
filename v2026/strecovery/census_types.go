// Package strecovery collects original EVM signatures without signing, sending,
// changing database status or turning retained bytes into spending authority.
package strecovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const ConfigSchema = "urnetwork-operator-recovery-config-v1"
const ArchiveSchema = "urnetwork-operator-recovery-archive-v1"
const MaximumArchiveBytes = 128 * 1024 * 1024

var labelPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// All limits are explicit; reaching one refuses a partial census rather than
// returning a truncated successful collection. Raw byte limits count duplicates.
type Limits struct {
	MaximumIntents          int `json:"maximum_intents"`
	MaximumAttempts         int `json:"maximum_attempts"`
	MaximumTransactionBytes int `json:"maximum_transaction_bytes"`
	MaximumTotalBytes       int `json:"maximum_total_bytes"`
}

// Bounds apply to the complete union, not a fresh allowance per source.
func (self Limits) validate() error {
	if self.MaximumIntents < 1 || self.MaximumIntents > 100000 || self.MaximumAttempts < 1 || self.MaximumAttempts > 100000 ||
		self.MaximumTransactionBytes < 1 || self.MaximumTransactionBytes > 128*1024 || self.MaximumTotalBytes < self.MaximumTransactionBytes || self.MaximumTotalBytes > 32*1024*1024 {
		return errors.New("recovery census requires finite intent, attempt and byte limits")
	}
	return nil
}

// Nonce ranges are independently supplied completeness expectations, never
// observations of current chain state or permission to reserve those nonces.
type Role struct {
	Id         string `json:"id"`
	Address    string `json:"address"`
	FirstNonce uint64 `json:"first_nonce"`
	NextNonce  uint64 `json:"next_nonce"`
}

// Content pins authenticate local inputs only; they do not attest their origin.
type FileReference struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

// Each operator database is selected independently, including its complete role
// set. Connection files are private credentials and never enter the archive.
type DatabaseSource struct {
	Id         string        `json:"id"`
	Connection FileReference `json:"connection"`
	Roles      []string      `json:"roles"`
}

// A store contains canonical hash-named .rlp files. Native .scale files remain
// outside this EVM census and are counted explicitly rather than interpreted.
type StoreSource struct {
	Id        string   `json:"id"`
	Directory string   `json:"directory"`
	Roles     []string `json:"roles"`
}

// Two or more explicit databases prevent accidentally collecting only one
// operator. Configured source/role authority must be supplied independently.
type Config struct {
	Schema    string           `json:"schema"`
	ChainId   uint64           `json:"chain_id"`
	Genesis   string           `json:"genesis_hash"`
	Roles     []Role           `json:"roles"`
	Databases []DatabaseSource `json:"databases"`
	Stores    []StoreSource    `json:"stores"`
	Limits    Limits           `json:"limits"`
}

// Canonical spellings keep equivalent local paths and hashes from hiding a
// duplicate source. Independent database topology is an external input fact.
func (self Config) Validate() error {
	if self.Schema != ConfigSchema || self.ChainId == 0 || !canonicalHex(self.Genesis, 32) || len(self.Roles) == 0 || len(self.Roles) > 64 ||
		len(self.Databases) < 2 || len(self.Databases) > 16 || len(self.Stores) == 0 || len(self.Stores) > 32 {
		return errors.New("recovery census requires explicit network, roles, both databases and evidence stores")
	}
	if err := self.Limits.validate(); err != nil {
		return err
	}
	roleIds, addresses := map[string]bool{}, map[string]bool{}
	for _, role := range self.Roles {
		if !labelPattern.MatchString(role.Id) || roleIds[role.Id] || !canonicalHex(role.Address, 20) || addresses[role.Address] ||
			role.NextNonce < role.FirstNonce || role.NextNonce-role.FirstNonce > uint64(self.Limits.MaximumAttempts) {
			return errors.New("recovery role identity or expected nonce coverage differs")
		}
		roleIds[role.Id], addresses[role.Address] = true, true
	}
	sourceIds, paths, claimedRoles := map[string]bool{}, map[string]bool{}, map[string]bool{}
	checkSource := func(id, path string, roles []string) error {
		if !labelPattern.MatchString(id) || sourceIds[id] || !absolutePath(path) || paths[path] || len(roles) == 0 {
			return errors.New("recovery sources require distinct labels, canonical paths and selected roles")
		}
		sourceRoles := map[string]bool{}
		for _, role := range roles {
			if !roleIds[role] || sourceRoles[role] {
				return errors.New("recovery source has an unknown or repeated role")
			}
			sourceRoles[role], claimedRoles[role] = true, true
		}
		sourceIds[id], paths[path] = true, true
		return nil
	}
	for _, source := range self.Databases {
		if !canonicalDigest(source.Connection.Sha256) {
			return errors.New("database connection file needs an exact byte pin")
		}
		if err := checkSource(source.Id, source.Connection.Path, source.Roles); err != nil {
			return err
		}
	}
	for _, source := range self.Stores {
		if err := checkSource(source.Id, source.Directory, source.Roles); err != nil {
			return err
		}
	}
	if len(claimedRoles) != len(roleIds) {
		return errors.New("recovery role is absent from the selected source census")
	}
	return nil
}

// Every generation and status survives the export; unsigned intents remain a
// separate class and never become fabricated transactions or consumed fees.
type Intent struct {
	Id            string    `json:"id"`
	IntentKey     string    `json:"intent_key"`
	LogicalKey    string    `json:"logical_key"`
	Generation    int       `json:"generation"`
	Profile       string    `json:"profile"`
	DeploymentId  string    `json:"deployment_id"`
	DeploymentKey string    `json:"deployment_key"`
	ChainId       int64     `json:"chain_id"`
	Genesis       string    `json:"genesis_hash"`
	From          string    `json:"from"`
	To            string    `json:"to"`
	CalldataHash  string    `json:"calldata_hash"`
	Calldata      []byte    `json:"calldata"`
	Nonce         int64     `json:"nonce"`
	Status        string    `json:"status"`
	CurrentHash   *string   `json:"current_hash"`
	AttemptCount  int       `json:"attempt_count"`
	Error         *string   `json:"error"`
	CreateTime    time.Time `json:"create_time"`
	UpdateTime    time.Time `json:"update_time"`
}

// Database status and stored receipt columns are retained claims, not canonical
// receipt evidence. Exact raw bytes are the custody artifact being recovered.
type Attempt struct {
	IntentId       string    `json:"intent_id"`
	Number         int       `json:"number"`
	Kind           string    `json:"kind"`
	Hash           string    `json:"hash"`
	Raw            []byte    `json:"raw"`
	GasLimit       int64     `json:"gas_limit"`
	GasPrice       *string   `json:"gas_price"`
	GasTipCap      *string   `json:"gas_tip_cap"`
	GasFeeCap      *string   `json:"gas_fee_cap"`
	Status         string    `json:"status"`
	InclusionBlock *int64    `json:"inclusion_block"`
	InclusionHash  *string   `json:"inclusion_hash"`
	FinalizedBlock *int64    `json:"finalized_block"`
	FinalizedHash  *string   `json:"finalized_hash"`
	Error          *string   `json:"error"`
	CreateTime     time.Time `json:"create_time"`
	UpdateTime     time.Time `json:"update_time"`
}

// A snapshot is complete only after its read-only repeatable-read transaction
// finishes. It is not a synchronized snapshot across separate databases.
type DatabaseSnapshot struct {
	Intents  []Intent  `json:"intents"`
	Attempts []Attempt `json:"attempts"`
}

// The collector accepts a read-only snapshot port, never a transaction writer.
type SnapshotReader interface {
	Snapshot(context.Context, DatabaseSource, Limits) (*DatabaseSnapshot, error)
}

// Errors identify the exact refused source/record without logging credentials,
// raw signatures or calldata. The original source is never changed on refusal.
type Refusal struct {
	Source string `json:"source"`
	Record string `json:"record"`
	Cause  string `json:"cause"`
}

// Refusal text is a stable projection, not a raw database or credential error.
func (self *Refusal) Error() string {
	return fmt.Sprintf("recovery census refused source %q record %q: %s", self.Source, self.Record, self.Cause)
}

// Hashes bind exact archive/source bytes; no hash is a signing authorization.
func digest(raw []byte) string {
	value := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(value[:])
}

// All hashed objects have fixed JSON wire structs with no caller-owned maps.
func objectDigest(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return digest(raw)
}

// Literal owner paths cannot expand environment or silently traverse aliases.
func absolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "$\x00")
}

// Hash and address identities use exactly one nonzero lowercase encoding.
func canonicalHex(value string, size int) bool {
	if len(value) != 2+2*size || !strings.HasPrefix(value, "0x") || value != strings.ToLower(value) {
		return false
	}
	raw, err := hex.DecodeString(value[2:])
	return err == nil && len(raw) == size && value != "0x"+strings.Repeat("0", 2*size)
}

// SHA256 identifiers are distinct from EVM transaction hashes.
func canonicalDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && canonicalHex("0x"+strings.TrimPrefix(value, "sha256:"), 32)
}
