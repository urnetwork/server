// Cached resource resolution. A full resolution stats every home, lists every
// version directory, and logs each step; hot paths (contract creation and
// close, client key registration, owned transactions and task claims) look
// resources up per request. A resolution is cached with the filesystem entries
// it depended on and reused only while every one of them is unchanged, which a
// lookup checks with one stat per entry and no logging.
//
// Every probe of a path first observes the path's parent directory, and a
// listing first observes the listed directory, so each entry a resolution read
// or found absent is covered by a directory observed before it: creating,
// removing, renaming or retargeting that entry changes the directory's
// modification and change times. A selected file is observed itself, so a
// rewrite in place is seen too. A config or vault push (an atomic rename into
// the mount, see warp's config-updater and the vault copy) therefore changes an
// observed directory, and the next lookup resolves again.
//
// Filesystem timestamps are coarse: a second change within the same tick as an
// observed one can leave the timestamps equal. Every change sets the kernel's
// change time to the current time, while the modification time can be set to
// anything (a copy that preserves times). A resolution is therefore cached only
// once every observed change time is older than resolverRacyInterval at the
// start of the resolution, so any later change gets a different change time.
// Until then each lookup resolves again, as it did before caching.
package server

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Longer than the timestamp granularity of the filesystems the mounts use
// (one second at worst) plus the lag of the kernel's coarse clock.
const resolverRacyInterval = 2 * time.Second

// Bounds each resolver's cache. Resource paths are code and config constants;
// past the bound, further paths are resolved on every lookup.
const maxResolverResolutions = 4096

// Unique per resolution, so equal generations name one cached resolution.
var resolverGeneration atomic.Uint64

// One cached resolution per mount root, environment and resource path.
type resolverResolutionKey struct {
	root    string
	env     string
	relPath string
}

// One resolution: the selected paths, or the not-found or unavailable error.
// Observations are set only on a cached resolution and are never changed.
type resolverResolution struct {
	paths        []string
	err          error
	observations []resolverObservation
	generation   uint64
}

// Whether every observed entry is unchanged, so a new resolution would select
// the same paths. A stat per observation; nothing is listed or logged.
func (self *resolverResolution) current() bool {
	if len(self.observations) == 0 {
		return false
	}
	for _, observation := range self.observations {
		info, err := os.Stat(observation.path)
		if err != nil {
			if observation.present || !resolverPathAbsent(err) {
				return false
			}
			continue
		}
		identity, ok := resolverIdentityOf(info)
		if !ok || !observation.present || identity != observation.identity {
			return false
		}
	}
	return true
}

// A directory or selected file as one resolution saw it, following symlinks.
type resolverObservation struct {
	path     string
	present  bool
	identity resolverIdentity
}

// The stat fields that change when an entry is replaced, retargeted, written,
// or gains or loses a directory entry.
type resolverIdentity struct {
	device          uint64
	inode           uint64
	mode            os.FileMode
	size            int64
	modifyTimeNanos int64
	changeTimeNanos int64
}

// Records the observations of one resolution. Methods accept a nil observer,
// which records nothing (explicit resolution is not cached). Not safe for
// concurrent use; one resolution owns it.
type resolverObserver struct {
	observations []resolverObservation
	pathIndexes  map[string]int
	// false once an observation could not be made or changed mid-resolution
	cacheable bool
}

// Observes path's parent before path itself is probed.
func (self *resolverObserver) observeParent(path string) {
	if self == nil {
		return
	}
	self.observeDirectory(filepath.Dir(path))
}

// Observes a directory, or its absence, once per resolution.
func (self *resolverObserver) observeDirectory(path string) {
	if self == nil {
		return
	}
	if _, ok := self.pathIndexes[path]; ok {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		if !resolverPathAbsent(err) {
			self.cacheable = false
			return
		}
		self.observe(resolverObservation{path: path})
		return
	}
	self.observeFile(path, info)
}

