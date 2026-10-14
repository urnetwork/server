package server

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maps"

	"github.com/urnetwork/glog"

	"github.com/coreos/go-semver/semver"
	"gopkg.in/yaml.v3"
	// _ "github.com/urnetwork/glog"
)

/*
support two deployment cases:

1. local development. $WARP_HOME/{vault,config,site}/<env>
2. warp container deployment. Each container has mounted at $WARP_HOME/{vault,config,site}

*/

func getenvWithDefault(key string, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func initGlog() {
	if !slices.Contains(os.Args, "-logtostderr") {
		fmt.Fprintf(os.Stderr, "[env]glog not configured from command line. Using default configuration.\n")

		flag.Set("logtostderr", getenvWithDefault("BY_LOG_LOGTOSTDERR", "true"))
		flag.Set("stderrthreshold", getenvWithDefault("BY_LOG_STDERRTHRESHOLD", "INFO"))
		flag.Set("v", getenvWithDefault("BY_LOG_V", "0"))
	}
}

type MountType = string

const (
	MOUNT_TYPE_VAULT  MountType = "vault"
	MOUNT_TYPE_CONFIG MountType = "config"
	MOUNT_TYPE_SITE   MountType = "string"
)

var Vault = NewResolver(MOUNT_TYPE_VAULT)
var Config = NewResolver(MOUNT_TYPE_CONFIG)
var Site = NewResolver(MOUNT_TYPE_SITE)

var (
	// ErrResourceNotFound identifies a resource that is genuinely absent from
	// every configured home. Optional-resource callers may explicitly noop on
	// this error without also swallowing permission, I/O, or shape failures.
	ErrResourceNotFound = errors.New("resource not found")
	// ErrResourceUnavailable identifies a resource lookup that could not prove
	// absence because a candidate path or version directory was inaccessible or
	// had an invalid filesystem shape.
	ErrResourceUnavailable = errors.New("resource unavailable")
)

var DefaultWarpHome = "/srv/warp"

func init() {
	initGlog()
	settingsObj := GetSettings()
	glog.Infof("[env]settings = %s\n", settingsObj)
	if envObj, ok := settingsObj["env_vars"]; ok {
		switch v := envObj.(type) {
		case map[string]any:
			for key, value := range v {
				switch w := value.(type) {
				case string:
					os.Setenv(key, w)
				default:
					glog.Infof("[env]unrecognized env value \"%s\"=\"%s\" (%T)\n", key, w, w)
				}
			}
		}
	}
}

func GetSettings() map[string]any {
	// merge in order of precedence (last has precedence)
	// - config/settings.yml[all]
	// - config/settings.yml[host]
	// - site/settings.yml
	settingsName := "settings.yml"

	settingsObj := map[string]any{}
	if res, err := Config.SimpleResource(settingsName); err == nil {
		configSettingsObj := res.Parse()
		if allSettingsObj, ok := configSettingsObj["all"]; ok {
			switch v := allSettingsObj.(type) {
			case map[string]any:
				maps.Copy(settingsObj, v)
			}
		}
		if hostSettingsObj, ok := configSettingsObj[RequireHost()]; ok {
			switch v := hostSettingsObj.(type) {
			case map[string]any:
				maps.Copy(settingsObj, v)
			}
		}
	}

	if res, err := Site.SimpleResource(settingsName); err == nil {
		maps.Copy(settingsObj, res.Parse())
	}

	return settingsObj
}

func WarpHome() string {
	warpHome := os.Getenv("WARP_HOME")
	if warpHome != "" {
		return warpHome
	}
	return DefaultWarpHome
}

func VaultHomeRoot() string {
	warpVaultHome := os.Getenv("WARP_VAULT_HOME")
	if warpVaultHome != "" {
		return warpVaultHome
	}
	return filepath.Join(WarpHome(), "vault")
}

func VaultHomes() []string {
	root := VaultHomeRoot()
	return resolveMultiHome(root)
}

// TestRealVaultEnv opts a test binary in to reading a vault that is not a test
// fixture, such as the deployment vault $WARP_HOME/vault with its secrets.
const TestRealVaultEnv = "URNETWORK_TEST_REAL_VAULT"

// The repository's synthetic vault and config, which test-env.sh and
// local/run-local.sh use in portable mode. Empty in a -trimpath build.
var syntheticTestResourceRoot = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return ""
	}
	return filepath.Join(filepath.Dir(file), "local", "testdata")
}()

