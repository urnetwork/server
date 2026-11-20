// A fresh process makes resolver-state ownership independent of test order.
package providertunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Run the actual plaintext deadline fixture before any other lookup, then
// touch Go's DNS configuration outside it. A custom dial alone is not isolation.
func TestProbeRegionalDnsResolverDoesNotEscapeBubble(t *testing.T) {
	const childKey = "SERVER_TEST_REGIONAL_DNS_RESOLVER_CHILD"
	const fixtureCompleted = "regional plaintext deadline fixture completed"
	const completed = "regional plaintext deadline and outside resolver joined"
	if os.Getenv(childKey) == "1" {
		TestProbeRegionalDnsFailureKeepsHardDeadline(t)
		if t.Failed() {
			return
		}
		fmt.Fprintln(os.Stdout, fixtureCompleted)
		// The canceled lookup acquires Go's Unix DNS-config semaphore before
		// checking cancellation. Its per-instance dial forbids every socket.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		resolver := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return nil, errors.New("regional resolver regression forbids network access")
			},
		}
		if _, err := resolver.LookupTXT(ctx, "after-regional-deadline.example."); err == nil {
			t.Fatal("the canceled synthetic lookup unexpectedly succeeded")
		}
		fmt.Fprintln(os.Stdout, completed)
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal("could not locate this test executable")
	}
	// These retain the existing resolver regression's deadlock bounds, not
	// a timing oracle; the fixture still requires its real ten-second deadline.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable,
		"-test.run=^TestProbeRegionalDnsResolverDoesNotEscapeBubble$",
		"-test.timeout=25s",
	)
	command.Env = append(os.Environ(), childKey+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		if bytes.Contains(output, []byte(fixtureCompleted+"\n")) &&
			bytes.Contains(output, []byte("fatal error: send on synctest channel from outside bubble")) {
			t.Fatal("regional deadline fixture left Go's DNS semaphore owned by its ended synctest bubble")
		}
		// Initialization output may contain environment-dependent data.
		t.Fatalf("fresh regional resolver process did not complete: %v (child output withheld)", err)
	}
	if !bytes.Contains(output, []byte(fixtureCompleted+"\n")) || !bytes.Contains(output, []byte(completed+"\n")) {
		t.Fatal("fresh process did not confirm both sides of the regional resolver boundary")
	}
}
