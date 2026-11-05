// Authenticated, length-delimited subevaluation transport. Only named regular
// files cross this boundary; neither side extracts archives or accepts commands.
package competitionagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const Schema = 1

// Definitive malformed/authentication failures are not transport uncertainty.
// Callers must hold the original run for integrity review, never redispatch it.
var ErrIntegrity = errors.New("subevaluation integrity rejection")

// Bounds belong to each transport/agent owner, not to candidate input. Nonpositive
// values are invalid, including a disabled bound; defaults cover full R9 reports.
type Limits struct {
	DocumentBytes      int64         `json:"document_bytes"`
	InputBytes         int64         `json:"input_bytes"`
	ArtifactBytes      int64         `json:"artifact_bytes"`
	FileBytes          int64         `json:"file_bytes"`
	FileCount          int           `json:"file_count"`
	MaximumJobDuration time.Duration `json:"maximum_job_duration"`
}

// The three-hour budget includes transfer, reconnect, evaluation and archival.
func DefaultLimits() Limits {
	return Limits{DocumentBytes: 2 * 1024 * 1024, InputBytes: 2 * 1024 * 1024 * 1024,
		ArtifactBytes: 16 * 1024 * 1024 * 1024, FileBytes: 2 * 1024 * 1024 * 1024,
		FileCount: 8192, MaximumJobDuration: 3 * time.Hour}
}

// Rejects unbounded owners before parsing any peer-controlled document.
func (self Limits) Validate() error {
	if self.DocumentBytes <= 0 || self.InputBytes <= 0 || self.ArtifactBytes <= 0 || self.FileBytes <= 0 || self.FileCount <= 0 || self.MaximumJobDuration <= 0 {
		return errors.New("subevaluation limits must all be positive")
	}
	return nil
}

// Immutable across every dispatch/reconnect. The slot ownership fence lives in
// the control envelope so it can advance without changing this execution.
type Identity struct {
	HostId      string    `json:"host_id"`
	RunId       string    `json:"run_id"`
	JobId       string    `json:"job_id"`
	RoundId     string    `json:"round_id"`
	RoundEpoch  int       `json:"round_epoch"`
	SourceEpoch int       `json:"source_epoch"`
	Cycle       int       `json:"cycle"`
	Attempt     int       `json:"attempt"`
	StartedAt   time.Time `json:"started_at"`
	DeadlineAt  time.Time `json:"deadline_at"`
}

// The run key does not contain a coordinator, transport or ownership generation.
func RunId(jobId string, cycle, attempt int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("competition-subevaluation/v1\x00%s\x00%d\x00%d", jobId, cycle, attempt)))
	return hex.EncodeToString(digest[:])
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Time is checked without replacing the signed clock with an agent-local one.
func (self Identity) Validate(limits Limits) error {
	if !namePattern.MatchString(self.HostId) || self.JobId == "" || self.RoundId == "" || self.RoundEpoch < 2 || self.RoundEpoch > 6 || self.SourceEpoch < 0 || self.SourceEpoch > 6 || self.Cycle < 0 || self.Attempt < 1 ||
		self.RunId != RunId(self.JobId, self.Cycle, self.Attempt) || self.StartedAt.IsZero() || !self.StartedAt.Before(self.DeadlineAt) ||
		limits.MaximumJobDuration < self.DeadlineAt.Sub(self.StartedAt) {
		return errors.New("invalid immutable subevaluation identity")
	}
	return nil
}

// A canonical slash-separated path is validated before any filesystem access.
type File struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// The evaluator image already contains the exact ten clean measured repositories;
// its pinned source materializer creates a fresh 0700 checkout for each run.
// Only local fixture bytes and the hidden seed are handed to the remote agent.
type StartManifest struct {
	Identity               Identity        `json:"identity"`
	EvaluatorImageDigest   string          `json:"evaluator_image_digest"`
	EvaluatorCommandSha256 string          `json:"evaluator_command_sha256"`
	SourceLockSha256       string          `json:"source_lock_sha256"`
	QualificationSha256    string          `json:"qualification_sha256"`
	ConfigLocalSha256      string          `json:"config_local_sha256"`
	VaultLocalSha256       string          `json:"vault_local_sha256"`
	RoundBaselineSha256    string          `json:"round_baseline_sha256"`
	EvaluatorRequest       json.RawMessage `json:"evaluator_request"`
	Inputs                 []File          `json:"inputs"`
}

// Commands are a closed vocabulary. SSH supplies encryption/host authentication;
// signatures additionally bind every operation to the independent agent role.
type Request struct {
	Schema       int            `json:"schema"`
	Operation    string         `json:"operation"`
	HostId       string         `json:"host_id"`
	RunId        string         `json:"run_id,omitempty"`
	FencingToken int64          `json:"fencing_token,omitempty"`
	Start        *StartManifest `json:"start,omitempty"`
	Signature    string         `json:"signature"`
}

// Terminal file digests are durable before publication. An unknown state never
// means that a launch was safe to repeat; only the same stable run may reconcile.
type Response struct {
	Schema         int             `json:"schema"`
	HostId         string          `json:"host_id"`
	RunId          string          `json:"run_id,omitempty"`
	FencingToken   int64           `json:"fencing_token,omitempty"`
	Identity       *Identity       `json:"identity,omitempty"`
	State          string          `json:"state"`
	ManifestSha256 string          `json:"manifest_sha256,omitempty"`
	RequestSha256  string          `json:"request_sha256,omitempty"`
	ExitCode       int             `json:"exit_code"`
	ErrorCode      string          `json:"error_code,omitempty"`
	SelfCheck      json.RawMessage `json:"self_check,omitempty"`
	Files          []File          `json:"files,omitempty"`
	Signature      string          `json:"signature"`
}