// A test binary reads only a fixture vault unless TestRealVaultEnv is "1": one
// below the release gate's private fixture root, the repository's synthetic
// local/testdata, or the temp directory or GOTMPDIR where a test writes its
// own. Any other vault, such as $WARP_HOME/vault, may hold real secrets and
// reads as empty. Other binaries are unaffected.
func refuseNonFixtureVault(root string, testBinary bool) error {
	if !testBinary || os.Getenv(TestRealVaultEnv) == "1" {
		return nil
	}
	// Go's t.TempDir creates its directory below GOTMPDIR when GOTMPDIR is
	// set, and the release gate's child sets GOTMPDIR apart from TMPDIR.
	for _, fixtureRoot := range []string{os.Getenv("WARP_TEST_ENV_PORTABLE_ROOT"), syntheticTestResourceRoot, os.TempDir(), os.Getenv("GOTMPDIR")} {
		if fixtureRoot != "" && pathWithin(root, fixtureRoot) {
			return nil
		}
	}
	return fmt.Errorf("%w: a test reads only a fixture vault, and %s is not one; set %s=1 to read it", ErrResourceNotFound, root, TestRealVaultEnv)
}

// Reports whether path is base or below it, after resolving the symlinks of
// each that exists.
func pathWithin(path string, base string) bool {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if resolved, err := filepath.EvalSymlinks(base); err == nil {
		base = resolved
	}
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func ConfigHomeRoot() string {
	warpConfigHome := os.Getenv("WARP_CONFIG_HOME")
	if warpConfigHome != "" {
		return warpConfigHome
	}
	return filepath.Join(WarpHome(), "config")
}

func ConfigHomes() []string {
	root := ConfigHomeRoot()
	return resolveMultiHome(root)
}

func SiteHomeRoot() string {
	warpSiteHome := os.Getenv("WARP_SITE_HOME")
	if warpSiteHome != "" {
		return warpSiteHome
	}
	return filepath.Join(WarpHome(), "site")
}

func SiteHomes() []string {
	root := SiteHomeRoot()
	return resolveMultiHome(root)
}

func resolveMultiHome(root string) []string {
	observations := resolveMultiHomeObserved(root)
	paths := make([]string, 0, len(observations))
	for index, observation := range observations {
		// Preserve the historical public shape: the literal root is always
		// returned, while invalid optional environment/all homes are omitted.
		if index == 0 || observation.err == nil {
			paths = append(paths, observation.path)
		}
	}
	return paths
}

type resolverHomeObservation struct {
	path string
	err  error
}

// Each lookup owns its optional credential-access check; no process identity
// changes or mutable global hooks are needed to inspect another service owner.
// A cached lookup also owns an observer of the entries its result depends on.
type resolverFilesystem struct {
	access   func(operation string, path string) error
	observer *resolverObserver
}

// Access refusals retain their cause but can never masquerade as file absence.
func (self resolverFilesystem) checkAccess(operation string, path string) error {
	if self.access != nil {
		if err := self.access(operation, path); err != nil {
			return fmt.Errorf("%w: %s access %s: %w", ErrResourceUnavailable, operation, path, err)
		}
	}
	return nil
}

// A following stat searches ancestors without requiring the target's contents.
func (self resolverFilesystem) stat(path string) (os.FileInfo, error) {
	if err := self.checkAccess("stat", path); err != nil {
		return nil, err
	}
	self.observer.observeParent(path)
	info, err := os.Stat(path)
	self.observer.observeError(err)
	return info, err
}

// A non-following stat has the same ancestor-search access contract.
func (self resolverFilesystem) lstat(path string) (os.FileInfo, error) {
	if err := self.checkAccess("stat", path); err != nil {
		return nil, err
	}
	self.observer.observeParent(path)
	info, err := os.Lstat(path)
	self.observer.observeError(err)
	return info, err
}

// Directory enumeration additionally requires read and search on the target.
func (self resolverFilesystem) readDir(path string) ([]os.DirEntry, error) {
	if err := self.checkAccess("read-dir", path); err != nil {
		return nil, err
	}
	self.observer.observeParent(path)
	// the listing is observed through the directory, before it is read
	self.observer.observeDirectory(path)
	entries, err := os.ReadDir(path)
	self.observer.observeError(err)
	return entries, err
}

// Only the underlying filesystem can establish absence. A callback may wrap
// an absence error while describing an unavailable credential/access proof.
func resolverPathAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrResourceUnavailable)
}

func resolveMultiHomeObserved(root string) []resolverHomeObservation {
	env, _ := Env()
	return (resolverFilesystem{}).resolveMultiHomeObserved(root, env)
}

