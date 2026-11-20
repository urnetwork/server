// The proxy processes' Go runtime memory limit on each host (server CPUOPT6.md
// Part B #3; owner, 2026-11-15).
//
// The service sets one GOMEMLIMIT for every proxy process (vault services.yml
// env_vars), but the proxy hosts differ: at sweep load on 2026-11-15 crisp kept
// 41.9 GB available at its daily minimum and fireside 9.0 GB, while every
// process sat at the 6500 MiB limit and collected about 12 times a minute. The
// vault's proxy_sizing.yml names a host's own limit, a sizing value per host
// like the service's cores, keyed by the host's fully qualified name as
// proxy.yml is:
//
//	go_memory_limit:
//	  crisp.bringyour.com: 8000MiB
//
// Each process reads it once, at start, and sets its runtime limit
// (runtime/debug.SetMemoryLimit) when its host has an entry, so a change
// applies at each block's next start. A host without an entry, and a missing
// resource, keep the service's GOMEMLIMIT. The container's cgroup memory cap
// (services.yml memory_limit) stays the runaway guard: an entry above
// MaxCgroupMemoryFraction of the cap, an unreadable or unlimited cap, or a
// value that does not parse keeps the service's value, with one error line, so
// a typo neither fails startup nor leaves a process with a limit its container
// cannot hold.
package proxy

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The vault resource and its key.
const (
	hostGoMemoryLimitResourceName = "proxy_sizing.yml"
	hostGoMemoryLimitKey          = "go_memory_limit"
)

// The container's cgroup memory cap: cgroup v2, then v1.
var hostGoMemoryLimitCgroupPaths = []string{
	"/sys/fs/cgroup/memory.max",
	"/sys/fs/cgroup/memory/memory.limit_in_bytes",
}

// The bounds a host's entry must keep.
type HostGoMemoryLimitSettings struct {
	// The largest share of the container's cgroup memory cap a host's limit
	// may take. 0.9 is warp's own soft limit, the GOMEMLIMIT it gives a
	// container whose service sets none, which leaves a tenth of the cap for
	// memory the runtime limit does not see. Nonpositive refuses every entry.
	MaxCgroupMemoryFraction float64
	// The smallest share of the cap a host's limit may take. An entry missing
	// its unit reads as bytes, and a limit of a few kilobytes would keep the
	// collector running without pause; a quarter of the 10 GiB cap is below
	// what a quiet process holds (about 2.3 GB of heap and stacks on
	// 2026-11-13) and far above any such typo. Nonpositive has no floor.
	MinCgroupMemoryFraction float64
}

// A new instance on every call.
func DefaultHostGoMemoryLimitSettings() *HostGoMemoryLimitSettings {
	return &HostGoMemoryLimitSettings{
		MaxCgroupMemoryFraction: 0.9,
		MinCgroupMemoryFraction: 0.25,
	}
}

// Sets the process's Go memory limit from the vault's entry for this host and
// returns the limit set, or 0 when the process keeps the service's
// GOMEMLIMIT.
func ApplyHostGoMemoryLimit() int64 {
	host, err := server.Host()
	if err != nil {
		glog.Errorf("[proxy]%s: the host is unknown; keeping the service's memory limit\n", hostGoMemoryLimitResourceName)
		return 0
	}
	domain, _ := server.Domain()
	return applyHostGoMemoryLimit(
		host,
		domain,
		readHostGoMemoryLimitCgroupCap,
		debug.SetMemoryLimit,
		DefaultHostGoMemoryLimitSettings(),
	)
}

