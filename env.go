package server

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"

	"github.com/coreos/go-semver/semver"
	"gopkg.in/yaml.v3"
	// _ "github.com/golang/glog"
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
		fmt.Printf("[env]glog not configured from command line. Using default configuration.\n")

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
	// always search the literal dir first
	paths := []string{root}

	if env, err := Env(); err == nil {
		envPath := filepath.Join(root, env)
		if info, err := os.Stat(envPath); err == nil && info.Mode().IsDir() {
			paths = append(paths, envPath)
		}
	}

	// allow an all path even if there is not an active env
	allPath := filepath.Join(root, "all")
	if info, err := os.Stat(allPath); err == nil && info.Mode().IsDir() {
		paths = append(paths, allPath)
	}

	return paths
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

type Resolver struct {
	mountType MountType

	stateLock sync.Mutex
	// relPath -> id -> value
	relPathOverrides map[string]map[int][]byte
}

func NewResolver(mountType MountType) *Resolver {
	return &Resolver{
		mountType:        mountType,
		relPathOverrides: map[string]map[int][]byte{},
	}
}

// locally override a file
// the returned pop removes the single override, but does not affect other overwrites
func (self *Resolver) PushSimpleResource(relPath string, value []byte) func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	overrides, ok := self.relPathOverrides[relPath]
	if !ok {
		overrides = map[int][]byte{}
		self.relPathOverrides[relPath] = overrides
	}
	nextId := 0
	for id, _ := range overrides {
		nextId = max(nextId, id+1)
	}
	overrides[nextId] = value

	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(overrides, nextId)
	}
}

func (self *Resolver) ResourcePath(relPath string) (string, error) {
	paths, err := self.ResourcePaths(relPath)
	if err != nil {
		return "", err
	}
	return paths[0], nil
}

func (self *Resolver) ResourcePaths(relPath string) ([]string, error) {
	if filepath.IsAbs(relPath) {
		panic("Resource path must be relative.")
	}

	var homes []string
	switch self.mountType {
	case MOUNT_TYPE_VAULT:
		homes = VaultHomes()
	case MOUNT_TYPE_CONFIG:
		homes = ConfigHomes()
	case MOUNT_TYPE_SITE:
		homes = SiteHomes()
	default:
		panic(fmt.Sprintf("Unknown mount type %s", self.mountType))
	}

	paths := []string{}

	for _, home := range homes {
		path := filepath.Join(home, relPath)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			paths = append(paths, path)
		}
	}

	// try versioned directories
	// at each directory level, look for <dir>/<version>, and iterate them in descending order
	for _, home := range homes {
		if versionedPaths, err := versionLookup(home, strings.Split(relPath, "/")); err == nil {
			paths = append(paths, versionedPaths...)
		}
	}

	if len(paths) == 0 {
		return nil, errors.New(fmt.Sprintf("Resource not found in %s (%s)", self.mountType, relPath))
	}

	return paths, nil
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
	path, err := self.ResourcePath(relPath)
	if err != nil {
		return nil, err
	}
	override := func() []byte {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if overrides, ok := self.relPathOverrides[relPath]; ok {
			if len(overrides) == 0 {
				return nil
			}
			maxId := 0
			for id, _ := range overrides {
				maxId = max(maxId, id)
			}
			return overrides[maxId]
		}
		return nil
	}()
	return &SimpleResource{
		path:     path,
		override: override,
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
	parsedObj map[string]any
}

func (self *SimpleResource) Parse() map[string]any {
	if self.parsedObj != nil {
		return self.parsedObj
	}
	var bytes []byte
	var err error
	if self.override != nil {
		// use the override value
		bytes = self.override
	} else {
		bytes, err = os.ReadFile(self.path)
		if err != nil {
			panic(fmt.Errorf("%s: %s", self.path, err))
		}
	}
	obj := map[string]any{}
	err = yaml.Unmarshal(bytes, &obj)
	if err != nil {
		panic(fmt.Errorf("%s: %s", self.path, err))
	}
	self.parsedObj = obj
	return obj
}

func (self *SimpleResource) UnmarshalYaml(value any) {
	var bytes []byte
	var err error
	if self.override != nil {
		// use the override value
		bytes = self.override
	} else {
		bytes, err = os.ReadFile(self.path)
		if err != nil {
			panic(err)
		}
	}
	err = yaml.Unmarshal(bytes, value)
	if err != nil {
		panic(err)
	}
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
	glog.Infof("[env]%s[%s] = %s\n", self.path, strings.Join(path, " "), value)
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
	re := regexp.MustCompile("{{[^}]*}}")
	envRe := regexp.MustCompile("^{{\\s*env:([^}\\s]*)\\s*}}$")
	translatedValue := string(re.ReplaceAllFunc([]byte(value), func(match []byte) []byte {
		envMatches := envRe.FindStringSubmatch(string(match))
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

func versionLookup(root string, path []string) (returnPaths []string, returnErr error) {
	// try root/<path[0]>/<versioned path[1:]>
	// try root/<version>/<path[0]>/<versioned path[1:]>

	glog.Infof("[env]try %s, %s\n", root, path)

	if len(path) == 0 {
		returnErr = errors.New("Empty path")
		return
	}
	if len(path) == 1 {
		versionedPath := filepath.Join(root, path[0])
		if info, err := os.Stat(versionedPath); err == nil && info.Mode().IsRegular() {
			returnPaths = append(returnPaths, versionedPath)
		}
		//  else {
		//     return "", errors.New("Not found.")
		// }
	}

	if 1 < len(path) {
		// the versioned version takes precedence
		if versionedPaths, err := versionLookup(filepath.Join(root, path[0]), path[1:]); err == nil {
			returnPaths = append(returnPaths, versionedPaths...)
		}
	}

	versionNames := map[semver.Version]string{}
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				if version, err := semver.NewVersion(entry.Name()); err == nil {
					versionNames[*version] = entry.Name()
				}
			}
		}
	}
	versions := maps.Keys(versionNames)
	semverSortWithBuild(versions)
	for i := len(versions) - 1; 0 <= i; i -= 1 {
		versionedRoot := filepath.Join(root, versionNames[versions[i]])
		glog.Infof("[env]test %s, %s\n", versionedRoot, path)
		if info, err := os.Stat(versionedRoot); err == nil && info.Mode().IsDir() {
			if versionedPaths, err := versionLookup(versionedRoot, path); err == nil {
				returnPaths = append(returnPaths, versionedPaths...)
			}
		}
	}

	if len(returnPaths) == 0 {
		returnErr = errors.New("Not found.")
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
