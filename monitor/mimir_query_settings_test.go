// Source-equivalence and private generation controls for the native query role.
package monitor

import (
	"context"
	"reflect"
	"testing"

	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

// These synthetic YAML pairs are also the independent native QueryUser oracle
// inputs. Ordinary order wins over vault order; an eligible warpctl wins both.
func TestMimirQueryNativeUserSelection(t *testing.T) {
	for _, testCase := range []struct {
		name, ordinary, secret, username, password string
	}{
		{name: "preferred", ordinary: "users: [{name: first, roles: [query]}, {name: warpctl, roles: [query]}]", secret: "users: [{name: first, password: private-first}, {name: warpctl, password: private-preferred}]", username: "warpctl", password: "private-preferred"},
		{name: "ordinary-order", ordinary: "users: [{name: first, roles: [query]}, {name: second, roles: [query]}]", secret: "users: [{name: second, password: private-second}, {name: first, password: private-first}]", username: "first", password: "private-first"},
		{name: "blank-preferred", ordinary: "users: [{name: warpctl, roles: [query]}, {name: second, roles: [query]}]", secret: "users: [{name: warpctl, password: ''}, {name: second, password: private-second}]", username: "second", password: "private-second"},
		{name: "push-not-query", ordinary: "users: [{name: warpctl, roles: [push]}, {name: reader, roles: [push, query]}]", secret: "users: [{name: warpctl, password: private-push}, {name: reader, password: 'private:query'}]", username: "reader", password: "private:query"},
		{name: "missing-secret", ordinary: "users: [{name: reader, roles: [query]}]", secret: "users: []"},
		{name: "no-query-role", ordinary: "users: [{name: reader, roles: [push]}]", secret: "users: [{name: reader, password: private-query}]"},
		{name: "ordinary-password", ordinary: "users: [{name: reader, roles: [query], password: private-query}]", secret: "users: [{name: reader, password: private-query}]"},
		{name: "ordinary-duplicate", ordinary: "users: [{name: reader, roles: [query]}, {name: reader, roles: [push]}]", secret: "users: [{name: reader, password: private-query}]"},
		{name: "ordinary-null", ordinary: "users: [null]", secret: "users: []"},
		{name: "ordinary-unnamed", ordinary: "users: [{roles: [query]}]", secret: "users: []"},
		{name: "secret-roles", ordinary: "users: [{name: reader, roles: [query]}]", secret: "users: [{name: reader, roles: [query], password: private-query}]"},
		{name: "secret-duplicate", ordinary: "users: [{name: reader, roles: [query]}]", secret: "users: [{name: reader, password: private-query}, {name: reader, password: private-other}]"},
		{name: "secret-null", ordinary: "users: [{name: reader, roles: [query]}]", secret: "users: [null]"},
		{name: "secret-unnamed", ordinary: "users: [{name: reader, roles: [query]}]", secret: "users: [{password: private-query}]"},
		{name: "secret-foreign", ordinary: "users: [{name: reader, roles: [query]}]", secret: "users: [{name: unknown, password: private-query}]"},
	} {
		var ordinary, secret, before mimirQueryConfig
		if yaml.Unmarshal([]byte(testCase.ordinary), &ordinary) != nil || yaml.Unmarshal([]byte(testCase.secret), &secret) != nil || yaml.Unmarshal([]byte(testCase.ordinary), &before) != nil {
			t.Fatal("invalid synthetic role fixture")
		}
		username, password, ok := mimirQueryUserFromConfigs(ordinary, secret)
		if username != testCase.username || password != testCase.password || ok != (testCase.username != "") || !reflect.DeepEqual(ordinary, before) {
			t.Errorf("%s changed native role selection or mutated the ordinary source", testCase.name)
		}
	}
}

// Native services domain priority is explicit primary, then sorted map keys.
func TestMimirQueryNativePrimaryDomain(t *testing.T) {
	for _, testCase := range []struct {
		services servicesYaml
		want     string
	}{
		{services: servicesYaml{Domain: "primary.example", Domains: map[string]string{"a.example": "", "z.example": ""}}, want: "primary.example"},
		{services: servicesYaml{Domains: map[string]string{"z.example": "", "a.example": ""}}, want: "a.example"},
		{services: servicesYaml{}, want: ""},
	} {
		if mimirQueryPrimaryDomain(testCase.services) != testCase.want {
			t.Error("native primary-domain priority changed")
		}
	}
}

// Exercise the actual server-owned resolvers with private in-memory overrides
// and empty temporary roots. No ambient resource or native Warp loader is used.
func TestMimirQueryOwnedResolverLoadAndRotation(t *testing.T) {
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_ENV", "")
	popOrdinary := server.Config.PushSimpleResource("grafana.yml", []byte("users: [{name: reader, roles: [query]}]"))
	defer popOrdinary()
	popSecret := server.Vault.PushSimpleResource("grafana.yml", []byte("users: [{name: reader, password: private-first}]"))
	defer popSecret()
	domain, username, password := loadMimirQuerySettings("synthetic", servicesYaml{Domain: "metrics.example"})
	if domain != "metrics.example" || username != "reader" || password != "private-first" {
		t.Fatal("owned resolver did not load the ordinary role and vault password")
	}
	popRotated := server.Vault.PushSimpleResource("grafana.yml", []byte("users: [{name: reader, password: private-rotated}]"))
	defer popRotated()
	_, _, password = loadMimirQuerySettings("synthetic", servicesYaml{Domain: "metrics.example"})
	if password != "private-rotated" {
		t.Fatal("owned resolver reused a superseded credential")
	}
	popMalformed := server.Config.PushSimpleResource("grafana.yml", []byte("users: [private-malformed"))
	defer popMalformed()
	domain, username, password = loadMimirQuerySettings("synthetic", servicesYaml{Domain: "metrics.example"})
	if domain != "" || username != "" || password != "" {
		t.Fatal("malformed ordinary config retained a partial credential")
	}
}

// Every effective native authority change is part of the existing in-memory
// comparison; it emits neither private values nor a content-derived hash.
func TestMimirQueryCredentialsParticipateInSettingsGeneration(t *testing.T) {
	settings := pickerTestSettings(t, "")
	for _, field := range []string{"domain", "username", "password"} {
		current := settings
		switch field {
		case "domain":
			current.Grafana.QueryDomain = "rotated.example"
		case "username":
			current.Grafana.QueryUsername = "rotated-reader"
		case "password":
			current.Grafana.QueryPassword = "private-rotated"
		}
		check := NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
		if same, err := check(context.Background(), settings); err != nil || same {
			t.Errorf("%s rotation escaped private settings comparison", field)
		}
	}
}
