package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

func TestReleaseDatabasePreflightOnlySelectsAndChecksIdentityHeadCapacity(t *testing.T) {
	settings := defaultReleasePreflightSettings()
	for _, test := range []struct {
		name    string
		mutate  func([]any) []any
		exists  bool
		head    int
		failure string
	}{
		{"healthy", nil, true, 9, ""},
		{"pending migration allowed", nil, true, 8, ""},
		{"wrong database", func(v []any) []any { v[0] = "other"; return v }, true, 9, "does not match"},
		{"wrong user", func(v []any) []any { v[1] = "other"; return v }, true, 9, "does not match"},
		{"writable connection refused", func(v []any) []any { v[4] = "off"; return v }, true, 9, "read-only contract"},
		{"replica refused", func(v []any) []any { v[3] = true; return v }, true, 9, "read-only contract"},
		{"saturated", func(v []any) []any { v[7] = 94; return v }, true, 9, "saturated"},
		{"missing audit", nil, false, 0, "never creates"},
		{"newer production schema", nil, true, 10, "newer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var queries []string
			query := func(ctx context.Context, sql string, dest ...any) error {
				queries = append(queries, sql)
				if !strings.HasPrefix(sql, "SELECT ") {
					t.Fatalf("non-read-only SQL: %s", sql)
				}
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) > settings.DatabaseTimeout {
					t.Fatal("database probe has no bounded deadline")
				}
				var values []any
				switch sql {
				case releaseIdentitySQL:
					values = []any{"main", "release-reader", "10.0.0.1", false, "on", 100, 3, 30, 2}
					if test.mutate != nil {
						values = test.mutate(values)
					}
				case releaseAuditExistsSQL:
					values = []any{test.exists}
				case releaseMigrationHeadSQL:
					if !test.exists {
						t.Fatal("read nonexistent migration table")
					}
					values = []any{test.head}
				default:
					t.Fatalf("unexpected diagnostic query: %s", sql)
				}
				for i, value := range values {
					reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(value))
				}
				return nil
			}
			var output bytes.Buffer
			err := checkReleaseDatabase(context.Background(), settings, query, 9, "main", "release-reader", &output)
			if test.failure == "" {
				if err != nil {
					t.Fatal(err)
				}
				if len(queries) != 3 || !strings.Contains(output.String(), "lock-waiters=2") {
					t.Fatal("missing head/capacity observations")
				}
			} else if err == nil || !strings.Contains(err.Error(), test.failure) {
				t.Fatalf("got %v, want %s", err, test.failure)
			}
		})
	}
}

func TestReleasePreflightRedactsConnectionAndHTTPFailures(t *testing.T) {
	settings := defaultReleasePreflightSettings()
	err := checkReleaseDatabase(context.Background(), settings, func(context.Context, string, ...any) error { return errors.New("password=never-log-me") }, 9, "main", "reader", io.Discard)
	if err == nil || strings.Contains(err.Error(), "never-log-me") {
		t.Fatalf("connection failure not redacted: %v", err)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("authorization probe wrote: %s", r.Method)
				}
				w.WriteHeader(status)
				fmt.Fprint(w, "credential-bearing provider body never-log-me")
			}))
			defer httpServer.Close()
			err := releasePreflightGET(context.Background(), settings, httpServer.Client(), httpServer.URL, "admin", "never-log-me", "", nil)
			if err == nil || strings.Contains(err.Error(), "never-log-me") {
				t.Fatalf("provider failure not redacted: %v", err)
			}
		})
	}
}

func TestReleasePreflightHTTPReadsExactAuthorizationSurfaces(t *testing.T) {
	var methods, paths []string
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.RequestURI())
		if r.ContentLength > 0 {
			t.Error("preflight sent a request body")
		}
		switch r.URL.Path {
		case "/api/user", "/api/v1/provisioning/policies":
			user, password, ok := r.BasicAuth()
			if !ok || user != "admin" || password != "synthetic-grafana" {
				t.Error("wrong Grafana authorization contract")
			}
		case "/v3/smtp/templates":
			if r.Header.Get("api-key") != "synthetic-brevo" {
				t.Error("wrong Brevo authorization contract")
			}
		default:
			t.Errorf("unexpected endpoint %s", r.URL)
		}
		fmt.Fprint(w, `{"isGrafanaAdmin":true}`)
	}))
	defer httpServer.Close()
	var output bytes.Buffer
	err := checkReleaseHTTP(context.Background(), defaultReleasePreflightSettings(), httpServer.Client(), httpServer.URL, "synthetic-grafana", httpServer.URL+"/v3", "synthetic-brevo", &output)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(methods, []string{"GET", "GET", "GET"}) || !reflect.DeepEqual(paths, []string{"/api/user", "/api/v1/provisioning/policies", "/v3/smtp/templates?limit=1&offset=0"}) {
		t.Fatalf("unexpected probes: %v %v", methods, paths)
	}
	if strings.Contains(output.String(), "synthetic-") {
		t.Fatal("credentials entered diagnostic output")
	}
}

