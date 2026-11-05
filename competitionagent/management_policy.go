// A protected ledger partitions the existing management reserve exactly once.
// Structural policy validation is not native resource or pressure admission.
package competitionagent

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
)

// This profile identifies the approved memory partition, not a host waiver.
const ManagementResourcePolicyProfile = "urnetwork-management-24g/v1"

// Roles are finite ledger identities; individual daemons cannot add reserves.
const (
	ManagementCoordinatorRole = "coordinator"
	ManagementPrivateRole     = "private"
	ManagementCleanupRole     = "cleanup"
	ManagementCoreRole        = "core"
	ManagementOsReservedRole  = "os-reserved"
)

// Unit identities are fixed protocol facts. Recovery deliberately does not
// descend from the ordinary management parent's binding memory or task limit.
const (
	managementParentSliceName       = "urnetwork-management.slice"
	managementParentCgroupPath      = "/urnetwork.slice/urnetwork-management.slice"
	managementCoordinatorSliceName  = "urnetwork-management-coordinator.slice"
	managementCoordinatorCgroupPath = managementParentCgroupPath + "/urnetwork-management-coordinator.slice"
	managementPrivateSliceName      = "urnetwork-management-private.slice"
	managementPrivateCgroupPath     = managementParentCgroupPath + "/urnetwork-management-private.slice"
	managementCleanupSliceName      = "urnetwork-recovery.slice"
	managementCleanupCgroupPath     = "/urnetwork.slice/urnetwork-recovery.slice"
	managementCoreSliceName         = "urnetwork-core-handler.slice"
	managementCoreCgroupPath        = "/urnetwork.slice/urnetwork-core.slice/urnetwork-core-handler.slice"
)

// Expected limits belong to the admitting owner. Memory values identify this
// approved profile and cannot be repartitioned under the same profile name.
// Task limits for ordinary/recovery roles are explicit prospective settings;
// native exhaustion/pressure acceptance remains a separate requirement.
type ManagementBoundarySettings struct {
	// Bytes in the original reserve; zero or a different profile value refuses.
	ReserveBytes int64
	// Bytes in the binding coordinator/private parent, excluding recovery/core.
	ParentMemoryBytes int64
	// Bytes for all coordinator processes together, never a per-process allowance.
	CoordinatorMemoryBytes int64
	// Bytes for the aggregate private engine, containerd, shims and helpers.
	PrivateMemoryBytes int64
	// Bytes for independent cleanup, outside ordinary binding ancestors.
	CleanupMemoryBytes int64
	// Bytes in the existing independently bounded core-handler class.
	CoreMemoryBytes int64
	// Bytes reserved once for the OS and all other admitted ambient ownership.
	OsReservedMemoryBytes int64
	// Parent tasks equal the sum of coordinator and private child ceilings.
	ParentTasksMaximum int64
	// Positive task ceilings; zero or negative refuses rather than disables.
	CoordinatorTasksMaximum int64
	PrivateTasksMaximum     int64
	CleanupTasksMaximum     int64
	// The existing core-handler class retains its independently reviewed ceiling.
	CoreTasksMaximum int64
	// Bytes of swap; this profile requires explicitly zero, never inheritance.
	SwapBytes int64
	// Inclusive CPU-index parsing bound, positive and no wider than this profile.
	// It bounds CPU-range expansion before reading an untrusted policy document.
	MaximumCpuIndex int
}

// The same 24 GiB includes ordinary management, independent recovery/core and
// the OS reserve. No extra coordinator allowance exists outside this ledger.
func DefaultManagementBoundarySettings() ManagementBoundarySettings {
	return ManagementBoundarySettings{
		ReserveBytes: 24 * 1024 * 1024 * 1024, ParentMemoryBytes: 16 * 1024 * 1024 * 1024,
		CoordinatorMemoryBytes: 8 * 1024 * 1024 * 1024, PrivateMemoryBytes: 8 * 1024 * 1024 * 1024,
		CleanupMemoryBytes: 2 * 1024 * 1024 * 1024, CoreMemoryBytes: 256 * 1024 * 1024,
		OsReservedMemoryBytes: 5888 * 1024 * 1024,
		ParentTasksMaximum:    20480, CoordinatorTasksMaximum: 4096, PrivateTasksMaximum: 16384,
		CleanupTasksMaximum: 1024, CoreTasksMaximum: 128, SwapBytes: 0, MaximumCpuIndex: 4095,
	}
}

