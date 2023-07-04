package bringyour

import (
    "os"
    "path/filepath"
    "strings"
    "errors"
    "fmt"
    "regexp"

    "golang.org/x/exp/maps"

    "gopkg.in/yaml.v3"
    "github.com/coreos/go-semver/semver"
)


/*
support two deployment cases:

1. local development. $WARP_HOME/{vault,config,site}/<env>
2. warp container deployment. Each container has mounted at $WARP_HOME/{vault,config,site}

*/


type MountType = string
const (
    MOUNT_TYPE_VAULT MountType = "vault"
    MOUNT_TYPE_CONFIG MountType = "config"
    MOUNT_TYPE_SITE MountType = "string"
)


var Vault = NewResolver(MOUNT_TYPE_VAULT)
var Config = NewResolver(MOUNT_TYPE_CONFIG)
var Site = NewResolver(MOUNT_TYPE_SITE)

var DefaultWarpHome = "/srv/warp"


func init() {
    settingsObj := GetSettings()
    Logger().Printf("Found settings %s\n", settingsObj)
    if envObj, ok := settingsObj["env_vars"]; ok {
        switch v := envObj.(type) {
        case map[string]any:
            for key, value := range v {
                switch w := value.(type) {
                case string:
                    os.Setenv(key, w)
                default:
                    Logger().Printf("Env settings unrecognized env value \"%s\"=\"%s\" (%T)\n", key, w, w)
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

        // use an all path only if there is an env
        allPath := filepath.Join(root, "all")
        if info, err := os.Stat(allPath); err == nil && info.Mode().IsDir() {
            paths = append(paths, allPath)
        }
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


func Env() (string, error) {
    env := os.Getenv("WARP_ENV")
    if env != "" {
        return env, nil
    }
    return "", errors.New("WARP_ENV not set")
}

func RequireEnv() string {
    env, err := Version()
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
}

func NewResolver(mountType MountType) *Resolver {
    return &Resolver{
        mountType: mountType,
    }
}

func (self *Resolver) ResourcePath(relPath string) (string, error) {
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

    for _, home := range homes {
        resPath := filepath.Join(home, relPath)
        if info, err := os.Stat(resPath); err == nil && info.Mode().IsRegular() {
            return resPath, nil
        }
    }

    // try versioned directories
    // at each directory level, look for <dir>/<version>, and iterate them in descending order
    for _, home := range homes {
        if path, err := versionLookup(home, strings.Split(relPath, "/")); err == nil {
            return path, nil
        }
    }

    return "", errors.New(fmt.Sprintf("Resource not found in %s (%s)", self.mountType, relPath))
}

func (self *Resolver) RequirePath(relPath string) string {
    resPath, err := self.ResourcePath(relPath)
    if err != nil {
        panic(err)
    }
    return resPath
}

func (self *Resolver) RequireBytes(relPath string) []byte {
    resPath := self.RequirePath(relPath)
    bytes, err := os.ReadFile(resPath)
    if err != nil {
        panic(err)
    }
    return bytes
}

// list indexes are not in the res path; instead, each element is expanded out into the value array
func (self *Resolver) SimpleResource(relPath string) (*SimpleResource, error) {
    resPath, err := self.ResourcePath(relPath)
    if err != nil {
        return nil, err
    }
    return &SimpleResource{
        resPath: resPath,
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
    resPath string
    parsedObj *map[string]any
}

func (self *SimpleResource) Parse() map[string]any {
    if self.parsedObj != nil {
        return *self.parsedObj
    }
    var bytes []byte
    var err error
    bytes, err = os.ReadFile(self.resPath)
    if err != nil {
        panic(err)
    }
    obj := map[string]any{}
    err = yaml.Unmarshal(bytes, &obj)
    if err != nil {
        panic(err)
    }
    self.parsedObj = &obj
    return obj
}

func (self *SimpleResource) RequireInt(resPath ...string) int {
    values := []int{}
    getAll(self.Parse(), resPath, &values)
    if len(values) != 1 {
        panic(fmt.Sprintf("Must have one value (found %d).", len(values)))
    }
    value := values[0]
    Logger().Printf("Env: %s[%s] = %d\n", self.resPath, strings.Join(resPath, " "), value)
    return value
}

func (self *SimpleResource) RequireString(resPath ...string) string {
    values := []string{}
    getAll(self.Parse(), resPath, &values)
    if len(values) != 1 {
        panic(fmt.Sprintf("Must have one value (found %d).", len(values)))
    }
    value := translateString(values[0])
    Logger().Printf("Env: %s[%s] = %s\n", self.resPath, strings.Join(resPath, " "), value)
    return value
}


func translateString(value string) string {
    // simple "j2" format
    // {{ env:XXX }}
    re := regexp.MustCompile("{{[^}]*}}")
    envRe := regexp.MustCompile("^{{\\s*env:([^}\\s]*)\\s*}}$")
    translatedValue := string(re.ReplaceAllFunc([]byte(value), func(match []byte)([]byte) {
        envMatches := envRe.FindStringSubmatch(string(match))
        if envMatches == nil {
            return match
        }
        key := envMatches[1]
        Logger().Printf("Env: lookup env var \"%s\"\n", key)
        envValue := os.Getenv(key)
        if envValue == "" {
            return match
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
                // fixme convert to T?
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


func versionLookup(root string, path []string) (string, error) {
    // try root/<path[0]>/<versioned path[1:]>
    // try root/<version>/<path[0]>/<versioned path[1:]>

    Logger().Printf("Try %s, %s\n", root, path)

    if len(path) == 0 {
        panic("Empty path")
    }
    if len(path) == 1 {
        versionedPath := filepath.Join(root, path[0])
        if info, err := os.Stat(versionedPath); err == nil && info.Mode().IsRegular() {
            return versionedPath, nil
        } else {
            return "", errors.New("Not found.")
        }
    }

    // the versioned version takes precedence
    if path, err := versionLookup(filepath.Join(root, path[0]), path[1:]); err == nil {
        return filepath.Join(path), nil
    }


    versionNames := map[*semver.Version]string{}
    if entries, err := os.ReadDir(root); err == nil {
        for _, entry := range entries {
            if entry.IsDir() {
                if version, err := semver.NewVersion(entry.Name()); err == nil {
                    versionNames[version] = entry.Name()
                }
            }
        }
    }
    versions := maps.Keys(versionNames)
    semver.Sort(versions)
    for i := len(versions) - 1; 0 <= i; i -= 1 {
        versionedRoot := filepath.Join(root, versionNames[versions[i]], path[0])
        Logger().Printf("Test %s, %s\n", versionedRoot, path[1:])
        if info, err := os.Stat(versionedRoot); err == nil && info.Mode().IsDir() {
            path, err := versionLookup(versionedRoot, path[1:])
            if err == nil {
                return path, nil
            }
        }
    }

    return "", errors.New("Not found.")
}