func TestReleasePreflightRequiresGrafanaAdminAndNeverFollowsRedirects(t *testing.T) {
	for _, admin := range []bool{false, true} {
		httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/user" {
				fmt.Fprintf(w, `{"isGrafanaAdmin":%t}`, admin)
				return
			}
			w.Header().Set("Location", "/would-mutate-if-followed")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		client := httpServer.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		err := checkReleaseHTTP(context.Background(), defaultReleasePreflightSettings(), client, httpServer.URL, "synthetic", httpServer.URL, "synthetic", io.Discard)
		httpServer.Close()
		if err == nil {
			t.Fatal("insufficient authorization/redirect accepted")
		}
	}
}

func TestReleasePreflightAdapterRequiresReadOnlyBeforeOpeningMaintenancePool(t *testing.T) {
	adapter, err := os.ReadFile("release_preflight.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(adapter)
	readOnly := strings.Index(source, "server.RequirePgReadOnly()")
	connection := strings.Index(source, "server.MaintenanceDb(")
	if readOnly < 0 || connection < readOnly {
		t.Fatal("maintenance connection opens before read-only enforcement")
	}
	for _, forbidden := range []string{"server.DbVersion(", "server.DbMigrate(", "server.WithPgReadWrite("} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("mutating diagnostic path: %s", forbidden)
		}
	}
	if !strings.Contains(source, "server.OptNoRetry()") || !strings.Contains(source, "selected.Database, selected.User") {
		t.Fatal("missing bounded retry/selected connection identity contract")
	}
}

// §12.4's user root against dns.yml's (QD-1407). The host tables and dns.yml
// files are synthetic, on .example names, but for the embedded table's own
// root, which two tests read at run time.

// A host table in the form ur embeds, naming userRoot ("" names none).
func testReleaseHostList(t *testing.T, userRoot string) *sidecar.SidecarHostList {
	userRootJson := ""
	if userRoot != "" {
		userRootJson = fmt.Sprintf(`"user_root": %q, `, userRoot)
	}
	hostList, err := sidecar.ParseSidecarHostList([]byte(`{"version": 1, ` + userRootJson + `"hosts": [{"host": "api.platform.example", "tcp": 443, "need": "required: enrollment", "service": "api", "requirement": "required"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return hostList
}

// A dns.yml naming roots, which dns would serve if it named one.
func testReleaseDnsYaml(roots ...string) []byte {
	return []byte("roots: [" + strings.Join(roots, ", ") + "]\nns: [ns1, ns2]\nsignatures_per_second: 500\nrrl: {responses_per_second: 20}\ntcp: {connections_per_source: 8}\n")
}

// Equal roots pass however dns.yml writes its root, in capitals or with a
// final dot, and the line printed names the root. A dns.yml dns would refuse
// for its unset numbers, as main's is until the dns edges' benchmarks set
// them, passes on its root alone.
func TestReleasePreflightDnsUserRootEqualRootsPass(t *testing.T) {
	hostList := testReleaseHostList(t, "ur.example")
	dnsYamls := [][]byte{
		// no signatures_per_second, tcp.connections_per_source or rrl.responses_per_second
		[]byte("roots: [ur.example]\nns: [ns1, ns2]\n"),
		// keys of a later release
		append(testReleaseDnsYaml("ur.example"), []byte("synthetic_later_key: 1\n")...),
	}
	for _, root := range []string{"ur.example", "ur.example.", "UR.Example"} {
		dnsYamls = append(dnsYamls, testReleaseDnsYaml(root))
	}
	for _, dnsYaml := range dnsYamls {
		var out bytes.Buffer
		if err := checkReleaseDnsUserRoot(hostList, dnsYaml, &out); err != nil {
			t.Fatalf("dns.yml %q: %v", dnsYaml, err)
		}
		if out.String() != "DNS user root: the embedded host table's user_root is dns.yml's root ur.example\n" {
			t.Fatalf("printed %q", out.String())
		}
	}
}

// A root other than dns.yml's fails naming both, a parent or child of it
// included, as does a table naming none, and prints nothing.
func TestReleasePreflightDnsUserRootDifferingRootsFailNamingBoth(t *testing.T) {
	for _, c := range []struct {
		userRoot string
		dnsRoot  string
	}{
		{userRoot: "ur.example", dnsRoot: "other.example"},
		{userRoot: "ur.example", dnsRoot: "net.ur.example"},
		{userRoot: "net.ur.example", dnsRoot: "ur.example"},
		{userRoot: "", dnsRoot: "ur.example"},
	} {
		var out bytes.Buffer
		err := checkReleaseDnsUserRoot(testReleaseHostList(t, c.userRoot), testReleaseDnsYaml(c.dnsRoot), &out)
		want := fmt.Sprintf("the embedded host table's user_root %q differs from dns.yml's root %q", c.userRoot, c.dnsRoot)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("user_root %q against dns.yml's %q answered %v", c.userRoot, c.dnsRoot, err)
		}
		if out.Len() != 0 {
			t.Fatalf("a failed check printed %q", out.String())
		}
	}
}

// A dns.yml naming no root or several fails naming them and the table's, even
// when one of several is the table's, since api then serves no user root; a
// dns.yml that does not parse fails too, and none prints.
func TestReleasePreflightDnsUserRootRefusesNoRootOrSeveral(t *testing.T) {
	hostList := testReleaseHostList(t, "ur.example")
	for _, c := range []struct {
		dnsYaml []byte
		want    string
	}{
		{dnsYaml: testReleaseDnsYaml("ur.example", "other.example"), want: `DNS user root: the embedded host table's user_root "ur.example" has no one root to match: dns.yml's roots are ["ur.example", "other.example"]`},
		{dnsYaml: testReleaseDnsYaml(), want: `user_root "ur.example" has no one root to match: dns.yml's roots are []`},
		{dnsYaml: []byte("ns: [ns1, ns2]\n"), want: `user_root "ur.example" has no one root to match: dns.yml's roots are []`},
		{dnsYaml: []byte("roots: ['']\n"), want: `user_root "ur.example" has no one root to match: dns.yml's roots are [""]`},
		{dnsYaml: []byte("roots: [ur.example\n"), want: "DNS user root: dns.yml: "},
		{dnsYaml: []byte("roots: ur.example\n"), want: "DNS user root: dns.yml: "},
	} {
		var out bytes.Buffer
		err := checkReleaseDnsUserRoot(hostList, c.dnsYaml, &out)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("dns.yml %q answered %v, want %s", c.dnsYaml, err, c.want)
		}
		if out.Len() != 0 {
			t.Fatalf("a failed check printed %q", out.String())
		}
	}
}

