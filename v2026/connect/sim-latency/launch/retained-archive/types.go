// First-committed evidence retention is separate from calibration acceptance.
// This package is unqualified until its synthetic tests and independent review pass.
package main

import (
	"context"
	"io"
	"time"
)

// These identities describe the new archive protocol, never the earlier bridge.
const (
	archivePlanKind      = "first-committed-retained-archive-plan"
	archiveInventoryKind = "first-committed-retained-archive-inventory"
	inventoryMemberName  = "ARCHIVE-CONTENTS.json"
)

// All resource bounds belong to one command invocation. Non-positive values refuse.
type archiveSettings struct {
	// Maximum size of an existing or newly prepared tar, in bytes.
	MaximumArchiveBytes int64
	// Maximum bytes in each plan, inventory, or private metadata document.
	MaximumMetadataBytes int64
	// Maximum regular payload files, excluding the inventory's internal copy.
	MaximumEntries int
	// Maximum examined source directories and files together.
	MaximumTreeEntries int
	// Maximum nested source directories; limits descriptors and recursion.
	MaximumSourceDepth int
	// Maximum entries returned by one directory read; bounds census scratch.
	DirectoryBatchEntries int
	// Maximum administrative JSON nesting; deeper input refuses before decoding.
	MaximumMetadataDepth int
	// Additional free disk required beyond the tar and declared staging reserve.
	MinimumFreeBytes int64
	// Maximum declared extra staging reservation, in bytes.
	MaximumStagingBytes int64
	// Streaming scratch-buffer bytes; no whole archive is retained in memory.
	BufferBytes int
	// Bounds preparation and local verification, including source rechecks.
	VerificationTimeout time.Duration
	// Bounds the single upload, protection checks, and exact-version readback.
	UploadTimeout time.Duration
}

// Defaults bound complete campaign archives without introducing a measurement deadline.
func defaultArchiveSettings() archiveSettings {
	return archiveSettings{
		MaximumArchiveBytes:   4 * 1024 * 1024 * 1024,
		MaximumMetadataBytes:  4 * 1024 * 1024,
		MaximumEntries:        8192,
		MaximumTreeEntries:    16384,
		MaximumSourceDepth:    64,
		DirectoryBatchEntries: 128,
		MaximumMetadataDepth:  32,
		MinimumFreeBytes:      512 * 1024 * 1024,
		MaximumStagingBytes:   1024 * 1024 * 1024,
		BufferBytes:           64 * 1024,
		VerificationTimeout:   10 * time.Minute,
		UploadTimeout:         30 * time.Minute,
	}
}

// Tests substitute only synthetic roots and identities; runtime flags cannot.
type archivePolicy struct {
	OwnerUid         uint32
	Anchor           string
	WorkParent       string
	WorkPrefix       string
	Context          string
	PublicPlanSha256 string
	KeyPrefix        string
	Bucket           string
	Owner            string
	RetainUntil      time.Time
	SourceRootUnits  map[string]string
}

// Private per-archive namespaces cannot masquerade as the bridge's old namespace.
func defaultArchivePolicy() archivePolicy {
	campaign := "/var/lib/urnetwork/competition-held-out-two-b-c2-a9e8e1a7-20261027-v2/campaign"
	selection := "/var/lib/urnetwork/competition-held-out-first-committed-c2-a9e8e1a7-20261027-v1"
	return archivePolicy{
		OwnerUid: 0, Anchor: "/", WorkParent: "/var/lib/urnetwork",
		WorkPrefix:       "competition-first-committed-retained-archive-",
		Context:          "launch-3a-bridge-first-committed-b-2026-10-27-v1",
		PublicPlanSha256: "a8a57ccfe3d5f050ec76bf475dc1e35fb25011c9b781ee1c90dfa7cca720bb9f",
		KeyPrefix:        "blob/competition/calibration/first-committed-2026-10-28",
		Bucket:           "competition", Owner: "support@ur.xyz",
		RetainUntil: time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC),
		SourceRootUnits: map[string]string{
			campaign:  "campaign",
			selection: "selection-and-recovery",
			"/var/lib/urnetwork/competition-first-committed-management-oom-20261027-v1": "management-oom-forensics",
			campaign + "/round-01/job-01-better":                                        "job-01-better",
			campaign + "/round-01/job-02-worse":                                         "job-02-worse",
			campaign + "/round-01/job-03-no-op":                                         "job-03-no-op",
		},
	}
}

