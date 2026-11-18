Fixtures for the mail breakers' measures (ABUSE-M9-03). Each is in its source's own shape with synthetic content:
names are .example, addresses are RFC 5737's and RFC 3849's, and no value came from a real account.

- postmaster_traffic_stats.json: Postmaster Tools v1 domains.trafficStats.get for root.example on 2026-11-01.
- postmaster_traffic_stats_no_ratio.json: the same answer with no userReportedSpamRatio.
- snds_data.csv: SNDS automated data access, one row for each address with enough mail in the period.
- snds_data_bad_row.csv: the same with a row whose filter result is no color.
- mirror/sbl.rbldnsd, mirror/css.rbldnsd, mirror/other.rbldnsd: a listing mirror's zone files as MAIL-M7-20's
  mirror names them, each category's file one prefix a line.
