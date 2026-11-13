// The primary's own hostname, captured by psql on the primary before a
// guarded statement runs, so a frame can prove which host answered it.
package monitor

// A psql meta-command, not a statement: psql runs the backticked shell
// command on the host where it runs and binds the result to
// :'monitor_primary_hostname'. The backticks keep it out of the raw-literal
// statement templates that use it.
const pgPrimaryHostnameMetaCommand = "\\set monitor_primary_hostname `hostname -s 2>/dev/null || printf ' unavailable'`"
