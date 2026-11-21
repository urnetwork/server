package model

// PostgreSQL state shared by the monitoring, DNS readiness and restore paths (§12.6).

import (
	"context"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/sidecar/dnsservice"
)

// Store the latest verdict per source, ordering concurrent reports by their server receipt times.
func PutDnsCanaryVerdict(ctx context.Context, root string, verdict dnsservice.DnsCanaryVerdict) error {
	if !server.SidecarMg5Linked() {
		return ErrDnsUnlinked
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO dns_canary_verdict (root, source, serial, ok, report_time)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (root, source) DO UPDATE SET
			serial = EXCLUDED.serial, ok = EXCLUDED.ok, report_time = EXCLUDED.report_time
			WHERE dns_canary_verdict.report_time < EXCLUDED.report_time
			OR (dns_canary_verdict.report_time = EXCLUDED.report_time AND NOT EXCLUDED.ok)`,
			dnsLabelRoot(root), verdict.Source, dnsSerialColumn(verdict.Serial), verdict.Ok, verdict.ReportTime.UTC()))
	})
	return nil
}

// Each monitor's current statement, including stale statements for the freshness check.
func GetDnsCanaryVerdicts(ctx context.Context, root string) ([]dnsservice.DnsCanaryVerdict, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	var verdicts []dnsservice.DnsCanaryVerdict
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT source, serial, ok, report_time FROM dns_canary_verdict WHERE root = $1 ORDER BY source`, dnsLabelRoot(root))
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var verdict dnsservice.DnsCanaryVerdict
				var serial int64
				server.Raise(result.Scan(&verdict.Source, &serial, &verdict.Ok, &verdict.ReportTime))
				verdict.Serial = dnsSerialFromColumn(serial)
				verdicts = append(verdicts, verdict)
			}
		})
	})
	return verdicts, nil
}

// New configured nameservers start running. A configuration reread preserves a restore's stopped state.
func SyncDnsNameServers(ctx context.Context, root string, names []string) error {
	if !server.SidecarMg5Linked() {
		return ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO dns_ns (root, ns, state)
			SELECT $1, ns, 'running' FROM unnest($2::text[]) AS ns ON CONFLICT DO NOTHING`, root, names))
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM dns_ns WHERE root = $1 AND NOT (ns = ANY($2))`, root, names))
	})
	return nil
}

// Read the names skipped by the SOA check until a reload makes them eligible again.
func GetStoppedDnsNameServers(ctx context.Context, root string) ([]string, error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	var names []string
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT ns FROM dns_ns WHERE root = $1 AND state = 'stopped' ORDER BY ns`, dnsLabelRoot(root))
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				server.Raise(result.Scan(&name))
				names = append(names, name)
			}
		})
	})
	return names, nil
}

// The restore's SOA checks drive both transitions. A repeated stop keeps its original time.
func SetDnsNameServerStopped(ctx context.Context, root, name, reason string, now time.Time) error {
	if !server.SidecarMg5Linked() {
		return ErrDnsUnlinked
	}
	root = dnsLabelRoot(root)
	server.Tx(ctx, func(tx server.PgTx) {
		if reason == "" {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE dns_ns SET state = 'running', stopped_time = NULL, reason = NULL WHERE root = $1 AND ns = $2`, root, name))
			return
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO dns_ns (root, ns, state, stopped_time, reason)
			VALUES ($1, $2, 'stopped', $3, $4)
			ON CONFLICT (root, ns) DO UPDATE SET state = 'stopped', stopped_time = EXCLUDED.stopped_time, reason = EXCLUDED.reason
			WHERE dns_ns.state <> 'stopped'`, root, name, now.UTC(), reason))
	})
	return nil
}
