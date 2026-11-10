package rootnames

// The connect program's tls names: config/<env>/tls.yml, which server/tls.go
// reads for connect's transports and for the proxy. In no environment may they
// cover a root or a name under one: a user name's tls ends in its own sidecar
// (sidecar DESIGN X1, §13.4), and the root's apex is answered by the lb's http
// servers, which redirect it to ur.io (§12.4). The platform's names on
// bringyour.com stay covered.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
)

// Platform names every environment's connect program serves.
var platformTlsHosts = []string{"bringyour.com", "api.bringyour.com", "connect.bringyour.com"}

// Whether name is domain or a name under it.
func isNameUnder(name string, domain string) bool {
	return name == domain || strings.HasSuffix(name, "."+domain)
}

// The names an allowed_hosts entry covers, as server/tls.go matches it, tested
// against one name and everything under it: an exact entry covers itself,
// "*.<base>" each name one label under base, and "**.<base>" every name
// under base. So an entry covers the root or a name under it when its base is
// the root or under it, and also, for "*.", when its base is the root's
// parent, and, for "**.", when its base is any domain above the root.
func tlsAllowedHostCovers(allowedHost string, root string) bool {
	allowedHost = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(allowedHost)), ".")
	root = strings.TrimSuffix(strings.ToLower(root), ".")
	switch {
	case strings.HasPrefix(allowedHost, "**."):
		base := strings.TrimPrefix(allowedHost, "**.")
		return isNameUnder(base, root) || isNameUnder(root, base)
	case strings.HasPrefix(allowedHost, "*."):
		base := strings.TrimPrefix(allowedHost, "*.")
		_, rootParent, _ := strings.Cut(root, ".")
		return isNameUnder(base, root) || base == rootParent
	default:
		return isNameUnder(allowedHost, root)
	}
}

// Reads one tls.yml's allowed_hosts.
func readTlsAllowedHosts(t *testing.T, path string, data []byte) []string {
	t.Helper()
	tlsConfig := &struct {
		AllowedHosts []string `yaml:"allowed_hosts"`
	}{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(tlsConfig); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return tlsConfig.AllowedHosts
}

// The coverage rule on its own, against server/tls.go's matching.
func TestTlsAllowedHostCoverage(t *testing.T) {
	for _, test := range []struct {
		allowedHost string
		covers      bool
	}{
		{"user.example", true},
		{"api.user.example", true},
		{"*.user.example", true},
		{"**.user.example", true},
		{"**.api.user.example", true},
		{"*.example", true},
		{"**.example", true},
		{"USER.Example.", true},
		{"platform.example", false},
		{"*.platform.example", false},
		{"**.platform.example", false},
		{"superuser.example", false},
		{"*.org", false},
	} {
		if covers := tlsAllowedHostCovers(test.allowedHost, "user.example"); covers != test.covers {
			t.Errorf("%q covers the root: %t, want %t", test.allowedHost, covers, test.covers)
		}
	}
}

// Every environment's tls.yml loads through server/tls.go's own loader,
// covers no root of any environment, and still covers the platform names.
func TestTlsAllowedHostsCoverNoRoot(t *testing.T) {
	roots := readConfigRoots(t, rootConfigDir)
	paths, err := filepath.Glob(filepath.Join(rootConfigDir, "*", "tls.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no environment in %s has a tls.yml", rootConfigDir)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer server.Config.PushSimpleResource("tls.yml", data)()
			if _, err := server.NewTransportTlsFromConfig(server.DefaultTransportTlsSettings()); err != nil {
				t.Errorf("%s: the connect program's loader refuses it: %v", path, err)
			}
		}()
		allowedHosts := readTlsAllowedHosts(t, path, data)
		for _, allowedHost := range allowedHosts {
			for _, root := range roots {
				if tlsAllowedHostCovers(allowedHost, root) {
					t.Errorf("%s: %q lets the connect program serve the root %s or a name under it", path, allowedHost, root)
				}
			}
		}
		for _, platformHost := range platformTlsHosts {
			covered := false
			for _, allowedHost := range allowedHosts {
				if tlsAllowedHostServes(allowedHost, platformHost) {
					covered = true
				}
			}
			if !covered {
				t.Errorf("%s: no entry lets the connect program serve %s", path, platformHost)
			}
		}
	}
}

// Whether an allowed_hosts entry lets server/tls.go serve exactly one name:
// the name itself, "*." on its parent, or "**." on any domain above it.
func tlsAllowedHostServes(allowedHost string, hostName string) bool {
	allowedHost = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(allowedHost)), ".")
	_, parent, _ := strings.Cut(hostName, ".")
	switch {
	case strings.HasPrefix(allowedHost, "**."):
		return parent != "" && isNameUnder(parent, strings.TrimPrefix(allowedHost, "**."))
	case strings.HasPrefix(allowedHost, "*."):
		return parent != "" && parent == strings.TrimPrefix(allowedHost, "*.")
	default:
		return allowedHost == hostName
	}
}
