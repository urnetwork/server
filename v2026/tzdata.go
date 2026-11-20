package server

// The server image (Dockerfile) is alpine with no zoneinfo, so a binary
// resolves an IANA zone only from the database embedded here. Without it every
// time.LoadLocation fails and its callers fall back to UTC: onboarding and
// re-engagement mail was placed by UTC hours instead of the network's,
// auth-client time zones were never stored, and the monitor could not read
// Play's date-times. Every service binary imports this package, so each one
// carries the database (about 450 KB); onboarding's
// TestServiceBinariesEmbedTimeZoneDatabase keeps it that way.
import _ "time/tzdata"
