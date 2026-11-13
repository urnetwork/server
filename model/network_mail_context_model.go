// SPDX-License-Identifier: MPL-2.0

package model

// The durable per-network mail context (network_mail_context, migration in
// network_mail_context_schema.go): what a network's own apps and browsers
// last reported about the reader, kept whatever the onboarding campaign's
// state. Until it, a device's time zone and language were kept only in a
// still-running onboarding row, so a network that finished onboarding, was a
// holdout, signed up through the device path, or signed up before
// 2026-09-15 was mailed at 10:00 UTC and in English (the campaign review of
// 2026-11-13). Written on every auth-client, sign-up and sign-in
// (controller/mail_context.go), read when a campaign email is placed and
// rendered. Only the latest values are kept: the newest zone, the newest
// language list, and the newest app build per platform and distribution.
// Nothing here comes from an IP lookup.

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/onboarding"
)

// NetworkMailContext is one network's row.
type NetworkMailContext struct {
	NetworkId server.Id `json:"network_id"`
	// the newest IANA zone a device reported, one the time package loads;
	// "" when none has
	TimeZone     string     `json:"time_zone"`
	TimeZoneTime *time.Time `json:"time_zone_time,omitempty"`
	// the newest language list a client reported, most preferred first,
	// normalized BCP 47 (onboarding.NormalizeLanguages)
	Languages     []string   `json:"languages"`
	LanguagesTime *time.Time `json:"languages_time,omitempty"`
	// the newest app build per platform and distribution, newest first
	Clients    []NetworkMailClient `json:"clients"`
	UpdateTime time.Time           `json:"update_time"`
}

// NetworkMailClient is the newest build one platform and distribution of the
// network's apps reported (X-UR-ClientInfo).
type NetworkMailClient struct {
	// an event platform (EventPlatforms): ios, macos, android, windows,
	// linux, web
	Platform string `json:"platform"`
	// session.ClientDistribution*, "" when the build reported none
	Distribution string    `json:"distribution,omitempty"`
	AppVersion   string    `json:"app_version,omitempty"`
	Time         time.Time `json:"time"`
}

// NetworkMailContextReport is what one request reported. Every part is
// optional; an empty part leaves the stored one as it is.
type NetworkMailContextReport struct {
	// validated by the caller (controller.usableReportedTimeZone)
	TimeZone string
	// normalized by the caller, most preferred first
	Languages []string
	// nil when the request named no platform
	Client *NetworkMailClient
}

// MergeNetworkMailContext is the row after a report: a changed part replaces
// the stored one with now as its time, and an unchanged part only renews its
// time once the stored time is older than the settings' RefreshInterval, so a
// device that authenticates many times a day writes the row at most daily.
// The clients keep one entry per platform and distribution, newest first, at
// most MaxClients. changed is false when nothing needs writing; current may
// be nil.
func MergeNetworkMailContext(
	current *NetworkMailContext,
	networkId server.Id,
	report *NetworkMailContextReport,
	now time.Time,
	settings *onboarding.MailContextSettings,
) (next *NetworkMailContext, changed bool) {
	next = &NetworkMailContext{NetworkId: networkId, Languages: []string{}, Clients: []NetworkMailClient{}}
	if current != nil {
		next.TimeZone = current.TimeZone
		next.TimeZoneTime = current.TimeZoneTime
		next.Languages = slices.Clone(current.Languages)
		next.LanguagesTime = current.LanguagesTime
		next.Clients = slices.Clone(current.Clients)
		next.UpdateTime = current.UpdateTime
	}
	if report == nil {
		return next, false
	}
	stale := func(at *time.Time) bool {
		return at == nil || !now.Before(at.Add(settings.RefreshInterval))
	}
	if report.TimeZone != "" && (report.TimeZone != next.TimeZone || stale(next.TimeZoneTime)) {
		next.TimeZone = report.TimeZone
		next.TimeZoneTime = &now
		changed = true
	}
	if languages := report.Languages; 0 < len(languages) && 0 < settings.MaxLanguages {
		if settings.MaxLanguages < len(languages) {
			languages = languages[:settings.MaxLanguages]
		}
		if !slices.Equal(languages, next.Languages) || stale(next.LanguagesTime) {
			next.Languages = slices.Clone(languages)
			next.LanguagesTime = &now
			changed = true
		}
	}
	if client := report.Client; client != nil && client.Platform != "" && 0 < settings.MaxClients {
		i := slices.IndexFunc(next.Clients, func(c NetworkMailClient) bool {
			return c.Platform == client.Platform && c.Distribution == client.Distribution
		})
		if i < 0 || next.Clients[i].AppVersion != client.AppVersion || stale(&next.Clients[i].Time) {
			if 0 <= i {
				next.Clients = slices.Delete(next.Clients, i, i+1)
			}
			reported := *client
			reported.Time = now
			next.Clients = slices.Insert(next.Clients, 0, reported)
			if settings.MaxClients < len(next.Clients) {
				next.Clients = next.Clients[:settings.MaxClients]
			}
			changed = true
		}
	}
	if changed {
		next.UpdateTime = now
	}
	return next, changed
}