// Pins identify rendered, protected unit files, not these inert templates.
// Root ownership, no-symlink traversal and actual bytes must be checked by the
// protected loader; native manager/kernel identities are checked independently.
type ManagementSlicePolicy struct {
	SliceName    string `json:"slice_name"`
	CgroupPath   string `json:"cgroup_path"`
	Unit         Pin    `json:"unit"`
	MemoryBytes  int64  `json:"memory_bytes"`
	SwapBytes    int64  `json:"swap_bytes"`
	TasksMaximum int64  `json:"tasks_maximum"`
}

// Every role occurs once. OS-reserved has bytes but no cgroup, unit, task or
// swap allowance; pretending that reserved headroom is a slice is invalid.
type ManagementRolePolicy struct {
	Role         string `json:"role"`
	SliceName    string `json:"slice_name"`
	CgroupPath   string `json:"cgroup_path"`
	Unit         Pin    `json:"unit"`
	MemoryBytes  int64  `json:"memory_bytes"`
	SwapBytes    int64  `json:"swap_bytes"`
	TasksMaximum int64  `json:"tasks_maximum"`
}

// The enclosing root-owned configuration must pin this document's exact bytes.
// Parent is a second-level constraint, not another entry in the 24 GiB sum.
// There is intentionally no Boolean that accepts native proof or pressure.
type ManagementResourcePolicy struct {
	Schema           int                    `json:"schema"`
	Profile          string                 `json:"profile"`
	ReserveBytes     int64                  `json:"reserve_bytes"`
	ManagementCpuSet string                 `json:"management_cpuset"`
	Parent           ManagementSlicePolicy  `json:"parent"`
	Roles            []ManagementRolePolicy `json:"roles"`
}

// Validation proves only the explicit profile, arithmetic and pinned identities.
// Admission additionally needs fresh actual headroom/ambient ceilings, complete
// process/cgroup closure, and independent cleanup/coordinator pressure evidence.
// Neither a valid ledger nor matching rendered unit text supplies that proof.
func ValidateManagementResourcePolicy(policy ManagementResourcePolicy, settings ManagementBoundarySettings) error {
	if err := settings.validate(); err != nil {
		return err
	}
	if policy.Schema != Schema || policy.Profile != ManagementResourcePolicyProfile || policy.ReserveBytes != settings.ReserveBytes {
		return errors.New("management ledger must explicitly select the approved reserve profile")
	}
	if _, err := dockerPolicyCpuSet(policy.ManagementCpuSet, settings.MaximumCpuIndex); err != nil {
		return errors.New("management ledger must explicitly pin a finite canonical CPU set")
	}
	if err := validateManagementSlice(policy.Parent, managementParentSliceName, managementParentCgroupPath); err != nil {
		return err
	}
	if policy.Parent.MemoryBytes != settings.ParentMemoryBytes || policy.Parent.SwapBytes != settings.SwapBytes ||
		policy.Parent.TasksMaximum != settings.ParentTasksMaximum {
		return errors.New("ordinary management parent differs from its owning settings")
	}
	expected := map[string]ManagementRolePolicy{
		ManagementCoordinatorRole: {Role: ManagementCoordinatorRole, SliceName: managementCoordinatorSliceName, CgroupPath: managementCoordinatorCgroupPath,
			MemoryBytes: settings.CoordinatorMemoryBytes, SwapBytes: settings.SwapBytes, TasksMaximum: settings.CoordinatorTasksMaximum},
		ManagementPrivateRole: {Role: ManagementPrivateRole, SliceName: managementPrivateSliceName, CgroupPath: managementPrivateCgroupPath,
			MemoryBytes: settings.PrivateMemoryBytes, SwapBytes: settings.SwapBytes, TasksMaximum: settings.PrivateTasksMaximum},
		ManagementCleanupRole: {Role: ManagementCleanupRole, SliceName: managementCleanupSliceName, CgroupPath: managementCleanupCgroupPath,
			MemoryBytes: settings.CleanupMemoryBytes, SwapBytes: settings.SwapBytes, TasksMaximum: settings.CleanupTasksMaximum},
		ManagementCoreRole: {Role: ManagementCoreRole, SliceName: managementCoreSliceName, CgroupPath: managementCoreCgroupPath,
			MemoryBytes: settings.CoreMemoryBytes, SwapBytes: settings.SwapBytes, TasksMaximum: settings.CoreTasksMaximum},
		ManagementOsReservedRole: {Role: ManagementOsReservedRole, MemoryBytes: settings.OsReservedMemoryBytes},
	}
	if len(policy.Roles) != len(expected) {
		return errors.New("management ledger must contain every role exactly once")
	}
	seenRoles := map[string]bool{}
	seenUnits := map[string]bool{policy.Parent.Unit.Path: true}
	totalBytes := int64(0)
	for _, role := range policy.Roles {
		if role.MemoryBytes <= 0 || math.MaxInt64-totalBytes < role.MemoryBytes {
			return errors.New("management memory ledger is nonpositive or overflows")
		}
		totalBytes += role.MemoryBytes
	}
	for _, role := range policy.Roles {
		want, known := expected[role.Role]
		if !known || seenRoles[role.Role] {
			return errors.New("management ledger contains an unknown or duplicate role")
		}
		seenRoles[role.Role] = true
		if role.MemoryBytes != want.MemoryBytes || role.SwapBytes != want.SwapBytes || role.TasksMaximum != want.TasksMaximum ||
			role.SliceName != want.SliceName || role.CgroupPath != want.CgroupPath {
			return fmt.Errorf("management role %q differs from its pinned identity or limits", role.Role)
		}
		if role.Role == ManagementOsReservedRole {
			if role.Unit != (Pin{}) {
				return errors.New("OS reserve cannot invent a bounded unit or a second allocation")
			}
			continue
		}
		slice := ManagementSlicePolicy{SliceName: role.SliceName, CgroupPath: role.CgroupPath, Unit: role.Unit,
			MemoryBytes: role.MemoryBytes, SwapBytes: role.SwapBytes, TasksMaximum: role.TasksMaximum}
		if err := validateManagementSlice(slice, want.SliceName, want.CgroupPath); err != nil {
			return err
		}
		if seenUnits[role.Unit.Path] {
			return errors.New("management ledger reuses a pinned unit for distinct owners")
		}
		seenUnits[role.Unit.Path] = true
	}
	if totalBytes != policy.ReserveBytes {
		return errors.New("management roles do not partition the original reserve exactly once")
	}
	return nil
}