// Observe literal, selected environment, and shared homes without consulting
// the process environment. An empty environment omits only its selected home.
func (self resolverFilesystem) resolveMultiHomeObserved(root string, env string) []resolverHomeObservation {
	// always search the literal dir first
	observations := []resolverHomeObservation{{path: root}}
	if info, err := self.stat(root); err == nil {
		if !info.Mode().IsDir() {
			observations[0].err = fmt.Errorf("%w: %s is not a directory", ErrResourceUnavailable, root)
		}
	} else if resolverPathAbsent(err) {
		if _, linkErr := self.lstat(root); linkErr == nil {
			observations[0].err = fmt.Errorf("%w: %s is a dangling filesystem entry", ErrResourceUnavailable, root)
		} else if !resolverPathAbsent(linkErr) {
			observations[0].err = fmt.Errorf("%w: inspect %s: %w", ErrResourceUnavailable, root, linkErr)
		}
	} else {
		observations[0].err = fmt.Errorf("%w: inspect %s: %w", ErrResourceUnavailable, root, err)
	}
	appendDirectory := func(path string) {
		if info, err := self.stat(path); err == nil {
			if info.Mode().IsDir() {
				observations = append(observations, resolverHomeObservation{path: path})
			} else {
				observations = append(observations, resolverHomeObservation{path: path, err: fmt.Errorf("%w: %s is not a directory", ErrResourceUnavailable, path)})
			}
		} else if resolverPathAbsent(err) {
			if _, linkErr := self.lstat(path); linkErr == nil {
				observations = append(observations, resolverHomeObservation{path: path, err: fmt.Errorf("%w: %s is a dangling filesystem entry", ErrResourceUnavailable, path)})
			} else if !resolverPathAbsent(linkErr) {
				observations = append(observations, resolverHomeObservation{path: path, err: fmt.Errorf("%w: inspect %s: %w", ErrResourceUnavailable, path, linkErr)})
			}
		} else {
			observations = append(observations, resolverHomeObservation{path: path, err: fmt.Errorf("%w: inspect %s: %w", ErrResourceUnavailable, path, err)})
		}
	}

	if env != "" {
		appendDirectory(filepath.Join(root, env))
	}

	// allow an all path even if there is not an active env
	appendDirectory(filepath.Join(root, "all"))
	return observations
}

func Host() (string, error) {
	host := os.Getenv("WARP_HOST")
	if host != "" {
		return host, nil
	}
	host, err := os.Hostname()
	if err == nil {
		return host, nil
	}
	return "", errors.New("WARP_HOST not set")
}

func RequireHost() string {
	host, err := Host()
	if err != nil {
		panic(err)
	}
	return host
}

func Block() (string, error) {
	block := os.Getenv("WARP_BLOCK")
	if block != "" {
		return block, nil
	}
	return "", errors.New("WARP_BLOCK not set")
}

func RequireBlock() string {
	block, err := Block()
	if err != nil {
		panic(err)
	}
	return block
}

func Service() (string, error) {
	service := os.Getenv("WARP_SERVICE")
	if service != "" {
		return service, nil
	}
	return "", errors.New("WARP_SERVICE not set")
}

func RequireService() string {
	service, err := Service()
	if err != nil {
		panic(err)
	}
	return service
}

func Domain() (string, error) {
	domain := os.Getenv("WARP_DOMAIN")
	if domain != "" {
		return domain, nil
	}
	return "", errors.New("WARP_DOMAIN not set")
}

func RequireDomain() string {
	domain, err := Domain()
	if err != nil {
		panic(err)
	}
	return domain
}

// service port -> host port
func HostPorts() (map[int]int, error) {
	if ports := os.Getenv("WARP_PORTS"); ports != "" {
		hostPorts := map[int]int{}
		portPairs := strings.Split(ports, ",")
		for _, portPair := range portPairs {
			parts := strings.Split(portPair, ":")
			if len(parts) != 2 {
				return nil, errors.New("Port pair must be service_port:host_port")
			}
			servicePort, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, err
			}
			hostPort, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, err
			}
			hostPorts[servicePort] = hostPort
		}
		return hostPorts, nil
	}
	return nil, errors.New("WARP_PORTS not set")
}

func RequireHostPorts() map[int]int {
	hostPorts, err := HostPorts()
	if err != nil {
		panic(err)
	}
	return hostPorts
}

func RequireServicePortToHostPort(servicePort int) int {
	hostPort, ok := RequireHostPorts()[servicePort]
	if !ok {
		panic(fmt.Errorf("Host port not found for service port %d", servicePort))
	}
	return hostPort
}

func HostIp() (ipv4 string, ipv6 string, err error) {
	ipv4 = os.Getenv("WARP_HOST_IPV4")
	ipv6 = os.Getenv("WARP_HOST_IPV6")

	// at least one of ipv4/ipv6 must be set
	if ipv4 == "" && ipv6 == "" {
		err = errors.New("WARP_HOST_IPV4 and WARP_HOST_IPV6 not set")
	}

	return
}

func RequireHostIp() (ipv4 string, ipv6 string) {
	var err error
	ipv4, ipv6, err = HostIp()
	if err != nil {
		panic(err)
	}
	return
}

func FwMark() (fwMark int, err error) {
	fwMarkStr := os.Getenv("WARP_FWMARK")

	if fwMarkStr == "" {
		return 0, nil
	}

	return strconv.Atoi(fwMarkStr)
}