// ApplyHostGoMemoryLimit with the cap reader and the runtime setter passed in.
func applyHostGoMemoryLimit(
	host string,
	domain string,
	readCgroupCap func() (int64, error),
	setMemoryLimit func(int64) int64,
	settings *HostGoMemoryLimitSettings,
) int64 {
	resource, err := server.Vault.SimpleResource(hostGoMemoryLimitResourceName)
	if err != nil {
		// an absent resource names no host; an unreadable one is reported
		if !errors.Is(err, server.ErrResourceNotFound) || errors.Is(err, server.ErrResourceUnavailable) {
			glog.Errorf("[proxy]%s is unreadable; keeping the service's memory limit\n", hostGoMemoryLimitResourceName)
		}
		return 0
	}
	values, err := resource.ParseE()
	if err != nil {
		glog.Errorf("[proxy]%s does not parse; keeping the service's memory limit\n", hostGoMemoryLimitResourceName)
		return 0
	}
	value, ok := hostGoMemoryLimitEntry(values, host, domain)
	if !ok {
		return 0
	}
	cgroupCap, capErr := readCgroupCap()
	limit, err := hostGoMemoryLimit(value, cgroupCap, capErr, settings)
	if err != nil {
		glog.Errorf("[proxy]%s: %s for %s: %s; keeping the service's memory limit\n", hostGoMemoryLimitResourceName, hostGoMemoryLimitKey, host, err)
		return 0
	}
	previousLimit := setMemoryLimit(limit)
	glog.Infof("[proxy]%s: %s for %s = %d bytes (was %d)\n", hostGoMemoryLimitResourceName, hostGoMemoryLimitKey, host, limit, previousLimit)
	return limit
}

// The host's entry under go_memory_limit: by its name as given, or, for a name
// without a domain, by its fully qualified name.
func hostGoMemoryLimitEntry(values map[string]any, host string, domain string) (any, bool) {
	hostValues, ok := values[hostGoMemoryLimitKey].(map[string]any)
	if !ok {
		if _, present := values[hostGoMemoryLimitKey]; present {
			glog.Errorf("[proxy]%s: %s must map hosts to sizes; keeping the service's memory limit\n", hostGoMemoryLimitResourceName, hostGoMemoryLimitKey)
		}
		return nil, false
	}
	if value, ok := hostValues[host]; ok {
		return value, true
	}
	if domain != "" && !strings.Contains(host, ".") {
		if value, ok := hostValues[host+"."+domain]; ok {
			return value, true
		}
	}
	return nil, false
}

// The limit an entry names, in bytes: a size such as 8000MiB, from
// MinCgroupMemoryFraction to MaxCgroupMemoryFraction of the container's cgroup
// memory cap.
func hostGoMemoryLimit(value any, cgroupCap int64, cgroupCapErr error, settings *HostGoMemoryLimitSettings) (int64, error) {
	text, ok := value.(string)
	if !ok {
		return 0, fmt.Errorf("%v (%T) is not a size", value, value)
	}
	byteCount, err := model.ParseByteCount(strings.TrimSpace(text))
	if err != nil {
		return 0, fmt.Errorf("%q is not a size: %w", text, err)
	}
	if byteCount <= 0 {
		return 0, fmt.Errorf("%q is not positive", text)
	}
	if cgroupCapErr != nil {
		return 0, fmt.Errorf("the container's memory cap is unreadable: %w", cgroupCapErr)
	}
	if cgroupCap <= 0 {
		return 0, errors.New("the container has no memory cap")
	}
	maxLimit := int64(float64(cgroupCap) * settings.MaxCgroupMemoryFraction)
	if maxLimit < int64(byteCount) {
		return 0, fmt.Errorf("%q is above %.2f of the container's memory cap of %d bytes", text, settings.MaxCgroupMemoryFraction, cgroupCap)
	}
	minLimit := int64(float64(cgroupCap) * settings.MinCgroupMemoryFraction)
	if int64(byteCount) < minLimit {
		return 0, fmt.Errorf("%q is below %.2f of the container's memory cap of %d bytes", text, settings.MinCgroupMemoryFraction, cgroupCap)
	}
	return int64(byteCount), nil
}

// The container's cgroup memory cap in bytes, 0 when it is unlimited.
func readHostGoMemoryLimitCgroupCap() (int64, error) {
	var errs []error
	for _, path := range hostGoMemoryLimitCgroupPaths {
		b, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		return parseHostGoMemoryLimitCgroupCap(string(b))
	}
	return 0, errors.Join(errs...)
}

// A cgroup memory cap file's value: "max" (v2) or a v1 limit near the largest
// int64 is unlimited (0).
func parseHostGoMemoryLimitCgroupCap(text string) (int64, error) {
	text = strings.TrimSpace(text)
	if text == "max" {
		return 0, nil
	}
	cgroupCap, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, err
	}
	// cgroup v1 reports no limit as the largest page-aligned int64
	if math.MaxInt64/2 < cgroupCap {
		return 0, nil
	}
	return cgroupCap, nil
}