// A new memory partition requires a new reviewed profile, not altered settings
// under the existing profile name. Independent finite task tuning stays explicit.
func (self ManagementBoundarySettings) validate() error {
	approved := DefaultManagementBoundarySettings()
	if self.MaximumCpuIndex <= 0 || self.MaximumCpuIndex > approved.MaximumCpuIndex {
		return errors.New("management CPU parser has no finite profile bound")
	}
	if self.ReserveBytes != approved.ReserveBytes || self.ParentMemoryBytes != approved.ParentMemoryBytes ||
		self.CoordinatorMemoryBytes != approved.CoordinatorMemoryBytes || self.PrivateMemoryBytes != approved.PrivateMemoryBytes ||
		self.CleanupMemoryBytes != approved.CleanupMemoryBytes || self.CoreMemoryBytes != approved.CoreMemoryBytes ||
		self.OsReservedMemoryBytes != approved.OsReservedMemoryBytes || self.CoreTasksMaximum != approved.CoreTasksMaximum || self.SwapBytes != 0 {
		return errors.New("management settings do not match the approved memory/core profile")
	}
	if self.CoordinatorTasksMaximum <= 0 || self.PrivateTasksMaximum <= 0 || self.CleanupTasksMaximum <= 0 ||
		math.MaxInt64-self.CoordinatorTasksMaximum < self.PrivateTasksMaximum {
		return errors.New("management task ceilings are nonpositive or overflow")
	}
	if self.ParentTasksMaximum != self.CoordinatorTasksMaximum+self.PrivateTasksMaximum {
		return errors.New("ordinary parent tasks must count coordinator and private children only")
	}
	return nil
}

// String identity and pin shape checks are pure. They cannot authorize a unit
// selected by a candidate or replace protected-file/native generation checks.
func validateManagementSlice(slice ManagementSlicePolicy, name, cgroup string) error {
	if slice.SliceName != name || slice.CgroupPath != cgroup || slice.MemoryBytes <= 0 || slice.SwapBytes != 0 || slice.TasksMaximum <= 0 {
		return errors.New("management slice has an unexpected identity or unbounded limits")
	}
	if !filepath.IsAbs(slice.Unit.Path) || filepath.Clean(slice.Unit.Path) != slice.Unit.Path || filepath.Base(slice.Unit.Path) != name ||
		strings.ContainsAny(slice.Unit.Path, "\x00\r\n\t") || !digestPattern.MatchString(slice.Unit.Sha256) {
		return errors.New("management slice requires its exact canonical rendered-unit pin")
	}
	return nil
}