// Uses canonical Go JSON with the signature field empty on both endpoints.
func SignRequest(request *Request, privateKey ed25519.PrivateKey) error {
	request.Signature = ""
	content, err := json.Marshal(request)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("invalid subevaluation request signing key or document")
	}
	request.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, content))
	return nil
}

// Rejects signatures before an operation can create state or launch a service.
func VerifyRequest(request Request, publicKey ed25519.PublicKey) error {
	signature, err := base64.StdEncoding.DecodeString(request.Signature)
	request.Signature = ""
	content, marshalErr := json.Marshal(request)
	if err != nil || marshalErr != nil || len(publicKey) != ed25519.PublicKeySize || !ed25519.Verify(publicKey, content, signature) {
		return errors.Join(ErrIntegrity, errors.New("subevaluation request authentication failed"))
	}
	return nil
}

// The agent holds only its own output-signing key and the coordinator public key.
func SignResponse(response *Response, privateKey ed25519.PrivateKey) error {
	response.Signature = ""
	content, err := json.Marshal(response)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("invalid subevaluation response signing key or document")
	}
	response.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, content))
	return nil
}

// Response authentication precedes reading any peer-declared file body.
func VerifyResponse(response Response, publicKey ed25519.PublicKey) error {
	signature, err := base64.StdEncoding.DecodeString(response.Signature)
	response.Signature = ""
	content, marshalErr := json.Marshal(response)
	if err != nil || marshalErr != nil || len(publicKey) != ed25519.PublicKeySize || !ed25519.Verify(publicKey, content, signature) {
		return errors.Join(ErrIntegrity, errors.New("subevaluation response authentication failed"))
	}
	return nil
}

// Equal input manifests cannot be restarted with a different clock or seed.
func ManifestDigest(manifest StartManifest) (string, error) {
	content, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}

// A bounded frame avoids buffered decoders consuming bytes from a file body.
func WriteDocument(writer io.Writer, value any, maximum int64) error {
	content, err := json.Marshal(value)
	if err != nil || maximum <= 0 || maximum < int64(len(content)) {
		return errors.New("subevaluation document exceeds its bound")
	}
	header := make([]byte, 8)
	binary.BigEndian.PutUint64(header, uint64(len(content)))
	if n, err := writer.Write(header); err != nil || n != len(header) {
		return errors.Join(err, io.ErrShortWrite)
	}
	if n, err := writer.Write(content); err != nil || n != len(content) {
		return errors.Join(err, io.ErrShortWrite)
	}
	return nil
}

// Unknown fields and trailing JSON are rejected before signature verification.
func ReadDocument(reader io.Reader, value any, maximum int64) error {
	header := make([]byte, 8)
	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}
	size := binary.BigEndian.Uint64(header)
	if size == 0 || maximum <= 0 || uint64(maximum) < size {
		return errors.Join(ErrIntegrity, errors.New("subevaluation document exceeds its bound"))
	}
	content := make([]byte, int(size))
	if _, err := io.ReadFull(reader, content); err != nil {
		return err
	}
	defer clear(content)
	if err := validateDocumentKeys(content); err != nil {
		return errors.Join(ErrIntegrity, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.Join(ErrIntegrity, err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.Join(ErrIntegrity, errors.New("subevaluation document contains trailing data"))
	}
	return nil
}

// Owned protocol keys are exact lowercase, including embedded request/check
// documents. Reject duplicate/case-alias fields before typed JSON can fold them.
func validateDocumentKeys(content []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	var visit func(int) error
	visit = func(depth int) error {
		if depth > 64 {
			return errors.New("subevaluation document nesting exceeds its bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || key != strings.ToLower(key) || seen[key] {
					return errors.New("subevaluation document contains duplicate or noncanonical fields")
				}
				seen[key] = true
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid subevaluation JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("subevaluation document contains trailing data")
	}
	return nil
}

// Rejects traversal, links, duplicates, overflows and attacker-chosen counts
// before opening a file. File ordering itself is part of the signed manifest.
func ValidateFiles(files []File, maximum int64, limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	if len(files) > limits.FileCount || maximum <= 0 {
		return errors.Join(ErrIntegrity, errors.New("subevaluation file count exceeds its bound"))
	}
	paths := map[string]bool{}
	total := int64(0)
	for _, file := range files {
		if file.Path == "" || strings.ContainsAny(file.Path, "\\\x00\r\n") || strings.HasPrefix(file.Path, "/") ||
			filepath.ToSlash(filepath.Clean(file.Path)) != file.Path || file.Path == "." || file.Path == ".." || strings.HasPrefix(file.Path, "../") ||
			paths[file.Path] || !digestPattern.MatchString(file.Sha256) || file.Bytes < 0 || limits.FileBytes < file.Bytes || maximum-total < file.Bytes {
			return errors.Join(ErrIntegrity, errors.New("unsafe or oversized subevaluation file manifest"))
		}
		paths[file.Path] = true
		total += file.Bytes
	}
	return nil
}