func RequireFwMark() (fwMark int) {
	var err error
	fwMark, err = FwMark()
	if err != nil {
		panic(err)
	}
	return
}

func RequireListenIpPort(servicePort int) (ipv4 string, ipv6 string, hostPort int) {
	var err error
	ipv4, ipv6, err = HostIp()
	if err == nil {
		hostPort = RequireServicePortToHostPort(servicePort)
	} else {
		// disable ipv6
		ipv4 = "0.0.0.0"
		ipv6 = ""
		hostPort = servicePort
	}
	return
}

func HasPort(servicePort int) bool {
	_, _, err := HostIp()
	if err == nil {
		hostPorts, err := HostPorts()
		if err != nil {
			return false
		}
		_, ok := hostPorts[servicePort]
		return ok
	}
	return true
}

// these are the most efficient dest for this host to reach the target host
func Routes() map[string]string {
	routeStrs := map[string]string{}
	if routes, ok := GetSettings()["routes"]; ok {
		switch v := routes.(type) {
		case map[string]any:
			for host, route := range v {
				routeStr := route.(string)
				routeStrs[host] = routeStr
			}
		}
	}
	return routeStrs
}

// LanHosts maps each host on the lan routers' bridges to its lan ip. It reads
// the top-level lan_hosts block of the env's settings.yml, which is not part
// of the per-host settings that GetSettings merges.
func LanHosts() map[string]string {
	lanHostIps := map[string]string{}
	res, err := Config.SimpleResource("settings.yml")
	if err != nil {
		return lanHostIps
	}
	lanHosts, ok := res.Parse()["lan_hosts"].(map[string]any)
	if !ok {
		return lanHostIps
	}
	for host, lanHost := range lanHosts {
		if lanHostObj, ok := lanHost.(map[string]any); ok {
			if ip, ok := lanHostObj["ip"].(string); ok && ip != "" {
				lanHostIps[host] = ip
			}
		}
	}
	return lanHostIps
}

func Env() (string, error) {
	env := os.Getenv("WARP_ENV")
	if env != "" {
		return env, nil
	}
	return "", errors.New("WARP_ENV not set")
}

func RequireEnv() string {
	env, err := Env()
	if err != nil {
		panic(err)
	}
	return env
}

func Version() (string, error) {
	version := os.Getenv("WARP_VERSION")
	if version != "" {
		return version, nil
	}
	return "", errors.New("WARP_VERSION not set")
}

func RequireVersion() string {
	version, err := Version()
	if err != nil {
		panic(err)
	}
	return version
}

func ConfigVersion() (string, error) {
	configVersion := os.Getenv("WARP_CONFIG_VERSION")
	if configVersion != "" {
		return configVersion, nil
	}
	// try to use the warp version as the warp config version
	if version, err := Version(); err == nil {
		return version, nil
	}
	return "", errors.New("WARP_CONFIG_VERSION not set")
}

func RequireConfigVersion() string {
	configVersion, err := ConfigVersion()
	if err != nil {
		panic(err)
	}
	return configVersion
}

// Resolves resources under one mount type. Resolutions are cached per mount
// root, environment and resource path, and every lookup first re-observes the
// filesystem entries the cached resolution depended on (see
// resolverResolution), so a vault or config push, a new version directory, and
// a file added, replaced or rewritten in place are all seen by the next lookup
// without probing and logging the whole search again. Safe for concurrent use.
type Resolver struct {
	mountType MountType

	// Fixed at construction. Tests that need a controlled clock construct their own resolver.
	now func() time.Time

	stateLock sync.Mutex
	// relPath -> id -> override
	relPathOverrides map[string]map[int]resolverOverride
	keyResolutions   map[resolverResolutionKey]*resolverResolution
}

// A pushed value stands in for a resource. The serial is unique to one push.
type resolverOverride struct {
	value  []byte
	serial uint64
}

// Distinguishes pushes so a value derived from one is never reused for another.
var resolverOverrideSerial atomic.Uint64

func NewResolver(mountType MountType) *Resolver {
	return &Resolver{
		mountType:        mountType,
		now:              time.Now,
		relPathOverrides: map[string]map[int]resolverOverride{},
		keyResolutions:   map[resolverResolutionKey]*resolverResolution{},
	}
}

// locally override a file
// the returned pop removes the single override, but does not affect other overwrites
func (self *Resolver) PushSimpleResource(relPath string, value []byte) func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	overrides, ok := self.relPathOverrides[relPath]
	if !ok {
		overrides = map[int]resolverOverride{}
		self.relPathOverrides[relPath] = overrides
	}
	nextId := 0
	for id, _ := range overrides {
		nextId = max(nextId, id+1)
	}
	overrides[nextId] = resolverOverride{
		value:  value,
		serial: resolverOverrideSerial.Add(1),
	}

	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(overrides, nextId)
	}
}

