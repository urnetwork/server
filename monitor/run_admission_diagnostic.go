// Optional, finite local queue observations. This is not a scheduling policy or
// a backend-health witness. All formatting and file work belongs to the emitter.
package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const runAdmissionEventLimit = 512
const runAdmissionPacketLimit = 128 * 1024

const (
	runAdmissionGrant uint8 = iota + 1
	runAdmissionRelease
)
const (
	runAdmissionExpired uint8 = iota + 1
	runAdmissionCanceled
	runAdmissionCancelAfterGrant
	runAdmissionContextEndedBeforeRun
	runAdmissionForDispatch
)

// Fixed storage is allocated at startup. Pool-lock writes end before the
// caller seals the terminal scalar and transfers this record exactly once.
type runAdmissionEvent struct {
	at      time.Time
	key     string
	ordinal uint64
	active  int
	kind    uint8
}
type runAdmissionHolder struct {
	at      time.Time
	key     string
	ordinal uint64
}
type runAdmissionAttempt struct {
	key                                                    string
	slot                                                   int
	enqueued, deadline, windowEnd                          time.Time
	priority, hasDeadline, windowClosed, granted, overflow bool
	terminal                                               uint8
	initial                                                [4]runAdmissionHolder
	initialCount                                           int
	initialActive                                          int
	events                                                 [runAdmissionEventLimit]runAdmissionEvent
	eventCount                                             int
}

// One producer (the joined run loop) closes pending; the emitter is never
// joined by shutdown. A blocked regular file can strand only this one optional
// emitter; it cannot retain a scheduler permit or a witness/gate/transport lock.
type RunAdmissionDiagnostic struct {
	identity         settingsWitnessIdentity
	runId, selection string
	attempts         [3]runAdmissionAttempt
	used             [3]bool
	ordinal          uint64
	pending          chan *runAdmissionAttempt
	emit             func(*runAdmissionAttempt)
}

// Require a validated invocation witness and a NEW private child of an owned
// private parent. Directory operations use held no-follow descriptors; pathname
// metadata is fenced before emission. Final parent/child components must be
// non-symlink; trusted ancestor selection remains the caller's authority.
// This does not defend against a hostile
// same-uid owner changing files after validation or privileged mount changes.
func NewRunAdmissionDiagnostic(directory string, witness *RunInvocationWitness) (*RunAdmissionDiagnostic, error) {
	if witness == nil || !witness.identity.valid() || witness.runId == "" || directory == "" {
		return nil, errors.New("admission diagnostic requires an invocation witness")
	}
	root, err := newRunAdmissionDirectory(directory, nil)
	if err != nil {
		return nil, err
	}
	self := &RunAdmissionDiagnostic{identity: witness.identity, runId: witness.runId, selection: witness.selectedKeysSha256, pending: make(chan *runAdmissionAttempt, 3)}
	self.emit = func(attempt *runAdmissionAttempt) { self.write(root, attempt) }
	go func() {
		defer root.Close()
		self.drain()
	}()
	return self, nil
}

type runAdmissionDirectory interface {
	OpenFile(string, int, os.FileMode) (*os.File, error)
	Link(string, string) error
}
type runAdmissionOwnedDirectory struct{ directory *os.File }

