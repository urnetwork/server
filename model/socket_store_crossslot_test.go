package model

// §22.4's slot rule for the sockets store: every script call, pipelined or not, names keys under one
// hash tag each carries, by sidecar.RedisKeysShareTag, over a run of every method; and no script builds a
// key, so the keys it was passed are all it touches. The runtime half on three masters, which would
// answer CROSSSLOT for a key slipping past this, is T3's cluster run (TEST-M1-08).

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
)

// A key built inside a script: a brace in a string literal, or a family's opening.
var testSocketScriptKeyBuilding = regexp.MustCompile(`"\{|\{(sg|svc|sn|sc)_`)

func TestSocketStoreScriptsBuildNoKeys(t *testing.T) {
	for name, source := range socketStoreScriptSources {
		if testSocketScriptKeyBuilding.MatchString(source) {
			t.Errorf("%s builds a key", name)
		}
		if strings.Contains(source, "KEYS[3]") {
			t.Errorf("%s reads a third key", name)
		}
		if !strings.Contains(source, "KEYS[1]") {
			t.Errorf("%s reads no key from KEYS", name)
		}
	}
}

func TestSocketStoreCallsShareOneTag(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		calls := []string{}
		sidecartest.RunSocketStoreSuite(t, func(clock sidecar.Clock, settings *socketsservice.SocketStoreSettings) socketsservice.SocketStore {
			store := NewSocketStore(clock, &SocketStoreSettings{Store: settings, CallTimeout: 10 * time.Second})
			store.scriptCallback = func(name string, keys []string) {
				if !sidecar.RedisKeysShareTag(keys) {
					t.Errorf("%s names %v", name, keys)
				}
				calls = append(calls, name)
			}
			return &testSuiteSocketStore{SocketStore: store}
		})
		seen := map[string]bool{}
		for _, name := range calls {
			seen[name] = true
		}
		for name := range socketStoreScripts {
			if !seen[name] {
				t.Errorf("the run never called %s", name)
			}
		}
	})
}
