//go:build acklineagetrace && h1memorytrace && darwin && cgo

package perfvar

/*
#include <libproc.h>
#include <sys/proc_info.h>
#include <unistd.h>

// Diagnostic-only native bridge. The SDK declares resident size in bytes.
// Read our own process only; no command, environment or process identity is
// returned. Zero fails closed rather than substituting cumulative maxRSS.
static unsigned long long h1_diag_self_rss(void) {
    struct proc_taskinfo info;
    int n = proc_pidinfo(getpid(), PROC_PIDTASKINFO, 0, &info, sizeof(info));
    return n == sizeof(info) ? info.pti_resident_size : 0;
}
*/
import "C"

// Ordinary builds exclude this file and do not acquire a cgo/native sampler.
func h1DiagnosticSelfRSS() uint64 { return uint64(C.h1_diag_self_rss()) }
