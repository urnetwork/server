package bringyour

import (
	"sync"
	"os"
	"os/user"
	"path"
	"strings"
	"errors"
	"fmt"
	"regexp"

	"golang.org/x/mod/semver"

	"gopkg.in/yaml.v3"
)

// current env
// env resolver lock into env on first use


// find latest keys dir
// find latest keys-red dir

// resolver object, lock into the latest dir on first use
// keys resolver
// keys-red resolver


// resolver parse yml, give it a schema struct
// https://github.com/go-yaml/yaml

// resolver resolve constant value
// can be envvar:NAME to lookup an env var. if not exists, error


type KeysType string
const (
	KeysTypeKeys KeysType = "keys"
	KeysTypeKeysRed KeysType = "keys-red"
)


type EnvResolver struct {
	mutex sync.Mutex
	resolved bool
	envName *string
	version *string
}

func NewEnvResolver() *EnvResolver {
	return &EnvResolver{}
}

func (self *EnvResolver) resolve() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if !self.resolved {
		envName := LatestEnv()
		version := LatestVersion()
		self.envName = &envName
		self.version = &version
		Logger().Printf("Env: resolved env is \"%s\" and version \"%s\"", envName, version)
		self.resolved = true
	}
}

func (self *EnvResolver) EnvName() string {
	self.resolve()
	return *self.envName
}

func (self *EnvResolver) Version() string {
	self.resolve()
	return *self.version
}


