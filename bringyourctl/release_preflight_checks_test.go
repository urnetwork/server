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
)

func TestReleaseDatabasePreflightOnlySelectsAndChecksIdentityHeadCapacity(t *testing.T) {
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
				if !bounded || time.Until(deadline) > 30*time.Second {
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
			err := checkReleaseDatabase(context.Background(), query, 9, "main", "release-reader", &output)
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
	err := checkReleaseDatabase(context.Background(), func(context.Context, string, ...any) error { return errors.New("password=never-log-me") }, 9, "main", "reader", io.Discard)
	if err == nil || strings.Contains(err.Error(), "never-log-me") {
		t.Fatalf("connection failure not redacted: %v", err)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("authorization probe wrote: %s", r.Method)
				}
				w.WriteHeader(status)
				fmt.Fprint(w, "credential-bearing provider body never-log-me")
			}))
			defer server.Close()
			err := releasePreflightGET(context.Background(), server.Client(), server.URL, "admin", "never-log-me", "", nil)
			if err == nil || strings.Contains(err.Error(), "never-log-me") {
				t.Fatalf("provider failure not redacted: %v", err)
			}
		})
	}
}

func TestReleasePreflightHTTPReadsExactAuthorizationSurfaces(t *testing.T) {
	var methods, paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer server.Close()
	var output bytes.Buffer
	err := checkReleaseHTTP(context.Background(), server.Client(), server.URL, "synthetic-grafana", server.URL+"/v3", "synthetic-brevo", &output)
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
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/user" {
				fmt.Fprintf(w, `{"isGrafanaAdmin":%t}`, admin)
				return
			}
			w.Header().Set("Location", "/would-mutate-if-followed")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		client := server.Client()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		err := checkReleaseHTTP(context.Background(), client, server.URL, "synthetic", server.URL, "synthetic", io.Discard)
		server.Close()
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