func (self *runAdmissionOwnedDirectory) Close() error { return self.directory.Close() }
func runAdmissionChildName(name string) bool {
	return name != "" && name != "." && name != ".." && !filepath.IsAbs(name) && filepath.Base(name) == name
}
func (self *runAdmissionOwnedDirectory) OpenFile(name string, flags int, mode os.FileMode) (*os.File, error) {
	if !runAdmissionChildName(name) {
		return nil, errors.New("admission diagnostic child unavailable")
	}
	fd, err := unix.Openat(int(self.directory.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, errors.New("admission diagnostic child unavailable")
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (self *runAdmissionOwnedDirectory) Link(from, to string) error {
	if !runAdmissionChildName(from) || !runAdmissionChildName(to) {
		return errors.New("admission diagnostic child unavailable")
	}
	fd := int(self.directory.Fd())
	return unix.Linkat(fd, from, fd, to, 0)
}
func runAdmissionPrivateDirectory(stat *unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFDIR && stat.Mode&07777 == 0700 && stat.Uid == uint32(os.Geteuid())
}
func runAdmissionSameDirectory(left, right *unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino && runAdmissionPrivateDirectory(left) && runAdmissionPrivateDirectory(right)
}

// afterCreate is an unexported deterministic race seam; production passes nil.
// No path is reopened for emitter work. A replaced parent/child, symlink or
// wrong identity/mode/owner refuses startup with fixed text, leaving artifacts.
func newRunAdmissionDirectory(path string, afterCreate func()) (*runAdmissionOwnedDirectory, error) {
	unavailable := errors.New("admission diagnostic directory unavailable")
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	if !runAdmissionChildName(name) {
		return nil, unavailable
	}
	parentFd, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, unavailable
	}
	defer unix.Close(parentFd)
	var parent, parentPathStat unix.Stat_t
	if unix.Fstat(parentFd, &parent) != nil || unix.Lstat(parentPath, &parentPathStat) != nil || !runAdmissionSameDirectory(&parent, &parentPathStat) {
		return nil, unavailable
	}
	if unix.Mkdirat(parentFd, name, 0700) != nil {
		return nil, unavailable
	}
	var createdChild unix.Stat_t
	if unix.Fstatat(parentFd, name, &createdChild, unix.AT_SYMLINK_NOFOLLOW) != nil || !runAdmissionPrivateDirectory(&createdChild) {
		return nil, unavailable
	}
	if afterCreate != nil {
		afterCreate()
	}
	childFd, err := unix.Openat(parentFd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, unavailable
	}
	child := os.NewFile(uintptr(childFd), name)
	var childStat, childPathStat unix.Stat_t
	if unix.Fstat(childFd, &childStat) != nil || unix.Fstatat(parentFd, name, &childPathStat, unix.AT_SYMLINK_NOFOLLOW) != nil || !runAdmissionSameDirectory(&createdChild, &childStat) || !runAdmissionSameDirectory(&childStat, &childPathStat) || unix.Lstat(parentPath, &parentPathStat) != nil || !runAdmissionSameDirectory(&parent, &parentPathStat) {
		child.Close()
		return nil, unavailable
	}
	return &runAdmissionOwnedDirectory{child}, nil
}

// Call only after RunExecutionLoop has joined. No wait, flush or emitter join:
// missing commit files (including shutdown partials) remain UNKNOWN.
func (self *RunAdmissionDiagnostic) Close() {
	if self != nil {
		close(self.pending)
	}
}

func (self *RunAdmissionDiagnostic) drain() {
	for attempt := range self.pending {
		self.emit(attempt)
	}
}

func runAdmissionSlot(key string) int {
	switch key {
	case "contract-degradation":
		return 1
	case "pg-freeze-age":
		return 2
	case "transaction-horizon":
		return 3
	default:
		return 0
	}
}

// Called under the existing pool lock only. No allocation, file work or second
// lock. Every target is sampled at most once, including a pre-expired caller.
func (self *RunAdmissionDiagnostic) beginWithLock(ctx context.Context, key string, priority bool, holders [4]*runSlotWaiter, active int) *runAdmissionAttempt {
	if self == nil {
		return nil
	}
	slot := runAdmissionSlot(key)
	if slot == 0 || self.used[slot-1] {
		return nil
	}
	self.used[slot-1] = true
	attempt := &self.attempts[slot-1]
	attempt.key, attempt.slot, attempt.priority = key, slot, priority
	attempt.enqueued = time.Now()
	attempt.initialActive = active
	attempt.deadline, attempt.hasDeadline = ctx.Deadline()
	for _, holder := range holders {
		if holder != nil {
			attempt.initial[attempt.initialCount] = runAdmissionHolder{holder.grantedAt, holder.signalKey, holder.grantOrdinal}
			attempt.initialCount++
		}
	}
	return attempt
}

// Only open admission windows receive events. Same-key grants retain distinct
// numeric ordinals; a holder still present at window end is not a leak claim.
func (self *RunAdmissionDiagnostic) eventWithLock(kind uint8, holder *runSlotWaiter, active int) {
	if self == nil {
		return
	}
	now := time.Now()
	for i := range self.attempts {
		attempt := &self.attempts[i]
		if !self.used[i] || attempt.windowClosed {
			continue
		}
		if attempt.eventCount == len(attempt.events) {
			attempt.overflow = true
			continue
		}
		attempt.events[attempt.eventCount] = runAdmissionEvent{now, holder.signalKey, holder.grantOrdinal, active, kind}
		attempt.eventCount++
	}
}

func (self *runAdmissionAttempt) closeWithLock(granted bool) {
	if self != nil && !self.windowClosed {
		self.windowEnd, self.windowClosed, self.granted = time.Now(), true, granted
	}
}

// After acquire returns and AFTER the caller's pre-Run context check. The
// admission window has ended under the pool lock. Permit-held work is only
// this scalar seal and nonblocking ownership transfer, never validation/I/O.
func (self *RunAdmissionDiagnostic) finish(attempt *runAdmissionAttempt, terminal uint8) int {
	if self == nil || attempt == nil {
		return 0
	}
	attempt.terminal = terminal
	select {
	case self.pending <- attempt:
	default:
	}
	return attempt.slot
}

type runAdmissionContextKey struct{}
type runAdmissionReference struct{ attempt *runAdmissionAttempt }

func (self *runAdmissionReference) attemptOrNil() *runAdmissionAttempt {
	if self == nil {
		return nil
	}
	return self.attempt
}
func (self *runSlotPool) admissionContext(ctx context.Context) (context.Context, *runAdmissionReference) {
	if self.diagnostic == nil {
		return ctx, nil
	}
	ref := &runAdmissionReference{}
	return context.WithValue(ctx, runAdmissionContextKey{}, ref), ref
}
func (self *runSlotPool) finishAdmission(ref *runAdmissionReference, terminal uint8) int {
	return self.diagnostic.finish(ref.attemptOrNil(), terminal)
}

func runAdmissionFailure(attempt *runAdmissionAttempt, err error) uint8 {
	if attempt != nil && attempt.granted {
		return runAdmissionCancelAfterGrant
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return runAdmissionExpired
	}
	return runAdmissionCanceled
}

type runAdmissionPublicEvent struct {
	Sequence int    `json:"sequence"`
	Kind     string `json:"kind"`
	Offset   *int64 `json:"offset_nanoseconds"`
	Key      string `json:"key"`
	Grant    uint64 `json:"grant_ordinal"`
	Active   int    `json:"active"`
}
type runAdmissionPublicHolder struct {
	Key   string `json:"key"`
	Grant uint64 `json:"grant_ordinal"`
	Age   *int64 `json:"age_nanoseconds_at_enqueue"`
}
type runAdmissionPacket struct {
	Schema           int                        `json:"schema"`
	Identity         settingsWitnessIdentity    `json:"identity"`
	RunId            string                     `json:"watcher_run_id"`
	Selection        string                     `json:"selected_keys_sha256"`
	Key              string                     `json:"key"`
	Slot             int                        `json:"local_admission_attempt"`
	Enqueued         time.Time                  `json:"enqueued_at_utc"`
	Deadline         time.Time                  `json:"deadline_at_utc"`
	Remaining        *int64                     `json:"remaining_nanoseconds_at_enqueue"`
	WindowEnd        *int64                     `json:"window_end_nanoseconds"`
	Priority         bool                       `json:"priority"`
	InitialActive    int                        `json:"initial_active"`
	Terminal         string                     `json:"terminal"`
	EventsComplete   bool                       `json:"events_complete"`
	EventOverflow    bool                       `json:"event_overflow"`
	IncompleteReason string                     `json:"incomplete_reason"`
	Initial          []runAdmissionPublicHolder `json:"initial_holders"`
	Events           []runAdmissionPublicEvent  `json:"events"`
}

// Emitter-only conversion/validation. Offsets use the monotonic clock carried
// by time.Time. A bad clock or overflow makes event completeness UNKNOWN.
func (self *RunAdmissionDiagnostic) packet(attempt *runAdmissionAttempt) runAdmissionPacket {
	terminal := map[uint8]string{runAdmissionExpired: "expired_without_grant", runAdmissionCanceled: "canceled_without_grant", runAdmissionCancelAfterGrant: "cancel_after_grant", runAdmissionContextEndedBeforeRun: "granted_then_context_ended_before_Run", runAdmissionForDispatch: "granted_for_dispatch"}[attempt.terminal]
	packet := runAdmissionPacket{Schema: 1, Identity: self.identity, RunId: self.runId, Selection: self.selection, Key: attempt.key, Slot: attempt.slot, Enqueued: attempt.enqueued.UTC(), Deadline: attempt.deadline.UTC(), Priority: attempt.priority, InitialActive: attempt.initialActive, Terminal: terminal, EventsComplete: attempt.windowClosed && !attempt.overflow && terminal != "" && attempt.hasDeadline && attempt.initialCount == attempt.initialActive && attempt.initialActive >= 0 && attempt.initialActive <= 4, Initial: []runAdmissionPublicHolder{}, Events: []runAdmissionPublicEvent{}}
	packet.EventOverflow = attempt.overflow
	invalidate := func(reason string) {
		packet.EventsComplete = false
		if packet.IncompleteReason == "" {
			packet.IncompleteReason = reason
		}
	}
	if !attempt.windowClosed {
		invalidate("window_unavailable")
	}
	if attempt.overflow {
		invalidate("event_overflow")
	}
	if terminal == "" {
		invalidate("terminal_unavailable")
	}
	if !attempt.hasDeadline {
		invalidate("deadline_unavailable")
	}
	if attempt.initialCount != attempt.initialActive || attempt.initialActive < 0 || attempt.initialActive > 4 {
		invalidate("occupancy_unavailable")
	}
	offset := func(from, to time.Time) *int64 {
		if from.IsZero() || to.IsZero() || to.Before(from) {
			invalidate("clock_unavailable")
			return nil
		}
		value := int64(to.Sub(from))
		return &value
	}
	packet.Remaining = offset(attempt.enqueued, attempt.deadline)
	packet.WindowEnd = offset(attempt.enqueued, attempt.windowEnd)
	for _, holder := range attempt.initial[:attempt.initialCount] {
		if holder.key == "unknown" || holder.ordinal == 0 {
			invalidate("holder_unavailable")
		}
		packet.Initial = append(packet.Initial, runAdmissionPublicHolder{holder.key, holder.ordinal, offset(holder.at, attempt.enqueued)})
	}
	previous := attempt.enqueued
	for i, event := range attempt.events[:attempt.eventCount] {
		kind := "unknown"
		switch event.kind {
		case runAdmissionGrant:
			kind = "grant"
		case runAdmissionRelease:
			kind = "release"
		}
		if kind == "unknown" || event.key == "unknown" || event.ordinal == 0 || event.active < 0 || event.active > 4 || event.at.Before(previous) || event.at.After(attempt.windowEnd) {
			invalidate("event_unavailable")
		}
		previous = event.at
		packet.Events = append(packet.Events, runAdmissionPublicEvent{i + 1, kind, offset(attempt.enqueued, event.at), event.key, event.ordinal, event.active})
	}
	if packet.EventsComplete && !runAdmissionCompletePacket(packet) {
		invalidate("event_unavailable")
	}
	return packet
}

// Emitter/offline-reader only. Complete means a coherent finite local timeline,
// not native execution, measurements, ownership blame, fairness or health.
func runAdmissionCompletePacket(packet runAdmissionPacket) bool {
	if packet.EventOverflow || packet.IncompleteReason != "" || packet.Enqueued.IsZero() || packet.Deadline.IsZero() || packet.Deadline.Before(packet.Enqueued) || packet.Remaining == nil || *packet.Remaining < 0 || packet.WindowEnd == nil || *packet.WindowEnd < 0 || packet.InitialActive != len(packet.Initial) || packet.InitialActive < 0 || packet.InitialActive > 4 {
		return false
	}
	holders := map[uint64]string{}
	var maximum uint64
	for _, holder := range packet.Initial {
		if holder.Age == nil || *holder.Age < 0 || holder.Grant == 0 || holder.Key == "unknown" {
			return false
		}
		if _, duplicate := holders[holder.Grant]; duplicate {
			return false
		}
		holders[holder.Grant] = holder.Key
		if holder.Grant > maximum {
			maximum = holder.Grant
		}
	}
	var previous int64
	targetGrants := 0
	for i, event := range packet.Events {
		if event.Sequence != i+1 || event.Offset == nil || *event.Offset < previous || *event.Offset > *packet.WindowEnd || event.Grant == 0 || event.Key == "unknown" {
			return false
		}
		previous = *event.Offset
		switch event.Kind {
		case "grant":
			if event.Key == packet.Key {
				targetGrants++
			}
			if event.Grant <= maximum {
				return false
			}
			if _, duplicate := holders[event.Grant]; duplicate {
				return false
			}
			holders[event.Grant] = event.Key
			maximum = event.Grant
		case "release":
			if key, present := holders[event.Grant]; !present || key != event.Key {
				return false
			}
			delete(holders, event.Grant)
		default:
			return false
		}
		if event.Active != len(holders) || event.Active < 0 || event.Active > 4 {
			return false
		}
	}
	granted := false
	if len(packet.Events) > 0 {
		last := packet.Events[len(packet.Events)-1]
		granted = last.Kind == "grant" && last.Key == packet.Key
	}
	switch packet.Terminal {
	case "expired_without_grant":
		return !granted && targetGrants == 0 && *packet.WindowEnd >= *packet.Remaining
	case "canceled_without_grant":
		return !granted && targetGrants == 0
	case "cancel_after_grant", "granted_then_context_ended_before_Run", "granted_for_dispatch":
		return granted && targetGrants == 1
	default:
		return false
	}
}

// Fixed vocabulary and structural bounds also apply to incomplete artifacts.
// Unknown clocks/holders may be retained as UNKNOWN, never as complete events.
func runAdmissionPacketVocabulary(packet runAdmissionPacket) bool {
	switch packet.Terminal {
	case "expired_without_grant", "canceled_without_grant", "cancel_after_grant", "granted_then_context_ended_before_Run", "granted_for_dispatch":
	default:
		return false
	}
	switch packet.IncompleteReason {
	case "", "window_unavailable", "event_overflow", "terminal_unavailable", "deadline_unavailable", "occupancy_unavailable", "clock_unavailable", "holder_unavailable", "event_unavailable":
	default:
		return false
	}
	if packet.EventsComplete != (packet.IncompleteReason == "") || packet.InitialActive < 0 || packet.InitialActive > 4 {
		return false
	}
	keys := map[string]bool{"unknown": true}
	for _, signal := range NewSignals() {
		if key := signal.Key(); key != "" && len(key) <= 64 {
			keys[key] = true
		}
	}
	for _, holder := range packet.Initial {
		if !keys[holder.Key] || holder.Age != nil && *holder.Age < 0 {
			return false
		}
	}
	for i, event := range packet.Events {
		if event.Sequence != i+1 || !keys[event.Key] || event.Kind != "grant" && event.Kind != "release" || event.Active < 0 || event.Active > 4 || event.Offset != nil && *event.Offset < 0 {
			return false
		}
	}
	if packet.EventsComplete {
		return runAdmissionCompletePacket(packet)
	}
	// An incomplete flag is not an escape hatch for an invented fixed reason.
	switch packet.IncompleteReason {
	case "window_unavailable":
		return packet.WindowEnd == nil
	case "event_overflow":
		return packet.EventOverflow
	case "deadline_unavailable":
		return packet.Deadline.IsZero() && packet.Remaining == nil
	case "occupancy_unavailable":
		return packet.InitialActive != len(packet.Initial)
	case "clock_unavailable":
		if packet.Enqueued.IsZero() || packet.Remaining == nil || packet.WindowEnd == nil {
			return true
		}
		for _, holder := range packet.Initial {
			if holder.Age == nil {
				return true
			}
		}
		for _, event := range packet.Events {
			if event.Offset == nil {
				return true
			}
		}
		return false
	case "holder_unavailable":
		for _, holder := range packet.Initial {
			if holder.Key == "unknown" || holder.Grant == 0 {
				return true
			}
		}
		return false
	case "event_unavailable":
		packet.IncompleteReason = ""
		return !runAdmissionCompletePacket(packet)
	default:
		return false
	}
}

type runAdmissionCommit struct {
	Schema int    `json:"schema"`
	Bytes  int    `json:"bytes"`
	Sha256 string `json:"sha256"`
	RunId  string `json:"watcher_run_id"`
	Slot   int    `json:"local_admission_attempt"`
}

// Full data write, fsync and successful close precede the receipt. A receipt
// becomes acceptable only by atomic exclusive hard-link publication AFTER its own full write/fsync/
// close succeeds. No pre-write complete flag, retries, overwrite or directory
// fsync/crash-durability promise. Any absent/partial/uncommitted file is UNKNOWN.
func (self *RunAdmissionDiagnostic) write(root runAdmissionDirectory, attempt *runAdmissionAttempt) {
	data, err := json.Marshal(self.packet(attempt))
	if err != nil || len(data) > runAdmissionPacketLimit {
		return
	}
	digest := sha256.Sum256(data)
	commit, err := json.Marshal(runAdmissionCommit{1, len(data), hex.EncodeToString(digest[:]), self.runId, attempt.slot})
	if err != nil {
		return
	}
	name := fmt.Sprintf("attempt-%d", attempt.slot)
	commitRunAdmissionPacket(name, data, commit, func(path string, value []byte) bool { return writeRunAdmissionFile(root, path, value) }, root.Link)
}

func commitRunAdmissionPacket(name string, data, receipt []byte, write func(string, []byte) bool, publish func(string, string) error) {
	if !write(name+".json", data) {
		return
	}
	if !write(name+".commit.pending", receipt) {
		return
	}
	_ = publish(name+".commit.pending", name+".commit.json")
}

func writeRunAdmissionFile(root runAdmissionDirectory, name string, data []byte) bool {
	output, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false
	}
	return finishRunAdmissionFile(output, data)
}

type runAdmissionOutput interface {
	io.WriteCloser
	Sync() error
}

func finishRunAdmissionFile(output runAdmissionOutput, data []byte) bool {
	n, writeErr := output.Write(data)
	syncErr := error(nil)
	if writeErr == nil && n == len(data) {
		syncErr = output.Sync()
	}
	closeErr := output.Close()
	return writeErr == nil && n == len(data) && syncErr == nil && closeErr == nil
}

// Cached readers require BOTH committed receipt and exact data hash/length.
// Valid JSON alone never establishes artifact acceptance or event completeness.
func readRunAdmissionArtifact(data, receipt []byte) (runAdmissionPacket, error) {
	var packet runAdmissionPacket
	var commit runAdmissionCommit
	decode := func(raw []byte, target any) error {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return errors.New("admission diagnostic framing unavailable")
		}
		return nil
	}
	if len(data) == 0 || len(data) > runAdmissionPacketLimit || len(receipt) == 0 || len(receipt) > 1024 || decode(receipt, &commit) != nil || decode(data, &packet) != nil {
		return packet, errors.New("admission diagnostic artifact unavailable")
	}
	// Canonical re-encoding refuses duplicate/missing fields and alternative
	// framing at every nested level; only this writer's exact schema is accepted.
	canonicalData, dataErr := json.Marshal(packet)
	canonicalCommit, commitErr := json.Marshal(commit)
	if dataErr != nil || commitErr != nil || !bytes.Equal(data, canonicalData) || !bytes.Equal(receipt, canonicalCommit) || packet.Initial == nil || packet.Events == nil || len(packet.Initial) > 4 || len(packet.Events) > runAdmissionEventLimit {
		return packet, errors.New("admission diagnostic framing unavailable")
	}
	digest := sha256.Sum256(data)
	if commit.Schema != 1 || packet.Schema != 1 || commit.Bytes != len(data) || commit.Sha256 != hex.EncodeToString(digest[:]) || !settingsWitnessHex(commit.RunId, 32) || commit.RunId != packet.RunId || commit.Slot != packet.Slot || packet.Slot != runAdmissionSlot(packet.Key) || packet.Slot == 0 || !packet.Identity.valid() || !settingsWitnessHex(packet.Selection, 64) {
		return packet, errors.New("admission diagnostic artifact unavailable")
	}
	if !runAdmissionPacketVocabulary(packet) {
		return packet, errors.New("admission diagnostic semantics unavailable")
	}
	return packet, nil
}