// The latest pushed override for relPath, if any.
func (self *Resolver) override(relPath string) (resolverOverride, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	overrides, ok := self.relPathOverrides[relPath]
	if !ok || len(overrides) == 0 {
		return resolverOverride{}, false
	}
	maxId := 0
	for id, _ := range overrides {
		maxId = max(maxId, id)
	}
	return overrides[maxId], true
}

func (self *Resolver) ResourcePath(relPath string) (string, error) {
	paths, err := self.ResourcePaths(relPath)
	if err != nil {
		return "", err
	}
	return paths[0], nil
}

func (self *Resolver) ResourcePaths(relPath string) ([]string, error) {
	resolution := self.resolution(relPath)
	if resolution.err != nil {
		return nil, resolution.err
	}
	// callers own the returned slice; the cached one is shared
	return slices.Clone(resolution.paths), nil
}

// Identifies the current resolution and override of relPath, including a
// stable absence. Equal versions name the same resolution of unchanged
// filesystem entries and the same pushed override, so a value derived from the
// resource can be reused while its version is unchanged (see
// ResourceValueCache). An unavailable or recently changed resolution is never
// equal to another.
func (self *Resolver) resourceVersion(relPath string) ResourceVersion {
	override, _ := self.override(relPath)
	return ResourceVersion{
		resolution: self.resolution(relPath).generation,
		override:   override.serial,
	}
}

// The resolution of relPath: the cached one when every entry it observed is
// unchanged, else a new resolution of the current filesystem, which is cached
// when it is stable. A not-found resolution is cached like a found one, so an
// absent optional resource stays absent without searching again. An
// unavailable resolution is never cached and forgets any cached one.
func (self *Resolver) resolution(relPath string) *resolverResolution {
	if filepath.IsAbs(relPath) {
		panic("Resource path must be relative.")
	}

	var root string
	switch self.mountType {
	case MOUNT_TYPE_VAULT:
		root = VaultHomeRoot()
		// checked before the cache, so a refusal is never cached and an opt-in
		// that is withdrawn takes effect at the next lookup
		if err := refuseNonFixtureVault(root, testing.Testing()); err != nil {
			return &resolverResolution{err: err, generation: resolverGeneration.Add(1)}
		}
	case MOUNT_TYPE_CONFIG:
		root = ConfigHomeRoot()
	case MOUNT_TYPE_SITE:
		root = SiteHomeRoot()
	default:
		panic(fmt.Sprintf("Unknown mount type %s", self.mountType))
	}
	env, _ := Env()
	key := resolverResolutionKey{root: root, env: env, relPath: relPath}

	cached := func() *resolverResolution {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.keyResolutions[key]
	}()
	if cached != nil && cached.current() {
		return cached
	}

	// the clock is read before the first observation (see stable)
	start := self.now()
	observer := &resolverObserver{cacheable: true}
	filesystem := resolverFilesystem{observer: observer}
	paths, err := filesystem.resolveResourcePaths(filesystem.resolveMultiHomeObserved(root, env), relPath, self.mountType)
	resolution := &resolverResolution{
		paths:      paths,
		err:        err,
		generation: resolverGeneration.Add(1),
	}
	resolved := err == nil || errors.Is(err, ErrResourceNotFound) && !errors.Is(err, ErrResourceUnavailable)
	cacheable := resolved && observer.stable(start)
	if cacheable {
		resolution.observations = observer.observations
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !cacheable {
			delete(self.keyResolutions, key)
		} else if _, ok := self.keyResolutions[key]; ok || len(self.keyResolutions) < maxResolverResolutions {
			self.keyResolutions[key] = resolution
		}
	}()
	return resolution
}

// Resolve a resource beneath an explicit mount root and environment, using the
// same literal/environment/all and descending version precedence as Resolver.
// No process environment, resolver overrides, or cached state is consulted.
// Like Resolver.ResourcePaths, an absolute resource path panics and duplicate
// candidates are retained. The caller selects its vault/config/site root.
func ResolveResourcePaths(root string, env string, relPath string) ([]string, error) {
	return ResolveResourcePathsWithAccess(root, env, relPath, nil)
}

// Run an optional instance-local access check immediately before each lookup
// operation. "stat" requires search on path ancestors; "read-dir" additionally
// requires read and search on the directory itself. Callbacks must allow real
// missing targets to reach the filesystem, and must not mutate credentials or
// paths. Every refusal is unavailable, with its cause retained. Existing lookup
// precedence decides whether that failure blocks a later candidate; an already
// selected higher-priority resource is not rejected by a lower failure. Final
// selected-file read access and original inode custody remain caller-owned.
func ResolveResourcePathsWithAccess(root string, env string, relPath string, access func(operation string, path string) error) ([]string, error) {
	if filepath.IsAbs(relPath) {
		panic("Resource path must be relative.")
	}
	filesystem := resolverFilesystem{access: access}
	return filesystem.resolveResourcePaths(filesystem.resolveMultiHomeObserved(root, env), relPath, root)
}

