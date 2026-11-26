package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// This deliberately does not call DbVersion: DbVersion creates migration_audit.
// The narrow callback supports deterministic tests with no database or secrets.
type releasePreflightQuery func(context.Context, string, ...any) error

// Tunables of the release preflight (QD-702), from defaultReleasePreflightSettings.
type releasePreflightSettings struct {
	// The whole run's bound, from the read-only database session to the last
	// check: 90 s, the database probe's 30 s and three authorization reads'
	// 20 s each.
	Timeout time.Duration
	// The database probe's bound, within Timeout: 30 s.
	DatabaseTimeout time.Duration
	// The fewest ordinary connection slots, past the reserved ones, the main
	// database must have free for the preflight to pass: 5. With fewer it is
	// saturated.
	DatabaseFreeConnectionMinCount int
	// The HTTP client's bound on one exchange: 20 s.
	HttpClientTimeout time.Duration
	// One authorization read's bound, its answer's body included: 20 s.
	HttpRequestTimeout time.Duration
	// The most bytes of an authorization read's JSON answer that are decoded:
	// 1 MiB. A longer answer does not decode, and the read fails.
	HttpResponseMaxByteCount int64
}

// The preflight's defaults, their only source. A fresh value each call.
func defaultReleasePreflightSettings() *releasePreflightSettings {
	return &releasePreflightSettings{
		Timeout:                        90 * time.Second,
		DatabaseTimeout:                30 * time.Second,
		DatabaseFreeConnectionMinCount: 5,
		HttpClientTimeout:              20 * time.Second,
		HttpRequestTimeout:             20 * time.Second,
		HttpResponseMaxByteCount:       1024 * 1024,
	}
}

const releaseIdentitySQL = `SELECT current_database(), current_user,
    COALESCE(inet_server_addr()::text, ''), pg_is_in_recovery(),
    current_setting('transaction_read_only'),
    current_setting('max_connections')::int,
    current_setting('superuser_reserved_connections')::int,
    (SELECT count(*)::int FROM pg_stat_activity),
    (SELECT count(*)::int FROM pg_stat_activity WHERE wait_event_type = 'Lock')`
const releaseAuditExistsSQL = `SELECT to_regclass('migration_audit') IS NOT NULL`
const releaseMigrationHeadSQL = `SELECT COALESCE(MAX(end_version_number), 0)
    FROM migration_audit WHERE status = 'success'`

func checkReleaseDatabase(ctx context.Context, settings *releasePreflightSettings, query releasePreflightQuery, expectedHead int, expectedDatabase, expectedUser string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, settings.DatabaseTimeout)
	defer cancel()
	var database, user, address, readOnly string
	var replica bool
	var maxConnections, reserved, connected, waiting int
	if err := query(ctx, releaseIdentitySQL, &database, &user, &address, &replica, &readOnly, &maxConnections, &reserved, &connected, &waiting); err != nil {
		return fmt.Errorf("Main database identity/connectivity query failed")
	}
	if replica || readOnly != "on" || database == "" || user == "" || address == "" {
		return fmt.Errorf("Main database identity/read-only contract failed (primary and read-only session required)")
	}
	if database != expectedDatabase || user != expectedUser {
		return fmt.Errorf("Main database/user does not match the selected LAN connection settings")
	}
	if maxConnections-reserved-connected < settings.DatabaseFreeConnectionMinCount {
		return fmt.Errorf("Main database saturated: connections=%d maximum=%d reserved=%d; fewer than %d ordinary slots remain", connected, maxConnections, reserved, settings.DatabaseFreeConnectionMinCount)
	}
	var exists bool
	if err := query(ctx, releaseAuditExistsSQL, &exists); err != nil || !exists {
		return fmt.Errorf("Main migration audit cannot be read; preflight never creates it")
	}
	var head int
	if err := query(ctx, releaseMigrationHeadSQL, &head); err != nil {
		return fmt.Errorf("Main migration head cannot be read")
	}
	if head > expectedHead {
		return fmt.Errorf("Main migration head %d is newer than this source's %d; select current release source", head, expectedHead)
	}
	fmt.Fprintf(out, "Main read-only identity: database=%s user=%s server=%s; migration head=%d source=%d; connections=%d/%d reserved=%d lock-waiters=%d\n", database, user, address, head, expectedHead, connected, maxConnections, reserved, waiting)
	return nil
}

func releasePreflightGET(ctx context.Context, settings *releasePreflightSettings, client *http.Client, url, user, password, apiKey string, result any) error {
	ctx, cancel := context.WithTimeout(ctx, settings.HttpRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("invalid preflight endpoint")
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	if apiKey != "" {
		req.Header.Set("api-key", apiKey)
	}
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("read-only authorization request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("read-only authorization request returned HTTP %d", response.StatusCode)
	}
	if result != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, settings.HttpResponseMaxByteCount)).Decode(result); err != nil {
			return fmt.Errorf("read-only authorization response is not valid JSON")
		}
	}
	return nil
}

