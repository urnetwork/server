package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// This deliberately does not call DbVersion: DbVersion creates migration_audit.
// The narrow callback supports deterministic tests with no database or secrets.
type releasePreflightQuery func(context.Context, string, ...any) error

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

func checkReleaseDatabase(ctx context.Context, query releasePreflightQuery, expectedHead int, expectedDatabase, expectedUser string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
	if maxConnections-reserved-connected < 5 {
		return fmt.Errorf("Main database saturated: connections=%d maximum=%d reserved=%d; fewer than 5 ordinary slots remain", connected, maxConnections, reserved)
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

func releasePreflightGET(ctx context.Context, client *http.Client, url, user, password, apiKey string, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
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
		if err := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(result); err != nil {
			return fmt.Errorf("read-only authorization response is not valid JSON")
		}
	}
	return nil
}

func checkReleaseHTTP(ctx context.Context, client *http.Client, grafanaURL, grafanaPassword, brevoURL, brevoKey string, out io.Writer) error {
	var identity struct {
		Admin bool `json:"isGrafanaAdmin"`
	}
	if err := releasePreflightGET(ctx, client, grafanaURL+"/api/user", "admin", grafanaPassword, "", &identity); err != nil {
		return fmt.Errorf("Grafana: %w", err)
	}
	if !identity.Admin {
		return fmt.Errorf("Grafana preflight identity is not a server administrator")
	}
	if err := releasePreflightGET(ctx, client, grafanaURL+"/api/v1/provisioning/policies", "admin", grafanaPassword, "", nil); err != nil {
		return fmt.Errorf("Grafana provisioning read: %w", err)
	}
	if brevoKey == "" {
		return fmt.Errorf("Brevo API credential is absent")
	}
	if err := releasePreflightGET(ctx, client, brevoURL+"/smtp/templates?limit=1&offset=0", "", "", brevoKey, nil); err != nil {
		return fmt.Errorf("Brevo template read: %w", err)
	}
	fmt.Fprintln(out, "Grafana admin/provisioning and Brevo template authorization reads passed; no writes or messages sent")
	return nil
}