const networkMailContextSelect = `
	SELECT network_id, time_zone, time_zone_time, languages, languages_time, clients, update_time
	FROM network_mail_context
	WHERE network_id = $1
`

func scanNetworkMailContext(result pgx.Rows) *NetworkMailContext {
	c := &NetworkMailContext{}
	var clientsJson []byte
	server.Raise(result.Scan(
		&c.NetworkId,
		&c.TimeZone,
		&c.TimeZoneTime,
		&c.Languages,
		&c.LanguagesTime,
		&clientsJson,
		&c.UpdateTime,
	))
	if c.Languages == nil {
		c.Languages = []string{}
	}
	// a malformed document reads as no clients
	if err := json.Unmarshal(clientsJson, &c.Clients); err != nil || c.Clients == nil {
		c.Clients = []NetworkMailClient{}
	}
	return c
}

// GetNetworkMailContext is the network's row, or nil when no client has
// reported anything yet.
func GetNetworkMailContext(ctx context.Context, networkId server.Id) (mailContext *NetworkMailContext) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, networkMailContextSelect, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				mailContext = scanNetworkMailContext(result)
			}
		})
	})
	return
}

// RecordNetworkMailContext merges a report into the network's row
// (MergeNetworkMailContext) and writes it when something changed. The read
// locks the row, so two reports for one network merge in turn; the first
// insert of two at once is last-writer-wins, both being the newest values.
func RecordNetworkMailContext(
	ctx context.Context,
	networkId server.Id,
	report *NetworkMailContextReport,
	now time.Time,
	settings *onboarding.MailContextSettings,
) (changed bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		changed = false
		var current *NetworkMailContext
		result, err := tx.Query(ctx, networkMailContextSelect+` FOR UPDATE`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				current = scanNetworkMailContext(result)
			}
		})
		next, nextChanged := MergeNetworkMailContext(current, networkId, report, now, settings)
		if !nextChanged {
			return
		}
		clientsJson, err := json.Marshal(next.Clients)
		server.Raise(err)
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_mail_context (
					network_id, time_zone, time_zone_time, languages, languages_time, clients, update_time
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (network_id) DO UPDATE SET
					time_zone = EXCLUDED.time_zone,
					time_zone_time = EXCLUDED.time_zone_time,
					languages = EXCLUDED.languages,
					languages_time = EXCLUDED.languages_time,
					clients = EXCLUDED.clients,
					update_time = EXCLUDED.update_time
			`,
			next.NetworkId,
			next.TimeZone,
			next.TimeZoneTime,
			next.Languages,
			next.LanguagesTime,
			string(clientsJson),
			next.UpdateTime,
		))
		changed = true
	})
	return
}