// The command's own sources: the host table this binary embeds, which must
// name a user root, against the env's dns.yml read through config. A dns.yml
// naming the table's root passes; one naming another fails naming both.
func TestReleasePreflightDnsUserRootReadsTheEmbeddedTableAndConfigDnsYaml(t *testing.T) {
	embedded, err := sidecar.SidecarHostListV1()
	if err != nil {
		t.Fatal(err)
	}
	if embedded.UserRoot == "" {
		t.Fatal("the embedded host table names no user root for a release to keep equal to dns.yml's")
	}
	popEqual := server.Config.PushSimpleResource("dns.yml", testReleaseDnsYaml(embedded.UserRoot))
	defer popEqual()
	var out bytes.Buffer
	if err := releasePreflightDnsUserRoot(releaseConfigDnsYaml, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "DNS user root: the embedded host table's user_root is dns.yml's root "+embedded.UserRoot+"\n" {
		t.Fatalf("printed %q", out.String())
	}
	popOther := server.Config.PushSimpleResource("dns.yml", testReleaseDnsYaml("other-root.example"))
	defer popOther()
	err = releasePreflightDnsUserRoot(releaseConfigDnsYaml, io.Discard)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("user_root %q differs from dns.yml's root %q", embedded.UserRoot, "other-root.example")) {
		t.Fatalf("another root answered %v", err)
	}
}

// An env whose config has no dns.yml serves no root, so it fails naming the
// table's; a dns.yml that does not read fails as unread, not as absent.
func TestReleasePreflightDnsUserRootFailsWithoutADnsYaml(t *testing.T) {
	embedded, err := sidecar.SidecarHostListV1()
	if err != nil {
		t.Fatal(err)
	}
	err = releasePreflightDnsUserRoot(func() ([]byte, error) {
		return nil, fmt.Errorf("dns.yml: %w", server.ErrResourceNotFound)
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("config has no dns.yml, so this env serves no root for the embedded host table's user_root %q", embedded.UserRoot)) {
		t.Fatalf("no dns.yml answered %v", err)
	}
	for _, readErr := range []error{
		fmt.Errorf("%w: %w", server.ErrResourceUnavailable, server.ErrResourceNotFound),
		errors.New("synthetic read failure"),
	} {
		err := releasePreflightDnsUserRoot(func() ([]byte, error) {
			return nil, readErr
		}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "DNS user root: dns.yml does not read: ") {
			t.Fatalf("%v answered %v", readErr, err)
		}
	}
}

// The command checks the user root on config's dns.yml.
func TestReleasePreflightAdapterChecksTheDnsUserRoot(t *testing.T) {
	adapter, err := os.ReadFile("release_preflight.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(adapter), "releasePreflightDnsUserRoot(releaseConfigDnsYaml, os.Stdout)") {
		t.Fatal("the command does not check the user root against config's dns.yml")
	}
}
