// Package filecap writes and reads the kernel's file capability attribute, security.capability, for smtpedge's
// image (QD-1066, owner decision 597; DESIGN §19.4's smtpedge capability).
//
// smtpedge runs as uid 65532 (D-OPS-1) and source-routes each MX dial with SO_MARK, which needs CAP_NET_ADMIN (or,
// from Linux 5.17, CAP_NET_RAW; the block is granted CAP_NET_ADMIN). warp's --cap-add=CAP_NET_ADMIN only puts
// the capability in the container's bounding set, and a process that is not root gains a capability at exec only
// from its binary's file capability. setcap is in no reviewed runtime package (server/cli/RUNTIME-PACKAGES.md),
// so the image's build stage runs this package's helper, cmd, from a bind mount to write cap_net_admin=ep on the
// binary, and the helper never ships. The release runs the same helper's check-image on the published image
// (CheckImage) and refuses one whose binary lacks it, since the failure otherwise shows only at the first marked
// dial, as EPERM at SO_MARK.
//
// The attribute is the kernel's struct vfs_cap_data, or vfs_ns_cap_data (include/uapi/linux/capability.h), in
// little-endian order: a 32-bit magic_etc whose top byte is the revision and whose low bit is
// VFS_CAP_FLAGS_EFFECTIVE, then each 32-bit word of the capability sets as its permitted half and its
// inheritable half. Revision 1 holds one word, revision 2 two, and revision 3 two and the 32-bit rootid of the
// user namespace that wrote it, the form a write from inside a user namespace takes. The helper writes revision
// 2, as setcap does outside a user namespace; the decoder reads all three and holds each to its exact length,
// as libcap's getcap and the kernel do.
//
// With the effective bit set, each capability the file permits is effective from exec in every thread, which a
// Go program needs: capabilities belong to a thread, so raising one later would reach only the thread that
// raised it. It also means the kernel refuses to exec the binary at all where the bounding set lacks a capability
// the file permits, so the image runs only where warp grants CAP_NET_ADMIN, as smtpedge's unit does.
package filecap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The extended attribute's name.
const AttributeName = "security.capability"

// CAP_NET_ADMIN's number (include/uapi/linux/capability.h).
const CapNetAdmin = 12

// The attribute's identifiers (include/uapi/linux/capability.h): magic_etc's revision byte and its one flag, the
// width of each of its fields, and how many capabilities its two words can name.
const (
	revisionShift  = 24
	revisionMask   = 0xff000000
	flagEffective  = 0x000001
	fieldByteCount = 4
	capabilityBits = 64
)

// The attribute's absence: the file holds no capability.
var ErrNoFileCapability = errors.New("no security.capability attribute")

// libcap's names by capability number, through CAP_CHECKPOINT_RESTORE, the last the kernel defines today; a
// number past them prints as the number, as libcap's cap_to_name does.
var capabilityNames = []string{
	"cap_chown",
	"cap_dac_override",
	"cap_dac_read_search",
	"cap_fowner",
	"cap_fsetid",
	"cap_kill",
	"cap_setgid",
	"cap_setuid",
	"cap_setpcap",
	"cap_linux_immutable",
	"cap_net_bind_service",
	"cap_net_broadcast",
	"cap_net_admin",
	"cap_net_raw",
	"cap_ipc_lock",
	"cap_ipc_owner",
	"cap_sys_module",
	"cap_sys_rawio",
	"cap_sys_chroot",
	"cap_sys_ptrace",
	"cap_sys_pacct",
	"cap_sys_admin",
	"cap_sys_boot",
	"cap_sys_nice",
	"cap_sys_resource",
	"cap_sys_time",
	"cap_sys_tty_config",
	"cap_mknod",
	"cap_lease",
	"cap_audit_write",
	"cap_audit_control",
	"cap_setfcap",
	"cap_mac_override",
	"cap_mac_admin",
	"cap_syslog",
	"cap_wake_alarm",
	"cap_block_suspend",
	"cap_audit_read",
	"cap_perfmon",
	"cap_bpf",
	"cap_checkpoint_restore",
}