// Share candidate ordering and error classification across explicit and
// process-configured resolution, including higher-priority lookup failures.
func (self resolverFilesystem) resolveResourcePaths(homes []resolverHomeObservation, relPath string, resourceScope string) ([]string, error) {
	paths := []string{}
	lookupErrs := []error{}
	recordError := func(err error) {
		if err != nil && (errors.Is(err, ErrResourceUnavailable) || !errors.Is(err, ErrResourceNotFound)) && len(paths) == 0 {
			lookupErrs = append(lookupErrs, err)
		}
	}

	for _, home := range homes {
		if home.err != nil {
			recordError(home.err)
			continue
		}
		path := filepath.Join(home.path, relPath)
		if exists, err := self.regularResourcePath(path); err != nil {
			recordError(err)
		} else if exists {
			paths = append(paths, path)
		}
	}

	// try versioned directories
	// at each directory level, look for <dir>/<version>, and iterate them in descending order
	for _, home := range homes {
		if home.err != nil {
			recordError(home.err)
			continue
		}
		if versionedPaths, err := self.versionLookup(home.path, strings.Split(relPath, "/")); err == nil {
			paths = append(paths, versionedPaths...)
		} else {
			recordError(err)
		}
	}

	if len(lookupErrs) != 0 {
		return nil, errors.Join(lookupErrs...)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w in %s (%s)", ErrResourceNotFound, resourceScope, relPath)
	}

	return paths, nil
}

// Prove an ordinary file or genuine absence under the same access observer;
// dangling entries and nonregular shapes remain unavailable.
func (self resolverFilesystem) regularResourcePath(path string) (bool, error) {
	if info, err := self.stat(path); err == nil {
		if info.Mode().IsRegular() {
			// a selected file is observed itself, so a rewrite in place is seen
			self.observer.observeFile(path, info)
			return true, nil
		}
		return false, fmt.Errorf("%w: %s is not a regular file", ErrResourceUnavailable, path)
	} else if resolverPathAbsent(err) {
		if _, linkErr := self.lstat(path); linkErr == nil {
			return false, fmt.Errorf("%w: %s is a dangling filesystem entry", ErrResourceUnavailable, path)
		} else if !resolverPathAbsent(linkErr) {
			return false, fmt.Errorf("%w: inspect %s: %w", ErrResourceUnavailable, path, linkErr)
		}
		return false, nil
	} else {
		return false, fmt.Errorf("%w: inspect %s: %w", ErrResourceUnavailable, path, err)
	}
}

func (self *Resolver) RequirePath(relPath string) string {
	path, err := self.ResourcePath(relPath)
	if err != nil {
		panic(err)
	}
	return path
}

func (self *Resolver) RequireBytes(relPath string) []byte {
	path := self.RequirePath(relPath)
	bytes, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return bytes
}

// list indexes are not in the res path; instead, each element is expanded out into the value array
func (self *Resolver) SimpleResource(relPath string) (*SimpleResource, error) {
	override, _ := self.override(relPath)
	resolution := self.resolution(relPath)
	path := relPath
	if resolution.err != nil {
		// an override does not need a backing file
		// (`PushSimpleResource` can stand in for a resource that does not
		// exist in the environment)
		if override.value == nil {
			return nil, resolution.err
		}
	} else {
		path = resolution.paths[0]
	}
	return &SimpleResource{
		path:     path,
		override: override.value,
		version: ResourceVersion{
			resolution: resolution.generation,
			override:   override.serial,
		},
	}, nil
}

func (self *Resolver) RequireSimpleResource(relPath string) *SimpleResource {
	simpleResource, err := self.SimpleResource(relPath)
	if err != nil {
		panic(err)
	}
	return simpleResource
}

type SimpleResource struct {
	path      string
	override  []byte
	version   ResourceVersion
	parsedObj map[string]any
}

// The resolution and override this resource was selected by (see
// Resolver.resourceVersion). A changed file is a different version.
func (self *SimpleResource) Version() ResourceVersion {
	return self.version
}

func (self *SimpleResource) bytes() ([]byte, error) {
	if self.override != nil {
		return self.override, nil
	}
	bytes, err := os.ReadFile(self.path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", self.path, err)
	}
	return bytes, nil
}

// ParseE is the error-returning counterpart to Parse. Request-time policy
// readers should use this form so an unavailable or malformed optional vault
// resource fails closed instead of turning one bad configuration file into a
// process panic.
func (self *SimpleResource) ParseE() (map[string]any, error) {
	if self.parsedObj != nil {
		return self.parsedObj, nil
	}
	bytes, err := self.bytes()
	if err != nil {
		return nil, err
	}
	obj := map[string]any{}
	if err := yaml.Unmarshal(bytes, &obj); err != nil {
		return nil, fmt.Errorf("%s: %w", self.path, err)
	}
	self.parsedObj = obj
	return obj, nil
}

