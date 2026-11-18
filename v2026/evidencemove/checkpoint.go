package evidencemove

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The checkpoint's identity: a resume refuses a checkpoint written for
// another source, bucket or prefix.
const (
	checkpointVersion            = 1
	ClientKeyHistorySourceName   = "st_client_key_history"
	checkpointLockSuffix         = ".lock"
	checkpointRefusalsSuffix     = ".refused.jsonl"
	checkpointTemporaryExtension = ".tmp"
)

type CheckpointState struct {
	Version int    `json:"version"`
	Source  string `json:"source"`
	Bucket  string `json:"bucket"`
	Prefix  string `json:"prefix"`
	// the last item completed, with every item before it
	Cursor  Cursor    `json:"cursor"`
	Counts  Counts    `json:"counts"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
}

// The durable cursor of a run, in one JSON file replaced atomically (write,
// sync, rename, sync the directory), under an exclusive lock so two runs never
// share it.
type Checkpoint struct {
	Path  string
	State CheckpointState
	lock  *os.File
}

// Opens the checkpoint at path, or starts one there, and takes its lock. A
// checkpoint of another source, bucket or prefix is refused.
func OpenCheckpoint(path string, source string, bucket string, prefix string) (*Checkpoint, error) {
	if path == "" {
		return nil, errors.New("a checkpoint path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+checkpointLockSuffix, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("checkpoint %s is held by another run: %w", path, err)
	}
	checkpoint := &Checkpoint{Path: path, lock: lock}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		now := time.Now().UTC()
		checkpoint.State = CheckpointState{Version: checkpointVersion, Source: source, Bucket: bucket, Prefix: prefix, Started: now, Updated: now}
		if err := checkpoint.save(); err != nil {
			checkpoint.Close()
			return nil, err
		}
	case err != nil:
		checkpoint.Close()
		return nil, err
	default:
		if err := json.Unmarshal(data, &checkpoint.State); err != nil {
			checkpoint.Close()
			return nil, fmt.Errorf("checkpoint %s: %w", path, err)
		}
		state := checkpoint.State
		if state.Version != checkpointVersion || state.Source != source || state.Bucket != bucket || state.Prefix != prefix {
			checkpoint.Close()
			return nil, fmt.Errorf(
				"checkpoint %s is version %d of %s in %s/%s, not version %d of %s in %s/%s",
				path, state.Version, state.Source, state.Bucket, state.Prefix, checkpointVersion, source, bucket, prefix,
			)
		}
	}
	return checkpoint, nil
}

// Moves the cursor past one completed batch and adds its counts, durably.
func (self *Checkpoint) Advance(cursor Cursor, counts Counts) error {
	previous := self.State
	self.State.Cursor = cursor
	self.State.Counts.plus(counts)
	self.State.Updated = time.Now().UTC()
	if err := self.save(); err != nil {
		self.State = previous
		return err
	}
	return nil
}

func (self *Checkpoint) save() error {
	data, err := json.MarshalIndent(self.State, "", "  ")
	if err != nil {
		return err
	}
	temporary := self.Path + checkpointTemporaryExtension
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(temporary, self.Path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(self.Path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// The file every refusal of this checkpoint's runs is appended to.
func (self *Checkpoint) RefusalsPath() string {
	return self.Path + checkpointRefusalsSuffix
}

// Releases the lock.
func (self *Checkpoint) Close() error {
	if self.lock == nil {
		return nil
	}
	err := self.lock.Close()
	self.lock = nil
	return err
}

// Opens path for appending JSON lines, creating it.
func OpenJSONL(path string) (*JSONLWriter, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &JSONLWriter{file: file}, nil
}