// One file's capability attribute, decoded. Bit n of a set is capability n.
type FileCapability struct {
	// 1, 2 or 3
	Revision int
	// every permitted or inheritable capability is effective from exec
	Effective   bool
	Permitted   uint64
	Inheritable uint64
	// revision 3's user namespace owner, the uid outside it of the root that wrote it; 0 in revisions 1 and 2
	RootId uint32
}

// cap_net_admin=ep as revision 2: what the image's helper writes on smtpedge's binary, the bytes setcap
// cap_net_admin+ep writes outside a user namespace, and what the image's checks require there.
func NetAdminFileCapability() FileCapability {
	return FileCapability{
		Revision:  2,
		Effective: true,
		Permitted: 1 << CapNetAdmin,
	}
}

// How many 32-bit words of each set a revision holds.
func revisionWordCount(revision int) (int, error) {
	switch revision {
	case 1:
		return 1, nil
	case 2, 3:
		return 2, nil
	default:
		return 0, fmt.Errorf("revision %d is not a file capability revision", revision)
	}
}

// The attribute's bytes.
func (self FileCapability) Encode() ([]byte, error) {
	wordCount, err := revisionWordCount(self.Revision)
	if err != nil {
		return nil, err
	}
	if wordCount == 1 && (self.Permitted>>32 != 0 || self.Inheritable>>32 != 0) {
		return nil, errors.New("revision 1 holds capabilities 0 to 31 only")
	}
	if self.Revision != 3 && self.RootId != 0 {
		return nil, fmt.Errorf("revision %d holds no rootid", self.Revision)
	}
	magic := uint32(self.Revision) << revisionShift
	if self.Effective {
		magic |= flagEffective
	}
	value := binary.LittleEndian.AppendUint32(nil, magic)
	for i := 0; i < wordCount; i += 1 {
		value = binary.LittleEndian.AppendUint32(value, uint32(self.Permitted>>(32*i)))
		value = binary.LittleEndian.AppendUint32(value, uint32(self.Inheritable>>(32*i)))
	}
	if self.Revision == 3 {
		value = binary.LittleEndian.AppendUint32(value, self.RootId)
	}
	return value, nil
}

// Reads an attribute's bytes as libcap's getcap and the kernel do: a known revision, exactly that revision's
// length, and magic_etc's other low bits, which no revision defines, ignored.
func DecodeFileCapability(value []byte) (FileCapability, error) {
	if len(value) < fieldByteCount {
		return FileCapability{}, fmt.Errorf("%d bytes hold no magic_etc", len(value))
	}
	magic := binary.LittleEndian.Uint32(value)
	revision := int((magic & revisionMask) >> revisionShift)
	wordCount, err := revisionWordCount(revision)
	if err != nil {
		return FileCapability{}, err
	}
	// magic_etc, a permitted and an inheritable field a word, and revision 3's rootid
	byteCount := fieldByteCount * (1 + 2*wordCount)
	if revision == 3 {
		byteCount += fieldByteCount
	}
	if len(value) != byteCount {
		return FileCapability{}, fmt.Errorf("revision %d is %d bytes, not %d", revision, byteCount, len(value))
	}
	capability := FileCapability{
		Revision:  revision,
		Effective: magic&flagEffective != 0,
	}
	for i := 0; i < wordCount; i += 1 {
		at := fieldByteCount * (1 + 2*i)
		capability.Permitted |= uint64(binary.LittleEndian.Uint32(value[at:])) << (32 * i)
		capability.Inheritable |= uint64(binary.LittleEndian.Uint32(value[at+fieldByteCount:])) << (32 * i)
	}
	if revision == 3 {
		capability.RootId = binary.LittleEndian.Uint32(value[byteCount-fieldByteCount:])
	}
	return capability, nil
}