func (self *SimpleResource) Parse() map[string]any {
	obj, err := self.ParseE()
	if err != nil {
		panic(err)
	}
	return obj
}

// The resource's raw content, or the pushed override when there is one, for a
// reader that parses its own format (the place list, whose loader lives in
// `geo`). Nothing is cached.
func (self *SimpleResource) BytesE() ([]byte, error) {
	return self.bytes()
}

// BytesE's content, panicking when the resource cannot be read.
func (self *SimpleResource) Bytes() []byte {
	bytes, err := self.BytesE()
	if err != nil {
		panic(err)
	}
	return bytes
}

// UnmarshalYamlE decodes a simple resource without panicking. It intentionally
// does not cache a typed value, so callers can use independent schema structs.
func (self *SimpleResource) UnmarshalYamlE(value any) error {
	bytes, err := self.bytes()
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(bytes, value); err != nil {
		return fmt.Errorf("%s: %w", self.path, err)
	}
	return nil
}

func (self *SimpleResource) UnmarshalYaml(value any) {
	if err := self.UnmarshalYamlE(value); err != nil {
		panic(err)
	}
}

func (self *SimpleResource) RequireBool(path ...string) bool {
	values := self.Bool(path...)
	if len(values) != 1 {
		panic(fmt.Sprintf("Must have one value (found %d).", len(values)))
	}
	value := values[0]
	glog.Infof("[env]%s[%s] = %t\n", self.path, strings.Join(path, " "), value)
	return value
}

func (self *SimpleResource) Bool(path ...string) []bool {
	values := []bool{}
	getAll(self.Parse(), path, &values)
	return values
}

func (self *SimpleResource) RequireInt(path ...string) int {
	values := self.Int(path...)
	if len(values) != 1 {
		panic(fmt.Sprintf("Must have one value (found %d).", len(values)))
	}
	value := values[0]
	glog.Infof("[env]%s[%s] = %d\n", self.path, strings.Join(path, " "), value)
	return value
}

func (self *SimpleResource) Int(path ...string) []int {
	values := []int{}
	getAll(self.Parse(), path, &values)
	return values
}

func (self *SimpleResource) RequireString(path ...string) string {
	values := self.String(path...)
	if len(values) != 1 {
		panic(fmt.Sprintf("Must have one value (found %d).", len(values)))
	}
	value := values[0]
	glog.Infof("[env]%s[%s] = %s\n", self.path, strings.Join(path, " "), MaskValue(value))
	return value
}

func (self *SimpleResource) String(path ...string) []string {
	values := []string{}
	getAll(self.Parse(), path, &values)
	translatedValues := []string{}
	for _, value := range values {
		translatedValues = append(translatedValues, translateString(value))
	}
	return translatedValues
}

func (self *SimpleResource) RequireStringList(path ...string) []string {
	return self.StringList(path...)
}

func (self *SimpleResource) StringList(path ...string) []string {
	return self.String(path...)
}

func translateString(value string) string {
	// simple "j2" format
	// {{ env:XXX }}
	translatedValue := string(templateExpression.ReplaceAllFunc([]byte(value), func(match []byte) []byte {
		envMatches := templateEnvExpression.FindStringSubmatch(string(match))
		if envMatches == nil {
			return match
		}
		key := envMatches[1]
		glog.Infof("[env]lookup env var \"%s\"\n", key)
		envValue := os.Getenv(key)
		if envValue == "" {
			panic(fmt.Sprintf("Missing env var %s", key))
		}
		return []byte(envValue)
	}))
	return translatedValue
}

// localEvaluationCredential keeps ordinary local and production resolution
// unchanged while allowing the isolated evaluator to rotate its throwaway
// PostgreSQL and Redis passwords for every stage. The container boundary sets
// the explicit mode bit; merely defining an override variable is insufficient.
func localEvaluationCredential(name string, configured string) string {
	if os.Getenv("APEX_CONTAINER_EVALUATION") != "true" {
		return configured
	}
	if env, err := Env(); err != nil || env != "local" {
		panic("evaluation credential overrides require WARP_ENV=local")
	}
	value := os.Getenv(name)
	if value == "" {
		panic(fmt.Sprintf("Missing evaluation credential %s", name))
	}
	return value
}