// Observes an entry from the stat that probed it.
func (self *resolverObserver) observeFile(path string, info os.FileInfo) {
	if self == nil {
		return
	}
	identity, ok := resolverIdentityOf(info)
	if !ok {
		self.cacheable = false
		return
	}
	self.observe(resolverObservation{path: path, present: true, identity: identity})
}

// A probe failure other than absence is not a stable fact.
func (self *resolverObserver) observeError(err error) {
	if self == nil || err == nil || resolverPathAbsent(err) {
		return
	}
	self.cacheable = false
}

// Records one observation per path. A second, different observation of a path
// means the entry changed while this resolution read it.
func (self *resolverObserver) observe(observation resolverObservation) {
	if index, ok := self.pathIndexes[observation.path]; ok {
		if self.observations[index] != observation {
			self.cacheable = false
		}
		return
	}
	if self.pathIndexes == nil {
		self.pathIndexes = map[string]int{}
	}
	self.pathIndexes[observation.path] = len(self.observations)
	self.observations = append(self.observations, observation)
}

// Whether the resolution can be cached: every observation was made, and every
// observed change time was at least resolverRacyInterval old at start. A
// modification time is not considered, since it need not be when the change
// happened.
func (self *resolverObserver) stable(start time.Time) bool {
	if !self.cacheable || len(self.observations) == 0 {
		return false
	}
	settled := start.Add(-resolverRacyInterval).UnixNano()
	for _, observation := range self.observations {
		if observation.present && settled < observation.identity.changeTimeNanos {
			return false
		}
	}
	return true
}

// Identifies one resolution of a resource and its pushed override (see
// Resolver.resourceVersion). Comparable, for use in ResourceValueCache keys.
type ResourceVersion struct {
	resolution uint64
	override   uint64
}

// Keeps one value derived from resources, reused while its key is unchanged.
// A key holds the ResourceVersion of every resource the value was read from,
// so a vault or config change, or a pushed override, derives the value again.
// Safe for concurrent use. Loads run without the lock; concurrent loads for a
// new key each derive the value, and the last stored wins.
type ResourceValueCache[K comparable, V any] struct {
	stateLock sync.Mutex
	loaded    bool
	key       K
	value     V
}

// The stored value for key that current accepts, or else the value load
// derives, which is stored when load reports it reusable. A nil current
// accepts every stored value for key.
func (self *ResourceValueCache[K, V]) Get(key K, current func(V) bool, load func() (V, bool)) V {
	value, ok := func() (V, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.value, self.loaded && self.key == key
	}()
	if ok && (current == nil || current(value)) {
		return value
	}
	value, reusable := load()
	if reusable {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.loaded = true
			self.key = key
			self.value = value
		}()
	}
	return value
}

// The simple "j2" templates translateString interpolates: `{{ env:NAME }}`.
var templateExpression = regexp.MustCompile("{{[^}]*}}")
var templateEnvExpression = regexp.MustCompile("^{{\\s*env:([^}\\s]*)\\s*}}$")

// The process environment variables that templated resource strings read, and
// the values they had when read. A value derived from templated strings keeps
// this and is derived again once any of the variables changes. Unlike
// translateString, reading and checking log nothing. Immutable once read.
type templateEnvironment struct {
	names  []string
	values []string
}

// Reads each variable the templates name once, in name order.
func readTemplateEnvironment(templates ...string) templateEnvironment {
	names := []string{}
	for _, template := range templates {
		for _, match := range templateExpression.FindAllString(template, -1) {
			if envMatches := templateEnvExpression.FindStringSubmatch(match); envMatches != nil {
				names = append(names, envMatches[1])
			}
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	values := make([]string, len(names))
	for i, name := range names {
		values[i] = os.Getenv(name)
	}
	return templateEnvironment{names: names, values: values}
}

// Whether every variable still has the value it had when read.
func (self templateEnvironment) current() bool {
	for i, name := range self.names {
		if os.Getenv(name) != self.values[i] {
			return false
		}
	}
	return true
}
