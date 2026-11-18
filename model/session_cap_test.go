package model

import (
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server/session"
)

// The checked-in config/all/pro.yml carries the session cap the owner chose
// on 2026-11-18: 5,000 live sessions per network, not enforced until the next
// release ("Turning it on should be one config change"). Runs against
// URNETWORK_CONFIG_ROOT (a config worktree), or the checkout next to the
// server's.
func TestProSessionCapConfigFile(t *testing.T) {
	data := configRepoFile(t, filepath.Join("all", "pro.yml"))
	var y proConfigYaml
	connect.AssertEqual(t, nil, yaml.Unmarshal(data, &y))
	connect.AssertEqual(t, 5000, y.MaxSessions)
	connect.AssertEqual(t, false, y.EnforceSessionCap)
	// the switch sits beside the other rollout switches, which stay dark too
	connect.AssertEqual(t, false, y.EnforceConcurrentClients)
	connect.AssertEqual(t, false, y.EnforceFeatures)
}

// The cap reaches every mint through session's policy source, and the switch
// alone decides refusal.
func TestSessionCapPolicyFollowsPro(t *testing.T) {
	c := &ProConfig{MaxSessions: 5000}
	connect.AssertEqual(t, session.SessionCapPolicy{Live: 5000, Retained: session.NetworkSessionRetainedLimit, Enforce: false}, c.SessionCapPolicy())
	c.EnforceSessionCap = true
	connect.AssertEqual(t, true, c.SessionCapPolicy().Enforce)
	// an absent or unusable value is no cap, never a cap of zero that refuses
	// every mint
	connect.AssertEqual(t, 0, (&ProConfig{}).SessionCapPolicy().Live)
	connect.AssertEqual(t, 0, (&ProConfig{MaxSessions: -1}).SessionCapPolicy().Live)
}

// "Provide enabled", the one predicate the session cap excludes by, is a
// registered public or stream provide key (owner, 2026-11-18). A client that
// provides only to its own network or to friends and family is no provider.
func TestProvideEnabledIsPublicOrStream(t *testing.T) {
	key := []byte{1}
	connect.AssertEqual(t, false, provideKeysEnableProvide(map[ProvideMode][]byte{}))
	connect.AssertEqual(t, false, provideKeysEnableProvide(map[ProvideMode][]byte{ProvideModeNetwork: key}))
	connect.AssertEqual(t, false, provideKeysEnableProvide(map[ProvideMode][]byte{ProvideModeFriendsAndFamily: key}))
	connect.AssertEqual(t, false, provideKeysEnableProvide(map[ProvideMode][]byte{ProvideModeNetwork: key, ProvideModeFriendsAndFamily: key}))
	connect.AssertEqual(t, true, provideKeysEnableProvide(map[ProvideMode][]byte{ProvideModeStream: key}))
	connect.AssertEqual(t, true, provideKeysEnableProvide(map[ProvideMode][]byte{ProvideModePublic: key}))
	connect.AssertEqual(t, true, provideKeysEnableProvide(map[ProvideMode][]byte{ProvideModeNetwork: key, ProvideModeStream: key}))
	connect.AssertEqual(t, `EXISTS (SELECT 1 FROM provide_key WHERE provide_key.client_id = $1 AND provide_key.provide_mode = ANY('{3,4}'::int[]))`, clientProvideEnabledSql("$1"))
}
