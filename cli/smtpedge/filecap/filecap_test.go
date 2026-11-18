// The attribute's format against what libcap's setcap writes and getcap prints: the helper's bytes for
// cap_net_admin=ep are setcap cap_net_admin+ep's, the bytes BuildKit's exported layer carried for it on
// 2026-11-18; each revision decodes to getcap's text, a malformed or unknown one is refused, and the check takes
// that one value and no other.

package filecap

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// setcap cap_net_admin+ep's attribute outside a user namespace, as getfattr -e base64 prints it: revision 2
// with the effective flag, then permitted 0x00001000 and nothing inheritable.
const setcapNetAdminBase64 = "AQAAAgAQAAAAAAAAAAAAAAAAAAA="

func TestNetAdminFileCapabilityIsSetcapsAttribute(t *testing.T) {
	value, err := NetAdminFileCapability().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.StdEncoding.EncodeToString(value); got != setcapNetAdminBase64 {
		t.Fatalf("the helper writes %s, setcap cap_net_admin+ep writes %s", got, setcapNetAdminBase64)
	}
	decoded, err := DecodeFileCapability(value)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != NetAdminFileCapability() {
		t.Fatalf("decoded %+v", decoded)
	}
	if got := decoded.String(); got != "cap_net_admin=ep" {
		t.Fatalf("getcap prints %q", got)
	}
	if err := CheckNetAdmin(value); err != nil {
		t.Fatal(err)
	}
}

// Each revision reads as libcap's _fcaps_load loads it and cap_to_text writes it.
func TestDecodeFileCapabilityReadsEachRevisionAsGetcap(t *testing.T) {
	all := uint64(1)<<len(capabilityNames) - 1
	cases := []struct {
		capability FileCapability
		text       string
	}{
		{capability: FileCapability{Revision: 1, Effective: true, Permitted: 1 << 13}, text: "cap_net_raw=ep"},
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: 1 << 13}, text: "cap_net_raw=ep"},
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: 1 << 10}, text: "cap_net_bind_service=ep"},
		{capability: FileCapability{Revision: 2, Permitted: 1 << CapNetAdmin}, text: "cap_net_admin=p"},
		{capability: FileCapability{Revision: 2, Inheritable: 1 << CapNetAdmin}, text: "cap_net_admin=i"},
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: 1 << CapNetAdmin, Inheritable: 1 << CapNetAdmin}, text: "cap_net_admin=eip"},
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: 1<<CapNetAdmin | 1<<13}, text: "cap_net_admin,cap_net_raw=ep"},
		// the effective flag raises every permitted or inheritable capability: here eip and ep
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: 1<<CapNetAdmin | 1<<13, Inheritable: 1 << CapNetAdmin}, text: "cap_net_admin=eip cap_net_raw+ep"},
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: 1 << 40}, text: "cap_checkpoint_restore=ep"},
		{capability: FileCapability{Revision: 3, Effective: true, Permitted: 1 << CapNetAdmin, RootId: 100000}, text: "cap_net_admin=ep"},
		// the prevailing combination is the base, the others add or drop against it
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: all}, text: "=ep"},
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: all &^ (1 << CapNetAdmin)}, text: "=ep cap_net_admin-ep"},
		// a capability past the named ones prints as its number, added
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: 1 << 41}, text: "= 41+ep"},
	}
	for _, c := range cases {
		value, err := c.capability.Encode()
		if err != nil {
			t.Fatalf("%+v: %v", c.capability, err)
		}
		decoded, err := DecodeFileCapability(value)
		if err != nil {
			t.Fatalf("%+v: %v", c.capability, err)
		}
		if decoded != c.capability {
			t.Errorf("%+v decoded as %+v", c.capability, decoded)
		}
		if got := decoded.String(); got != c.text {
			t.Errorf("%+v: getcap prints %q, not %q", c.capability, got, c.text)
		}
	}
	// revision 3's rootid shows as getcap shows it
	described := FileCapability{Revision: 3, Effective: true, Permitted: 1 << CapNetAdmin, RootId: 100000}.describe()
	if described != "revision 3 cap_net_admin=ep [rootid=100000]" {
		t.Errorf("described %q", described)
	}
}

// The layouts byte for byte: revision 1's 12 bytes, revision 3's 24 with the rootid last.
func TestEncodeFileCapabilityLaysOutEachRevision(t *testing.T) {
	cases := []struct {
		capability FileCapability
		hex        string
	}{
		{capability: FileCapability{Revision: 1, Effective: true, Permitted: 1 << 13}, hex: "010000010020000000000000"},
		{capability: FileCapability{Revision: 2, Inheritable: 1<<33 | 1}, hex: "0000000200000000010000000000000002000000"},
		{capability: FileCapability{Revision: 3, Effective: true, Permitted: 1 << CapNetAdmin, RootId: 0x01020304}, hex: "010000030010000000000000000000000000000004030201"},
	}
	for _, c := range cases {
		value, err := c.capability.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(value); got != c.hex {
			t.Errorf("%+v encodes as %s, not %s", c.capability, got, c.hex)
		}
	}
}

