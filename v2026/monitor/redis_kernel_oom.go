// Redis kernel attribution uses explicit native victim fields. Allocator names
// and cgroup context are not victim identity; only bounded categories leave
// the selector, and retained raw-row sources receive the same victim check.
package monitor

import (
	"fmt"
	"regexp"
)

// These POSIX patterns are shared by the native awk selector and Go reducer.
// Linux mm/oom_kill.c prints the selected victim in the kill sentence and in
// the terminal task/pid/uid suffix, distinct from "invoked oom-killer".
const (
	redisKernelMessagePrefix = `^[[:space:]]*(\[[^]]+\][[:space:]]*)?`
	redisKernelKilledPattern = redisKernelMessagePrefix + `((Out of memory|Memory cgroup out of memory):[[:space:]]+)?Killed process[[:space:]]+[0-9]+[[:space:]]+\((%s)\)([[:space:]]|$)`
	redisKernelTaskPattern   = redisKernelMessagePrefix + `oom-kill:.*,[[:space:]]*task=(%s),pid=[0-9]+,uid=[0-9]+[[:space:]]*$`
)

var (
	redisKernelKilledRedis = regexp.MustCompile(fmt.Sprintf(redisKernelKilledPattern, "redis-server"))
	redisKernelTaskRedis   = regexp.MustCompile(fmt.Sprintf(redisKernelTaskPattern, "redis-server"))
	redisKernelKilledAny   = regexp.MustCompile(fmt.Sprintf(redisKernelKilledPattern, "[^()]+"))
	redisKernelTaskAny     = regexp.MustCompile(fmt.Sprintf(redisKernelTaskPattern, "[^,]+"))
)

// Scan the retained source before reducing to at most one fixed row, so a
// later unrelated event cannot displace an earlier confirmed Redis victim.
// Unresolved invocation or malformed victim text stays unknown. Native read
// and pipeline authority belong to the calling observation command.
func redisProcessKernelOomSelector() string {
	return fmt.Sprintf(`awk '
  /invoked oom-killer/ {
    if (pending) unknown = 1
    pending = 1
    next
  }
  $0 ~ /%s/ || $0 ~ /%s/ {
    redis = 1
    pending = 0
    next
  }
  $0 ~ /%s/ || $0 ~ /%s/ {
    pending = 0
    next
  }
  tolower($0) ~ /out of memory|oom-kill|killed process/ { unknown = 1 }
  END {
    if (redis) print "kernel_oom redis-victim"
    else if (unknown || pending) print "kernel_oom victim-unknown"
  }
'`, redisKernelKilledRedis.String(), redisKernelTaskRedis.String(), redisKernelKilledAny.String(), redisKernelTaskAny.String())
}

// Fixed selector output and existing raw-row sources share the same ownership
// requirement. Unrecognized content cannot authorize a Redis PAGE or health.
func redisProcessKernelOomVictim(row string) string {
	if row == "redis-victim" || redisKernelKilledRedis.MatchString(row) || redisKernelTaskRedis.MatchString(row) {
		return "redis"
	}
	if redisKernelKilledAny.MatchString(row) || redisKernelTaskAny.MatchString(row) {
		return "other"
	}
	return "unknown"
}