type Resolver struct {
	keysType KeysType
	mutex sync.Mutex
	basePath *string
}
func NewResolver(keysType KeysType) *Resolver {
	return &Resolver{
		keysType: keysType,
	}
}
func (self *Resolver) BasePath() string {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.basePath == nil {
		basePath := LatestBasePath(self.keysType)
		self.basePath = &basePath
		Logger().Printf("Env: %s %s resolved base path is \"%s\"", Env.EnvName(), self.keysType, basePath)
	}
	return *self.basePath
}
func (self *Resolver) HasNewer() bool {
	return self.BasePath() != LatestBasePath(self.keysType)
}
func (self *Resolver) ResourcePath(relPath string) (string, error) {
	basePath := self.BasePath()
	resPath := path.Join(basePath, relPath)
	if fileInfo, err := os.Stat(resPath); err == nil && fileInfo.Mode().IsRegular() {
		return resPath, nil
	}

	// common files across all versions/envs (depending on base)
	resPath = path.Join(basePath, "..", relPath)
	Logger().Printf("TEST2 %s\n", resPath)
	if fileInfo, err := os.Stat(resPath); err == nil && fileInfo.Mode().IsRegular() {
		return resPath, nil
	}

	return "", errors.New("Resource not found.")
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
	parsedObj *map[string]interface{}
}
func (self *SimpleResource) Parse() map[string]interface{} {
	if self.parsedObj != nil {
		return *self.parsedObj
	}
	bytes, err := os.ReadFile(self.resPath)
	if err != nil {
		panic(err)
	}
	obj := map[string]interface{}{}
	yaml.Unmarshal(bytes, &obj)
	self.parsedObj = &obj
	return obj
}
// gets or raises
func (self *SimpleResource) RequireInt(resPath ...string) int {
	values := []int{}
	GetAll(self.Parse(), resPath, &values)
	if len(values) != 1 {
		panic(fmt.Sprintf("Must have one value (found %d).", len(values)))
	}
	value := values[0]
	Logger().Printf("Env: %s[%s] = %d\n", self.resPath, strings.Join(resPath, " "), value)
	return value
}
// gets or raises
func (self *SimpleResource) RequireString(resPath ...string) string {
	values := []string{}
	GetAll(self.Parse(), resPath, &values)
	if len(values) != 1 {
		panic(fmt.Sprintf("Must have one value (found %d).", len(values)))
	}
	value := values[0]
	value = self.TranslateString(value)
	Logger().Printf("Env: %s[%s] = %s\n", self.resPath, strings.Join(resPath, " "), value)
	return value
}
func (self *SimpleResource) TranslateString(value string) string {
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


func GetAll[T any](obj map[string]interface{}, objPath []string, out *[]T) {
	values := []T{}

	var collect func(interface{}, []string)
	collect = func(relValue interface{}, relObjPath []string) {
		if len(relObjPath) == 0 {
			switch v := relValue.(type) {
				case T:
					values = append(values, v)
				// fixme convert to T?
			}
		} else {
			switch v := relValue.(type) {
			case []interface{}:
				for _, value := range v {
					collect(value, relObjPath)
				}
			case map[string]interface{}:
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



var Env = NewEnvResolver()

var Keys = NewResolver(KeysTypeKeys)
var KeysRed = NewResolver(KeysTypeKeysRed)


/*
two common deployment cases:

1. local development. All repos are checked out under ~/bringyour (or $BRINGYOUR_HOME)
2. container deployment. Each container has a secure storage mounted at /srv/bringyour ($KEYS_HOME)

*/


var DefaultBringYourHome = func()(string) {
	user, _ := user.Current()
	return path.Join(user.HomeDir, "bringyour")
}()
var DefaultKeysHome = "/srv/bringyour"
var DefaultEnv = "local"
var DefaultVersion = "0.0.0-local"


func BringYourHome() string {
	bringYourHome := os.Getenv("BRINGYOUR_HOME")
	if bringYourHome != "" {
		return bringYourHome
	}
	return DefaultBringYourHome
}

func KeysHome() string {
	keysHome := os.Getenv("KEYS_HOME")
	if keysHome != "" {
		return keysHome
	}
	return DefaultKeysHome
}


func LatestEnv() string {
	// env var BRINGYOUR_HOME
	// (default ~/bringyour)
	// look for $BRINGYOUR_HOME/.env
	// if not, default "local"

	envPath := path.Join(BringYourHome(), ".env")
	bytes, err := os.ReadFile(envPath)
	if err == nil {
		return strings.TrimSpace(string(bytes))
	}
	return DefaultEnv
}

func LatestVersion() string {
	envPath := path.Join(BringYourHome(), ".version")
	bytes, err := os.ReadFile(envPath)
	if err == nil {
		return strings.TrimSpace(string(bytes))
	}
	return DefaultVersion
}

func LatestBasePath(keysType KeysType) string {
	// env var KEYS_HOME (this should be set in the container to some mounted dir)
	// (default /srv/bringyour)
	// env var BRINGYOUR_HOME
	// (default ~/bringyour)
	// env keys home is $HOME/$KEY/$env (multienv) or $HOME/$KEY (single env)

	// look for a versioned dir, $ENVKEY/<version> for max version (semver)
	// look for an unversioned dir $ENVKEY
		
	keysHome := KeysHome()
	if _, err := os.Stat(keysHome); os.IsNotExist(err) {
		keysHome = BringYourHome()
	}

	// look for multienv
	keysTypeHome := path.Join(keysHome, string(keysType), Env.EnvName())
	Logger().Printf("TEST %s\n", keysTypeHome)
	if fileInfo, err := os.Stat(keysTypeHome); os.IsNotExist(err) || !fileInfo.IsDir() {
		// single env
		keysTypeHome = path.Join(keysHome, string(keysType))
	}

	if versionPath, err := latestVersionPath(keysTypeHome); err == nil {
		// versioned
		return versionPath
	}
	// unversioned
	return keysTypeHome
}

func latestVersionPath(basePath string) (string, error) {
	dirEntries, err := os.ReadDir(basePath)
	if err != nil {
		return "", err
	}
	// look for semver names
	semverDirs := []string{}
	for _, dirEntry := range dirEntries {
		if dirEntry.Type().IsDir() && semver.IsValid(dirEntry.Name()) {
			semverDirs = append(semverDirs, dirEntry.Name())
		}
	}
	if 0 < len(semverDirs) {
		semver.Sort(semverDirs)
		versionPath := path.Join(basePath, semverDirs[len(semverDirs) - 1])
		return versionPath, nil
	}

	return "", errors.New("No semver dirs found.")
}