func TestDecodeFileCapabilityRefusesMalformedAttributes(t *testing.T) {
	v2, err := NetAdminFileCapability().Encode()
	if err != nil {
		t.Fatal(err)
	}
	v1 := []byte{0x01, 0x00, 0x00, 0x01, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	withRevision := func(value []byte, revision byte) []byte {
		changed := append([]byte(nil), value...)
		changed[3] = revision
		return changed
	}
	cases := map[string][]byte{
		"empty":                   nil,
		"short magic":             v2[:3],
		"revision 0":              withRevision(v2, 0),
		"revision 4":              withRevision(v2, 4),
		"revision 2 short":        v2[:12],
		"revision 2 long":         append(append([]byte(nil), v2...), 0, 0, 0, 0),
		"revision 1 at 20 bytes":  withRevision(v2, 1),
		"revision 1 short":        v1[:8],
		"revision 3 at 20 bytes":  withRevision(v2, 3),
		"revision 2 at 24 bytes":  withRevision(append(append([]byte(nil), v2...), 1, 0, 0, 0), 2),
		"revision 1 at 24 bytes":  withRevision(append(append([]byte(nil), v2...), 1, 0, 0, 0), 1),
		"revision 3 at 28 bytes":  withRevision(append(append([]byte(nil), v2...), 1, 0, 0, 0, 0, 0, 0, 0), 3),
		"revision 2 one word off": v2[:19],
	}
	for name, value := range cases {
		if decoded, err := DecodeFileCapability(value); err == nil {
			t.Errorf("%s decoded as %+v", name, decoded)
		}
		if err := CheckNetAdmin(value); err == nil || !strings.Contains(err.Error(), "does not decode") {
			t.Errorf("%s: the check answered %v", name, err)
		}
	}
	if _, err := DecodeFileCapability(v1); err != nil {
		t.Errorf("revision 1's 12 bytes: %v", err)
	}
}

func TestEncodeFileCapabilityRefusesWhatItsRevisionCannotHold(t *testing.T) {
	for _, capability := range []FileCapability{
		{Revision: 0, Permitted: 1},
		{Revision: 4, Permitted: 1},
		{Revision: 1, Permitted: 1 << 32},
		{Revision: 1, Inheritable: 1 << 40},
		{Revision: 2, Effective: true, Permitted: 1 << CapNetAdmin, RootId: 1},
		{Revision: 1, Permitted: 1, RootId: 1},
	} {
		if value, err := capability.Encode(); err == nil {
			t.Errorf("%+v encoded as %x", capability, value)
		}
	}
}

// Only the helper's own bytes pass: no other capability or set, no other revision, and no namespaced attribute,
// which grants nothing on a host outside its user namespace.
func TestCheckNetAdminRefusesEveryOtherAttribute(t *testing.T) {
	cases := []struct {
		capability FileCapability
		found      string
	}{
		{capability: FileCapability{Revision: 3, Effective: true, Permitted: 1 << CapNetAdmin, RootId: 100000}, found: "revision 3 cap_net_admin=ep [rootid=100000]"},
		{capability: FileCapability{Revision: 3, Effective: true, Permitted: 1 << CapNetAdmin}, found: "revision 3 cap_net_admin=ep"},
		{capability: FileCapability{Revision: 1, Effective: true, Permitted: 1 << CapNetAdmin}, found: "revision 1 cap_net_admin=ep"},
		{capability: FileCapability{Revision: 2, Permitted: 1 << CapNetAdmin}, found: "revision 2 cap_net_admin=p"},
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: 1 << CapNetAdmin, Inheritable: 1 << CapNetAdmin}, found: "revision 2 cap_net_admin=eip"},
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: 1<<CapNetAdmin | 1<<21}, found: "revision 2 cap_net_admin,cap_sys_admin=ep"},
		{capability: FileCapability{Revision: 2, Effective: true, Permitted: 1 << 13}, found: "revision 2 cap_net_raw=ep"},
		{capability: FileCapability{Revision: 2}, found: "revision 2 ="},
	}
	for _, c := range cases {
		value, err := c.capability.Encode()
		if err != nil {
			t.Fatal(err)
		}
		err = CheckNetAdmin(value)
		if err == nil {
			t.Errorf("%+v passed the check", c.capability)
			continue
		}
		if want := "holds " + c.found + ", not revision 2 cap_net_admin=ep"; !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want it to say %q", c.capability, err, want)
		}
	}
	// a flag bit no revision defines changes the bytes, so the check refuses it though getcap reads past it
	value, err := NetAdminFileCapability().Encode()
	if err != nil {
		t.Fatal(err)
	}
	value[0] |= 0x02
	if decoded, err := DecodeFileCapability(value); err != nil || decoded != NetAdminFileCapability() {
		t.Errorf("an undefined flag bit decoded as %+v, %v", decoded, err)
	}
	if err := CheckNetAdmin(value); err == nil {
		t.Error("an undefined flag bit passed the check")
	}
}
