#!/usr/bin/env sh
# Sweep the warp grafana for dashboard panels that read the 12 stat keys
# removed from /stats/last-90 (server/STATS3.md — packets, extenders,
# superspeed). Grafana is host-local (config/main/grafana.yml local_port
# 3100, no public domain), so run this ON a host that can reach it, e.g.:
#
#   GRAFANA_URL=http://localhost:3100 \
#   GRAFANA_PASSWORD=$(...from vault/main/grafana.yml grafana.admin_password) \
#   ./grafana-stats-key-sweep.sh            # report only
#   ./grafana-stats-key-sweep.sh --json     # dump matching dashboard JSON
#
# Report-only by design: panel removal is a visual/layout edit best made in
# the grafana UI with the dashboard in front of you. This finds every
# dashboard + panel title that references a removed key so nothing is missed.
set -eu

: "${GRAFANA_URL:?set GRAFANA_URL (e.g. http://localhost:3100)}"
: "${GRAFANA_USER:=admin}"
: "${GRAFANA_PASSWORD:?set GRAFANA_PASSWORD (vault/main/grafana.yml grafana.admin_password)}"

KEYS="all_packets_data all_packets_summary all_packets_summary_rate \
providers_superspeed_data providers_summary_superspeed \
extender_transfer_data extender_transfer_summary extender_transfer_summary_rate \
extenders_data extenders_superspeed_data extenders_summary extenders_summary_superspeed"

auth="${GRAFANA_USER}:${GRAFANA_PASSWORD}"
found=0

uids=$(curl -fsS -u "$auth" "${GRAFANA_URL}/api/search?type=dash-db&limit=5000" \
    | python3 -c 'import json,sys; [print(d["uid"]) for d in json.load(sys.stdin)]')

for uid in $uids; do
    dash=$(curl -fsS -u "$auth" "${GRAFANA_URL}/api/dashboards/uid/${uid}")
    for key in $KEYS; do
        if printf '%s' "$dash" | grep -q "$key"; then
            found=1
            title=$(printf '%s' "$dash" | python3 -c 'import json,sys; print(json.load(sys.stdin)["dashboard"]["title"])')
            echo "MATCH dashboard '${title}' (uid ${uid}) references: ${key}"
            if [ "${1:-}" = "--json" ]; then
                printf '%s\n' "$dash" | python3 -m json.tool
            fi
        fi
    done
done

if [ "$found" = "0" ]; then
    echo "clean: no dashboard references any removed stat key"
fi