// The capability as libcap's cap_to_text writes it, which getcap prints after the file's name: "cap_net_admin=ep"
// for NetAdminFileCapability. The text names the prevailing combination of sets over the named capabilities as
// its base, "=" when that is none, then each other combination's capabilities with the sets they add (+) and
// drop (-), the first clause after an empty base written as "names=sets"; capabilities past the named ones
// follow as numbers, each added.
func (self FileCapability) String() string {
	effective := uint64(0)
	if self.Effective {
		effective = self.Permitted | self.Inheritable
	}
	// libcap's set bits: e 1, i 2, p 4
	flags := func(n int) int {
		f := 0
		if effective>>n&1 != 0 {
			f |= 1
		}
		if self.Inheritable>>n&1 != 0 {
			f |= 2
		}
		if self.Permitted>>n&1 != 0 {
			f |= 4
		}
		return f
	}
	letters := func(f int) string {
		s := ""
		if f&1 != 0 {
			s += "e"
		}
		if f&2 != 0 {
			s += "i"
		}
		if f&4 != 0 {
			s += "p"
		}
		return s
	}
	name := func(n int) string {
		if n < len(capabilityNames) {
			return capabilityNames[n]
		}
		return strconv.Itoa(n)
	}
	namesWith := func(f int, from int, to int) string {
		names := []string{}
		for n := from; n < to; n += 1 {
			if flags(n) == f {
				names = append(names, name(n))
			}
		}
		return strings.Join(names, ",")
	}

	namedCount := len(capabilityNames)
	histogram := [8]int{}
	for n := 0; n < namedCount; n += 1 {
		histogram[flags(n)] += 1
	}
	// the combination most named capabilities share, the unset one on a tie
	prevailing := 7
	for f := 6; 0 <= f; f -= 1 {
		if histogram[prevailing] <= histogram[f] {
			prevailing = f
		}
	}

	text := "=" + letters(prevailing)
	// where the text starts: libcap drops a leading "= " for "names=sets"
	start := 0
	for f := 7; 0 <= f; f -= 1 {
		if f == prevailing || histogram[f] == 0 {
			continue
		}
		text += " " + namesWith(f, 0, namedCount)
		if added := f &^ prevailing; added != 0 {
			op := "+"
			if text[start] == '=' && text[start+1] == ' ' {
				start += 2
				op = "="
			}
			text += op + letters(added)
		}
		if dropped := prevailing &^ f; dropped != 0 {
			text += "-" + letters(dropped)
		}
	}
	unnamedHistogram := [8]int{}
	for n := namedCount; n < capabilityBits; n += 1 {
		unnamedHistogram[flags(n)] += 1
	}
	for f := 7; 1 <= f; f -= 1 {
		if unnamedHistogram[f] != 0 {
			text += " " + namesWith(f, namedCount, capabilityBits) + "+" + letters(f)
		}
	}
	return text[start:]
}

// What a refusal says was found: the text getcap prints, with revision 3's rootid as getcap gives it.
func (self FileCapability) describe() string {
	text := fmt.Sprintf("revision %d %s", self.Revision, self)
	if self.RootId != 0 && self.RootId != math.MaxUint32 {
		text += fmt.Sprintf(" [rootid=%d]", self.RootId)
	}
	return text
}

// Holds an attribute's bytes to NetAdminFileCapability's, byte for byte, the only value smtpedge's binary may
// carry: no other capability, nothing inheritable, and revision 2, since a revision 3 attribute grants nothing
// outside the user namespace its rootid names, which is not where the hosts run the block. A refusal says what
// was found.
func CheckNetAdmin(value []byte) error {
	wantCapability := NetAdminFileCapability()
	want, err := wantCapability.Encode()
	if err != nil {
		return err
	}
	if bytes.Equal(value, want) {
		return nil
	}
	found, err := DecodeFileCapability(value)
	if err != nil {
		return fmt.Errorf("%s does not decode: %w", AttributeName, err)
	}
	return fmt.Errorf("%s holds %s, not %s", AttributeName, found.describe(), wantCapability.describe())
}