// Pins bind opaque bytes; no private plan, request, credential, or seed is decoded.
type filePin struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

// Each name is both a tar member and its original source-relative regular file.
type archiveEntry struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	Sha256 string `json:"sha256"`
}

// The complete source census excludes its own separately pinned internal copy.
type archiveInventory struct {
	Schema           int            `json:"schema"`
	Kind             string         `json:"kind"`
	Context          string         `json:"context"`
	PublicPlanSha256 string         `json:"public_plan_sha256"`
	SourceRoot       string         `json:"source_root"`
	Unit             string         `json:"unit"`
	ExcludesItself   bool           `json:"inventory_excludes_itself"`
	Entries          []archiveEntry `json:"entries"`
}

// A root-reviewed private plan binds exactly one source snapshot and new object.
type archivePlan struct {
	Schema              int       `json:"schema"`
	Kind                string    `json:"kind"`
	Context             string    `json:"context"`
	PublicPlanSha256    string    `json:"public_plan_sha256"`
	SourceRoot          string    `json:"source_root"`
	Unit                string    `json:"unit"`
	Inventory           filePin   `json:"inventory"`
	ArchivePath         string    `json:"archive_path"`
	ArchiveBytes        int64     `json:"archive_bytes"`
	ArchiveSha256       string    `json:"archive_sha256"`
	StorageNonce        string    `json:"storage_nonce"`
	Key                 string    `json:"key"`
	Owner               string    `json:"owner"`
	RetainUntil         time.Time `json:"retain_until"`
	StagingReserveBytes int64     `json:"staging_reserve_bytes"`
}

// Preparation is explicit and local; it never constructs a blob transport.
type prepareRequest struct {
	SourceRoot          string
	StorageNonce        string
	StagingReserveBytes int64
}

// Upload needs both an explicit mode and the exact already-reviewed plan digest.
type archiveRequest struct {
	PlanPin                   filePin
	Upload                    bool
	UploadAuthorizationSha256 string
}

// Reports contain only archive identities, never decoded source contents.
type archiveReport struct {
	Mode              string  `json:"mode"`
	Plan              filePin `json:"plan"`
	InventorySha256   string  `json:"inventory_sha256"`
	ArchiveSha256     string  `json:"archive_sha256"`
	ArchiveBytes      int64   `json:"archive_bytes"`
	PayloadFiles      int     `json:"payload_files"`
	ArchiveMembers    int     `json:"archive_members"`
	ReceiptPath       string  `json:"receipt_path,omitempty"`
	AcceptanceClaimed bool    `json:"calibration_or_launch_acceptance_claimed"`
}

// Production proof identifies one protected authority, not an ad-hoc endpoint.
type archiveProtection struct {
	Authority          string   `json:"authority"`
	Bucket             string   `json:"bucket"`
	ObjectLock         string   `json:"object_lock"`
	Versioning         bool     `json:"versioning"`
	ReplicationTargets []string `json:"replication_targets"`
}

// A returned immutable version must match every requested retention fact.
type archiveRetention struct {
	Key         string    `json:"key"`
	Size        int64     `json:"size"`
	VersionId   string    `json:"version_id"`
	Mode        string    `json:"mode"`
	RetainUntil time.Time `json:"retain_until"`
}

// The transport has no delete, resync, current-version read, or retry operation.
type archiveTransport interface {
	CheckProtection(context.Context) (archiveProtection, error)
	ListVersions(context.Context, string, func() error) error
	PutRetained(context.Context, string, string, time.Time) (archiveRetention, error)
	GetVersion(context.Context, string, string) (io.ReadCloser, error)
}

// Instance-local observations make every failure path deterministic in tests.
type archiveHooks struct {
	Now            func() time.Time
	AvailableBytes func(string) (int64, error)
	AcquireSlot    func() (io.Closer, error)
	NewTransport   func(archivePolicy) (archiveTransport, error)
	AfterStep      func(string) error
}
