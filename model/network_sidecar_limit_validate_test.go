package model

// ValidateNetworkSidecarLimit (DESIGN.md 20.5, FS-47's row clause, owner decisions 66 and
// 146), which §21's POST /abuse/action calls. Pure: it needs no database.

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateNetworkSidecarLimit(t *testing.T) {
	defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{
		"evs_closure_bytes_floor": 4 * 1024 * 1024,
	})()

	for _, c := range []struct {
		limitKey   string
		limitValue int64
		reason     string
		ok         bool
	}{
		{limitKey: "dns_entries", limitValue: 1, reason: "abuse case 0001", ok: true},
		{limitKey: "dns_entries", limitValue: 0, reason: "abuse case 0001", ok: true},
		{limitKey: "sidecar_clients", limitValue: -1, reason: "ops: change 1", ok: true},
		{limitKey: "acl_groups", limitValue: 1, reason: "ops: change 1", ok: true},
		{limitKey: "smtp_burst_window", limitValue: 60_000, reason: "ops: change 1", ok: true},
		{limitKey: "directory_requests_per_day", limitValue: 10, reason: "abuse case 0002", ok: true},
		{limitKey: "evs_closure_bytes_per_day", limitValue: 4 * 1024 * 1024, reason: "abuse case 0003", ok: true},
		{limitKey: "evs_closure_bytes_per_day", limitValue: -1, reason: "ops: change 1", ok: true},

		// a row never takes the allowance below the floor, nor sets the floor or the pacing
		{limitKey: "evs_closure_bytes_per_day", limitValue: 4*1024*1024 - 1, reason: "abuse case 0003"},
		{limitKey: "evs_closure_bytes_floor", limitValue: 8 * 1024 * 1024, reason: "ops: change 1"},
		{limitKey: "evs_closure_uploads_per_s", limitValue: 8, reason: "ops: change 1"},
		// no key for forwarding or sockets, nor any other this build does not know
		{limitKey: "forwarding", limitValue: 1, reason: "ops: change 1"},
		{limitKey: "sockets", limitValue: 1, reason: "ops: change 1"},
		{limitKey: "", limitValue: 1, reason: "ops: change 1"},
		// values a kind cannot take
		{limitKey: "acl_groups", limitValue: 2, reason: "ops: change 1"},
		{limitKey: "acl_groups", limitValue: -1, reason: "ops: change 1"},
		{limitKey: "smtp_burst_window", limitValue: 0, reason: "ops: change 1"},
		{limitKey: "dns_entries", limitValue: -2, reason: "ops: change 1"},
		// a reason that cites nothing, or cannot be stored
		{limitKey: "dns_entries", limitValue: 1, reason: ""},
		{limitKey: "dns_entries", limitValue: 1, reason: "   "},
		{limitKey: "dns_entries", limitValue: 1, reason: strings.Repeat("x", 257)},
		{limitKey: "dns_entries", limitValue: 1, reason: "abuse\ncase"},
		{limitKey: "dns_entries", limitValue: 1, reason: "abuse \xff"},
	} {
		err := ValidateNetworkSidecarLimit(c.limitKey, c.limitValue, c.reason)
		if c.ok && err != nil {
			t.Errorf("%q=%d %q: %v", c.limitKey, c.limitValue, c.reason, err)
		}
		if !c.ok && !errors.Is(err, ErrNetworkSidecarLimitRefused) {
			t.Errorf("%q=%d %q: %v, want refused", c.limitKey, c.limitValue, c.reason, err)
		}
	}
}

// What a row written directly may still do: the closure allowance under the floor
// applies, to be clamped, and nothing else the validation refuses does.
func TestUsableNetworkSidecarLimitRow(t *testing.T) {
	defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{
		"evs_closure_bytes_floor": 4 * 1024 * 1024,
	})()

	for _, c := range []struct {
		limitKey   string
		limitValue int64
		usable     bool
	}{
		{limitKey: "dns_entries", limitValue: 3, usable: true},
		{limitKey: "evs_closure_bytes_per_day", limitValue: 1024, usable: true},
		{limitKey: "evs_closure_bytes_per_day", limitValue: 0, usable: true},
		{limitKey: "evs_closure_bytes_per_day", limitValue: -2, usable: false},
		{limitKey: "evs_closure_uploads_per_s", limitValue: 1, usable: false},
		{limitKey: "evs_closure_bytes_floor", limitValue: 0, usable: false},
		{limitKey: "no_such_limit", limitValue: 1, usable: false},
		{limitKey: "acl_groups", limitValue: 7, usable: false},
	} {
		if usable := usableNetworkSidecarLimitRow(c.limitKey, c.limitValue); usable != c.usable {
			t.Errorf("%s=%d: usable %t, want %t", c.limitKey, c.limitValue, usable, c.usable)
		}
	}
}
