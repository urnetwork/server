// Package stats writes local, anonymized, protobuf stats streams to the site
// directory for later upload and analysis.
//
// A process enables stats once at startup with Enable, which mints a per-
// process instance id and roots a directory tree at
//
//	<site home>/stats/<env>/<service>-<host>-<instance>/<stream>/
//
// Callers append protobuf messages to a named stream with the package-level
// Append (or Default().Append). Each stream buffers to a bounded queue and a
// background goroutine writes streaming-zstd, length-delimited segments,
// rotated by size and age (see writer.go). Append never blocks and never
// fails: on a full queue the message is dropped and counted, so instrumenting
// a hot path (e.g. FindProviders2) is safe.
//
// Ids placed in messages must be anonymized with Anonymize so the published
// streams cannot be matched to production ids while staying traceable across
// samples (see anonymize.go).
//
// Stats are disabled (every Append a no-op) when the site directory does not
// exist, or when anonymization cannot be done safely (non-local env with no
// configured salt). This keeps the subsystem inert in environments that did
// not opt in, and fail-safe when it cannot anonymize.
package stats

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Stats is a set of named stream writers rooted at one instance directory.
// A disabled Stats is a valid object whose Append is a no-op. All methods are
// safe for concurrent use.
type Stats struct {
	ctx     context.Context
	enabled bool

	instanceId server.Id
	root       string
	anonymizer *anonymizer
	settings   *streamWriterSettings

	stateLock sync.Mutex
	// created on first append per stream
	streamWriters map[string]*streamWriter
}

// Config overrides the defaults resolved by Enable. A nil field takes the
// default.
type Config struct {
	// segment rotation / queue tuning
	MaxSegmentBytes int64
	MaxSegmentAge   time.Duration
	QueueSize       int
	// MaxQueueBytes bounds retained protobuf payloads per stream. QueueSize
	// remains a secondary object-count bound.
	MaxQueueBytes int64
}

// defaultStats is read lock-free by Default() on the FindProviders2 hot path,
// so it is an atomic pointer rather than a mutex-guarded value.
var defaultStats atomic.Pointer[Stats]

func init() {
	defaultStats.Store(&Stats{enabled: false})
}

// Default returns the process stats. Before Enable it is a disabled Stats.
// Lock-free — safe to call on a hot path.
func Default() *Stats {
	return defaultStats.Load()
}

// Enable resolves the environment and installs the process stats, returning
// it. It is safe to call when stats should be inactive: if the site directory
// is missing or anonymization cannot be done safely, the returned Stats is
// disabled. Call once at startup.
func Enable(ctx context.Context, config *Config) *Stats {
	s := newStats(ctx, config)
	defaultStats.Store(s)
	return s
}

func newStats(ctx context.Context, config *Config) *Stats {
	// the site directory must already exist. When it does not (e.g. a dev
	// process with no WARP_SITE_HOME, or a service that did not mount a
	// site), stats stay disabled rather than creating a tree under a
	// non-existent home.
	siteHome := server.SiteHomeRoot()
	if info, err := os.Stat(siteHome); err != nil || !info.IsDir() {
		glog.Infof("[stats]disabled: site home %q not present\n", siteHome)
		return &Stats{enabled: false}
	}

	env, _ := server.Env()
	local := env == "local"

	anonymizer, ok := newAnonymizer(local, statsHmacSalt())
	if !ok {
		glog.Infof("[stats]disabled: no hmac salt configured for env %q (fail-safe)\n", env)
		return &Stats{enabled: false}
	}

	instanceId := server.NewId()
	host, _ := server.Host()
	service, _ := server.Service()
	label := sanitizeLabel(fmt.Sprintf("%s-%s-%s", nonEmpty(service, "service"), nonEmpty(host, "host"), instanceId))
	root := filepath.Join(siteHome, "stats", nonEmpty(env, "env"), label)
	if err := os.MkdirAll(root, 0o755); err != nil {
		glog.Infof("[stats]disabled: cannot create %q: %s\n", root, err)
		return &Stats{enabled: false}
	}

	settings := defaultStreamWriterSettings()
	if config != nil {
		if 0 < config.MaxSegmentBytes {
			settings.maxSegmentBytes = config.MaxSegmentBytes
		}
		if 0 < config.MaxSegmentAge {
			settings.maxSegmentAge = config.MaxSegmentAge
		}
		if 0 < config.QueueSize {
			settings.queueSize = config.QueueSize
		}
		if 0 < config.MaxQueueBytes {
			settings.maxQueueBytes = config.MaxQueueBytes
		}
	}

	glog.Infof("[stats]enabled instance=%s root=%s local=%t\n", instanceId, root, local)

	return &Stats{
		ctx:           ctx,
		enabled:       true,
		instanceId:    instanceId,
		root:          root,
		anonymizer:    anonymizer,
		settings:      settings,
		streamWriters: map[string]*streamWriter{},
	}
}