func checkReleaseHTTP(ctx context.Context, settings *releasePreflightSettings, client *http.Client, grafanaURL, grafanaPassword, brevoURL, brevoKey string, out io.Writer) error {
	var identity struct {
		Admin bool `json:"isGrafanaAdmin"`
	}
	if err := releasePreflightGET(ctx, settings, client, grafanaURL+"/api/user", "admin", grafanaPassword, "", &identity); err != nil {
		return fmt.Errorf("Grafana: %w", err)
	}
	if !identity.Admin {
		return fmt.Errorf("Grafana preflight identity is not a server administrator")
	}
	if err := releasePreflightGET(ctx, settings, client, grafanaURL+"/api/v1/provisioning/policies", "admin", grafanaPassword, "", nil); err != nil {
		return fmt.Errorf("Grafana provisioning read: %w", err)
	}
	if brevoKey == "" {
		return fmt.Errorf("Brevo API credential is absent")
	}
	if err := releasePreflightGET(ctx, settings, client, brevoURL+"/smtp/templates?limit=1&offset=0", "", "", brevoKey, nil); err != nil {
		return fmt.Errorf("Brevo template read: %w", err)
	}
	fmt.Fprintln(out, "Grafana admin/provisioning and Brevo template authorization reads passed; no writes or messages sent")
	return nil
}

// The narrow view of dns.yml the user root check reads: its roots alone.
type releaseDnsRootsYaml struct {
	Roots []string `yaml:"roots"`
}

// §12.4's user root is written in two files, which a release keeps equal
// (QD-1407, owner decision 791): the user_root of the host table ur embeds
// (sidecar/api_hosts_v1.json, §18.9), which ur resolves the <net> of `.ur`, a
// git remote and an address under, and files GET /sidecar/label's rows under,
// from M2 and offline; and the root of the env's dns.yml
// (config/<env>/dns.yml, §12.6), which dns serves and api's /network/dns
// routes file labels under. The table compared is the one this binary
// embeds, which is the release's when bringyourctl is built from the
// release's source, as the migration head check already takes it to be.
//
// dns.yml is read for its roots alone, as the reserved label check reads
// services.yml's hosts: dns refuses a dns.yml whose signatures_per_second and
// the other numbers §12.6 leaves to the dns edges' benchmarks are unset, as
// main's are until then, and that is no reason to hold a release. api takes
// the user root only from a dns.yml naming one root (model.DnsUserRoot), so a
// dns.yml naming none or several fails, naming them and the table's. Two
// roots that differ fail naming both.
func checkReleaseDnsUserRoot(hostList *sidecar.SidecarHostList, dnsYamlBytes []byte, out io.Writer) error {
	var dnsYaml releaseDnsRootsYaml
	if err := yaml.Unmarshal(dnsYamlBytes, &dnsYaml); err != nil {
		return fmt.Errorf("DNS user root: dns.yml: %w", err)
	}
	// dns serves a root canonical, lowercase with a final dot
	// (dnsservice.LoadDnsSettings), which the table's root, validated
	// lowercase, leaves off
	dnsRoots := []string{}
	for _, root := range dnsYaml.Roots {
		dnsRoots = append(dnsRoots, strings.TrimSuffix(strings.ToLower(root), "."))
	}
	if len(dnsRoots) != 1 || dnsRoots[0] == "" {
		quotedRoots := []string{}
		for _, root := range dnsRoots {
			quotedRoots = append(quotedRoots, fmt.Sprintf("%q", root))
		}
		return fmt.Errorf("DNS user root: the embedded host table's user_root %q has no one root to match: dns.yml's roots are [%s], and api serves a user root only from a dns.yml naming one", hostList.UserRoot, strings.Join(quotedRoots, ", "))
	}
	if hostList.UserRoot != dnsRoots[0] {
		return fmt.Errorf("DNS user root: the embedded host table's user_root %q differs from dns.yml's root %q; a release keeps sidecar/api_hosts_v1.json's user_root equal to dns.yml's root", hostList.UserRoot, dnsRoots[0])
	}
	fmt.Fprintf(out, "DNS user root: the embedded host table's user_root is dns.yml's root %s\n", dnsRoots[0])
	return nil
}

// The env's dns.yml from config, which api reads its user root from. Config,
// not vault: it holds no secret.
func releaseConfigDnsYaml() ([]byte, error) {
	resource, err := server.Config.SimpleResource("dns.yml")
	if err != nil {
		return nil, err
	}
	return resource.BytesE()
}

// The user root check on what this process holds: the host table it embeds,
// and the env's dns.yml as dnsYaml reads it (releaseConfigDnsYaml). No fetch:
// an env with no dns.yml serves no root, so it fails.
func releasePreflightDnsUserRoot(dnsYaml func() ([]byte, error), out io.Writer) error {
	hostList, err := sidecar.SidecarHostListV1()
	if err != nil {
		return fmt.Errorf("DNS user root: the embedded host table: %w", err)
	}
	dnsYamlBytes, err := dnsYaml()
	if errors.Is(err, server.ErrResourceNotFound) && !errors.Is(err, server.ErrResourceUnavailable) {
		return fmt.Errorf("DNS user root: config has no dns.yml, so this env serves no root for the embedded host table's user_root %q", hostList.UserRoot)
	} else if err != nil {
		return fmt.Errorf("DNS user root: dns.yml does not read: %w", err)
	}
	return checkReleaseDnsUserRoot(hostList, dnsYamlBytes, out)
}
