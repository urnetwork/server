# Privacy-safe web and search analytics

The public web service emits one structured `web_page_view` event for each
successful HTML `GET` on `ur.io` or `ur.xyz`. Cloudflare converts the client
address to `CF-IPCountry`; nginx validates only the country code and never logs
the address, forwarded-for chain, user agent, cookie, query string, fragment,
or raw referrer. The raw referrer is reduced in request memory to a fixed
`source` and `engine` value and then discarded.

The recurring taskworker imports aggregate search data from:

- Google Search Console (service-account JSON);
- Bing Webmaster JSON/HTTP (API key);
- Yandex Webmaster v4 (OAuth token and user id);
- Baidu Search Resource Platform (manual CSV inbox).

DuckDuckGo, OpenAI, Anthropic, Gemini, Copilot, Perplexity, xAI, Meta AI,
You.com, and Poe currently contribute referral attribution only. No supported
webmaster query API is assumed for those sources.

The task target in `taskworker/work` calls `controller.RunWebSearchAnalytics`.
Provider HTTP/manual-import handling lives in `controller/analytics_controller.go`;
shared configuration, row/state objects, persistence, and cleanup live in
`model/analytics_model.go`.

## Baidu delivery path

Upload UTF-8 CSV files to:

`<minio.prefix>/<env>/analytics/search/baidu/*.csv`

With the default main configuration this is
`blob/main/analytics/search/baidu/*.csv`. The object-store credentials already
mounted into taskworker are used; Baidu credentials are not required. Imports
are keyed by object name and SHA-256 content hash, so polling is idempotent and
a corrected file at the same name is processed when its content changes.

Required columns are `site,date,query,clicks,impressions`. Optional columns are
`path,region,device,search_type,position`. Dates may be `YYYY-MM-DD` or RFC3339.
Accepted site values are the configured names (`ur.io` and `ur.xyz`). Common
export aliases such as `keyword`, `shows`, `landing_page`, and `avg_position`
are accepted.

## Cardinality and retention

Query rows are filtered before storage. They must meet the configured
`minimum_impressions` floor (never lower than 10), likely email/phone/IP values
are redacted, text is capped at 160 runes, URL query strings are discarded, and
only the 5,000 highest-impression rows per provider/site/period are retained.
The database repeats the absolute 10-impression minimum as a backstop. The
task also trims already-stored groups back to the configured top-N after every
successful fetch, so rows that fall out of the retained set cannot accumulate.

`web_search_analytics` retains 400 days by default. Every analytics run deletes
up to 10,000 expired rows and up to 10,000 rows below the current impression
floor using indexed cleanup queries. Raising `minimum_impressions` therefore
reaps historic lower-volume queries immediately in bounded batches instead of
leaving them until the retention cutoff.

Provider authentication is isolated. Empty, malformed, or rejected credentials
record a privacy-safe status and skip that provider without failing the task or
blocking any other provider.