func getAll[T any](obj map[string]any, objPath []string, out *[]T) {
	values := []T{}

	var collect func(any, []string)
	collect = func(relValue any, relObjPath []string) {
		if len(relObjPath) == 0 {
			switch v := relValue.(type) {
			case T:
				values = append(values, v)
			case []any:
				for _, value := range v {
					collect(value, relObjPath)
				}
			}
		} else {
			switch v := relValue.(type) {
			case []any:
				for _, value := range v {
					collect(value, relObjPath)
				}
			case map[string]any:
				value, ok := v[relObjPath[0]]
				if !ok {
					return
				}
				collect(value, relObjPath[1:])
			}
		}
	}
	collect(obj, objPath)
	*out = values
}

// Search every directory component and descending version under the same
// scoped filesystem access contract, retaining original failure precedence.
func (self resolverFilesystem) versionLookup(root string, path []string) (returnPaths []string, returnErr error) {
	// try root/<path[0]>/<versioned path[1:]>
	// try root/<version>/<path[0]>/<versioned path[1:]>

	glog.Infof("[env]try %s, %s\n", root, path)

	if len(path) == 0 {
		returnErr = fmt.Errorf("%w: empty resource path", ErrResourceUnavailable)
		return
	}
	lookupErrs := []error{}
	recordError := func(err error) {
		if err != nil && (errors.Is(err, ErrResourceUnavailable) || !errors.Is(err, ErrResourceNotFound)) && len(returnPaths) == 0 {
			lookupErrs = append(lookupErrs, err)
		}
	}
	if len(path) == 1 {
		versionedPath := filepath.Join(root, path[0])
		if exists, err := self.regularResourcePath(versionedPath); err != nil {
			recordError(err)
		} else if exists {
			returnPaths = append(returnPaths, versionedPath)
		}
	}

	if 1 < len(path) {
		// the versioned version takes precedence
		if versionedPaths, err := self.versionLookup(filepath.Join(root, path[0]), path[1:]); err == nil {
			returnPaths = append(returnPaths, versionedPaths...)
		} else {
			recordError(err)
		}
	}

	type versionRootObservation struct {
		name string
		err  error
	}
	versionRoots := map[semver.Version]versionRootObservation{}
	if entries, err := self.readDir(root); err == nil {
		for _, entry := range entries {
			version, versionErr := semver.NewVersion(entry.Name())
			if versionErr != nil {
				continue
			}
			versionedRoot := filepath.Join(root, entry.Name())
			observation := versionRootObservation{name: entry.Name()}
			// Version-directory symlinks were not traversed by the original
			// DirEntry.IsDir contract. Keep that boundary explicit: following a
			// semver symlink can create recursive/cyclic version graphs.
			if info, statErr := self.lstat(versionedRoot); statErr == nil {
				if !info.Mode().IsDir() {
					observation.err = fmt.Errorf("%w: %s is not a directory", ErrResourceUnavailable, versionedRoot)
				}
			} else if resolverPathAbsent(statErr) {
				observation.err = fmt.Errorf("%w: %s disappeared during lookup", ErrResourceUnavailable, versionedRoot)
			} else {
				observation.err = fmt.Errorf("%w: inspect %s: %w", ErrResourceUnavailable, versionedRoot, statErr)
			}
			versionRoots[*version] = observation
		}
	} else if resolverPathAbsent(err) {
		if _, linkErr := self.lstat(root); linkErr == nil {
			recordError(fmt.Errorf("%w: %s is a dangling filesystem entry", ErrResourceUnavailable, root))
		} else if !resolverPathAbsent(linkErr) {
			recordError(fmt.Errorf("%w: inspect %s: %w", ErrResourceUnavailable, root, linkErr))
		}
	} else {
		recordError(fmt.Errorf("%w: list %s: %w", ErrResourceUnavailable, root, err))
	}
	versions := slices.Collect(maps.Keys(versionRoots))
	semverSortWithBuild(versions)
	for i := len(versions) - 1; 0 <= i; i -= 1 {
		observation := versionRoots[versions[i]]
		versionedRoot := filepath.Join(root, observation.name)
		glog.Infof("[env]test %s, %s\n", versionedRoot, path)
		if observation.err != nil {
			recordError(observation.err)
			continue
		}
		if versionedPaths, err := self.versionLookup(versionedRoot, path); err == nil {
			returnPaths = append(returnPaths, versionedPaths...)
		} else {
			recordError(err)
		}
	}

	if len(lookupErrs) != 0 {
		returnErr = errors.Join(lookupErrs...)
		returnPaths = nil
		return
	}
	if len(returnPaths) == 0 {
		returnErr = fmt.Errorf("%w under %s (%s)", ErrResourceNotFound, root, strings.Join(path, "/"))
		return
	}

	return
}

func semverSortWithBuild(versions []semver.Version) {
	slices.SortStableFunc(versions, func(a semver.Version, b semver.Version) int {
		if a.LessThan(b) {
			return -1
		}
		if b.LessThan(a) {
			return 1
		}
		if a.Metadata < b.Metadata {
			return -1
		}
		if b.Metadata < a.Metadata {
			return 1
		}
		return 0
	})
}