// Enabled reports whether appends are recorded.
func (self *Stats) Enabled() bool {
	return self.enabled
}

// InstanceId is the per-process id in the stats directory path. Zero when
// disabled.
func (self *Stats) InstanceId() server.Id {
	return self.instanceId
}

// Anonymize returns the stable pseudonym for id. On a disabled Stats it
// returns the raw id bytes (only reachable when nothing is being written).
func (self *Stats) Anonymize(id server.Id) []byte {
	if !self.enabled {
		return id.Bytes()
	}
	return self.anonymizer.anonymize(id)
}

// Append records message on the named stream. Never blocks; drops (and
// counts) the message if the stream's queue is full. A no-op when disabled.
// stream must be a simple name (used as a directory); it is sanitized.
func (self *Stats) Append(stream string, message proto.Message) {
	if !self.enabled {
		return
	}
	writer := self.streamWriter(stream)
	if writer == nil {
		return
	}
	writer.append(message)
}

func (self *Stats) streamWriter(stream string) *streamWriter {
	stream = sanitizeLabel(stream)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if writer, ok := self.streamWriters[stream]; ok {
		return writer
	}
	writer, err := newStreamWriter(self.ctx, filepath.Join(self.root, stream), stream, self.settings)
	if err != nil {
		glog.Infof("[stats]stream %q writer err=%s\n", stream, err)
		return nil
	}
	self.streamWriters[stream] = writer
	return writer
}

// --- bulk stream loading ---
//
// These read stats back in bulk (the inverse of Append), yielding each raw
// protobuf frame so the caller can unmarshal to its own message type
// (segments are generic — the stream name determines the type). Single-source
// readers live in segment.go (ReadSegment/ReadSegmentStream, one segment) and
// flat.go (ReadFlat, an xz flat export, typed to FindProviders2Sample). Frames
// are yielded in write order (segment names sort by time).

// StreamDir returns the directory holding this process's segments for stream.
// Empty when disabled.
func (self *Stats) StreamDir(stream string) string {
	if !self.enabled {
		return ""
	}
	return filepath.Join(self.root, sanitizeLabel(stream))
}

// LoadStream reads every frame this process has written to stream, across its
// finalized segments, calling onFrame for each. A convenience over
// LoadSegmentDir(self.StreamDir(stream), ...).
func (self *Stats) LoadStream(stream string, onFrame func(frame []byte) error) error {
	if !self.enabled {
		return nil
	}
	return LoadSegmentDir(self.StreamDir(stream), onFrame)
}

// SegmentPaths lists the finalized .pb.zst segment files under dir (recursive),
// sorted by name (segment names lead with a millisecond timestamp, so name
// order is write order).
func SegmentPaths(dir string) ([]string, error) {
	paths := []string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, segmentExt) { // .pb.zst, never .partial
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

// LoadSegments reads every frame from the given segment files, in the given
// order, calling onFrame for each.
func LoadSegments(paths []string, onFrame func(frame []byte) error) error {
	for _, path := range paths {
		if err := ReadSegment(path, onFrame); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

// LoadSegmentDir reads every frame from every finalized segment under dir
// (recursive), in time order. A missing directory yields nothing.
func LoadSegmentDir(dir string, onFrame func(frame []byte) error) error {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil
	}
	paths, err := SegmentPaths(dir)
	if err != nil {
		return err
	}
	return LoadSegments(paths, onFrame)
}

// LoadStreamTyped reads every frame under dir and unmarshals each into a fresh
// message from newMessage, calling onMessage. A generic typed convenience over
// LoadSegmentDir. The message is reused only if newMessage returns the same
// pointer; return a fresh one to retain.
func LoadStreamTyped[T proto.Message](dir string, newMessage func() T, onMessage func(T) error) error {
	return LoadSegmentDir(dir, func(frame []byte) error {
		message := newMessage()
		if err := proto.Unmarshal(frame, message); err != nil {
			return err
		}
		return onMessage(message)
	})
}

// Close finalizes every stream's open segment and stops the writers.
func (self *Stats) Close() {
	if !self.enabled {
		return
	}
	self.stateLock.Lock()
	writers := make([]*streamWriter, 0, len(self.streamWriters))
	for _, writer := range self.streamWriters {
		writers = append(writers, writer)
	}
	self.streamWriters = map[string]*streamWriter{}
	self.stateLock.Unlock()

	for _, writer := range writers {
		writer.close()
	}
}

// statsHmacSalt reads the anonymization salt from the stats vault resource.
// Empty when unset (or in an env with no vault stats.yml).
func statsHmacSalt() []byte {
	resource, err := server.Vault.SimpleResource("stats.yml")
	if err != nil {
		return nil
	}
	values := resource.String("hmac_salt")
	if len(values) != 1 || values[0] == "" {
		return nil
	}
	return []byte(values[0])
}

func nonEmpty(s string, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// sanitizeLabel keeps a path segment to a safe, filesystem-friendly form.
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "unnamed"
	}
	return out
}
