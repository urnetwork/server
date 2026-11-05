// The prospective direct-container class preserves a pinned stock seccomp
// profile except for resource-limit writes. Both core limits must start at one:
// the reviewed kernel pipe path suppresses its helper at one, not at zero.
// Profile admission is not matched-kernel or crash-storm qualification.
package competitionagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Bounds belong to the profile owner. Nonpositive values refuse admission;
// they never disable a check or authorize another syscall or architecture.
type CandidateCoreProfileSettings struct {
	DocumentBytes     int64 // Bytes per stock, derived, or inline document; default fits the reviewed stock profile.
	DocumentDepth     int   // Json nesting levels; default covers the closed Docker profile schema.
	RuleCount         int   // Rules after restrictive expansion; default leaves space for all stock predicates.
	SyscallNameCount  int   // Total names across all rules, including expanded resource alternatives.
	ArgumentCount     int   // Comparisons per rule; the syscall ABI itself still has only six argument slots.
	ArchitectureCount int   // Entries in each architecture collection, including foreign stock mappings preserved verbatim.
	ConditionCount    int   // Capability/architecture conditions per rule.
	StringBytes       int   // Bytes per name, comment, version, or other string token.
}

// This is a document-work budget, not a profile authority or native pass flag.
func DefaultCandidateCoreProfileSettings() CandidateCoreProfileSettings {
	return CandidateCoreProfileSettings{DocumentBytes: 512 * 1024, DocumentDepth: 32, RuleCount: 4096,
		SyscallNameCount: 8192, ArgumentCount: 6, ArchitectureCount: 32, ConditionCount: 64, StringBytes: 16 * 1024}
}

// Resource numbers and low-word conversion are Linux x86 ABI facts, not knobs.
const (
	candidateCoreResource = uint64(4)
	candidateResourceEnd  = uint64(16)
	candidateResourceMask = uint64(math.MaxUint32)
)

var candidateSeccompNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
var candidateSeccompArchPattern = regexp.MustCompile(`^SCMP_ARCH_[A-Z0-9_]+$`)
var candidateSeccompKernelPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
var candidateSeccompCapabilityPattern = regexp.MustCompile(`^CAP_[A-Z0-9_]+$`)

// Only the reviewed Docker/OCI profile vocabulary is accepted. Unknown fields
// must be reviewed rather than discarded during a restrictive transformation.
type candidateSeccompProfile struct {
	DefaultAction    string                         `json:"defaultAction"`
	DefaultErrnoRet  *uint32                        `json:"defaultErrnoRet,omitempty"`
	Architectures    []string                       `json:"architectures,omitempty"`
	ArchMap          []candidateSeccompArchitecture `json:"archMap,omitempty"`
	Flags            []string                       `json:"flags,omitempty"`
	ListenerPath     string                         `json:"listenerPath,omitempty"`
	ListenerMetadata string                         `json:"listenerMetadata,omitempty"`
	Syscalls         []candidateSeccompSyscall      `json:"syscalls"`
}

// Stock mappings are retained, including foreign entries unused on this class.
type candidateSeccompArchitecture struct {
	Architecture     string   `json:"architecture"`
	SubArchitectures []string `json:"subArchitectures"`
}

// Conditions are copied, never generalized by a generated allow alternative.
type candidateSeccompCondition struct {
	Caps      []string `json:"caps,omitempty"`
	Arches    []string `json:"arches,omitempty"`
	MinKernel string   `json:"minKernel,omitempty"`
}

// Each generated rule contains one comparison per argument index. In
// particular, it does not depend on repeated-index conjunction in runc.
type candidateSeccompArgument struct {
	Index    uint   `json:"index"`
	Value    uint64 `json:"value"`
	ValueTwo uint64 `json:"valueTwo,omitempty"`
	Op       string `json:"op"`
}

// Deprecated singular names and notification/trace authority are deliberately
// outside this class; the actual stock export must satisfy the closed schema.
type candidateSeccompSyscall struct {
	Names    []string                   `json:"names"`
	Action   string                     `json:"action"`
	ErrnoRet *uint32                    `json:"errnoRet,omitempty"`
	Args     []candidateSeccompArgument `json:"args,omitempty"`
	Comment  string                     `json:"comment,omitempty"`
	Includes *candidateSeccompCondition `json:"includes,omitempty"`
	Excludes *candidateSeccompCondition `json:"excludes,omitempty"`
}

// No additional permission is introduced: original allow rules for the two
// setters become their intersection with non-core resources, or a core query.
// Appending an errno rule while retaining an unconditional allow is unsafe.
func TransformCandidateCoreProfile(stock []byte, settings CandidateCoreProfileSettings) ([]byte, error) {
	profile, err := parseCandidateCoreProfile(stock, settings)
	if err != nil {
		return nil, err
	}
	result := *profile
	result.Syscalls = nil
	unconditional := map[string]bool{}
	for _, rule := range profile.Syscalls {
		retainedNames := make([]string, 0, len(rule.Names))
		for _, name := range rule.Names {
			if name != "setrlimit" && name != "prlimit64" || rule.Action != "SCMP_ACT_ALLOW" {
				retainedNames = append(retainedNames, name)
				continue
			}
			// Fail closed on an unreviewed stock setter predicate. Repeated
			// argument indexes are not a safe conjunction in runc/libseccomp.
			if len(rule.Args) != 0 || rule.ErrnoRet != nil {
				return nil, errors.New("stock resource setters have unreviewed argument predicates")
			}
			if emptyCandidateSeccompCondition(rule.Includes) && emptyCandidateSeccompCondition(rule.Excludes) {
				unconditional[name] = true
			}
			resourceIndex := uint(0)
			if name == "prlimit64" {
				resourceIndex = 1
			}
			for resource := uint64(0); resource < candidateResourceEnd; resource++ {
				if resource == candidateCoreResource && name == "setrlimit" {
					continue
				}
				generated := rule
				generated.Names = []string{name}
				generated.Args = []candidateSeccompArgument{{Index: resourceIndex, Value: candidateResourceMask, ValueTwo: resource, Op: "SCMP_CMP_MASKED_EQ"}}
				if resource == candidateCoreResource {
					// A non-null old_limit does not make a write harmless:
					// Linux applies new_limit before copying old_limit out.
					generated.Args = append(generated.Args, candidateSeccompArgument{Index: 2, Value: 0, Op: "SCMP_CMP_EQ"})
				}
				result.Syscalls = append(result.Syscalls, generated)
			}
		}
		if len(retainedNames) != 0 {
			retained := rule
			retained.Names = retainedNames
			result.Syscalls = append(result.Syscalls, retained)
		}
		if len(result.Syscalls) > settings.RuleCount {
			return nil, errors.New("derived core profile exceeds its rule bound")
		}
	}
	if !unconditional["setrlimit"] || !unconditional["prlimit64"] {
		return nil, errors.New("stock profile does not contain the reviewed unconditional resource-setter class")
	}
	content, err := json.Marshal(&result)
	if err != nil {
		return nil, err
	}
	if _, err := parseCandidateCoreProfile(content, settings); err != nil {
		return nil, err
	}
	return content, nil
}

// A missing pin cannot fall back to Docker's mutable default. Both files are
// captured through raw no-follow directory walks and hashed as actually read.
// Returned bytes are the canonical profile the Docker API must receive inline.
func ValidateCandidateCoreProfiles(stock, candidate Pin, settings CandidateCoreProfileSettings, protected bool) ([]byte, error) {
	if err := settings.validate(); err != nil {
		return nil, err
	}
	stockContent, err := readCandidateCoreProfilePin(stock, settings, protected)
	if err != nil {
		return nil, err
	}
	expected, err := TransformCandidateCoreProfile(stockContent, settings)
	if err != nil {
		return nil, err
	}
	candidateContent, err := readCandidateCoreProfilePin(candidate, settings, protected)
	if err != nil {
		return nil, err
	}
	profile, err := parseCandidateCoreProfile(candidateContent, settings)
	if err != nil {
		return nil, err
	}
	actual, err := json.Marshal(profile)
	if err != nil || !bytes.Equal(actual, expected) {
		return nil, errors.New("candidate core profile is not the exact restrictive stock transformation")
	}
	return expected, nil
}

// The caller cannot supply another profile path, unconfined mode, LSM label,
// or duplicate security option. Docker CLI embeds the file content in this API.
func (self *DockerProxy) verifyCandidateCoreSecurity(options []string) error {
	if len(options) != 2 {
		return errors.New("Docker candidate omitted its exact core-suppression security class")
	}
	var inline []byte
	nnp := false
	for _, option := range options {
		switch {
		case option == "no-new-privileges:true" || option == "no-new-privileges=true":
			if nnp {
				return errors.New("Docker candidate repeated no-new-privileges")
			}
			nnp = true
		case strings.HasPrefix(option, "seccomp="):
			if inline != nil {
				return errors.New("Docker candidate repeated its seccomp profile")
			}
			inline = []byte(strings.TrimPrefix(option, "seccomp="))
		default:
			return errors.New("Docker candidate requested another security authority")
		}
	}
	if !nnp || len(inline) == 0 {
		return errors.New("Docker candidate omitted no-new-privileges or its inline profile")
	}
	expected, err := ValidateCandidateCoreProfiles(self.plan.StockSeccompProfile, self.plan.CandidateCoreProfile, self.settings.CoreProfile, self.protected)
	if err != nil {
		return err
	}
	profile, err := parseCandidateCoreProfile(inline, self.settings.CoreProfile)
	if err != nil {
		return err
	}
	actual, err := json.Marshal(profile)
	if err != nil || !bytes.Equal(actual, expected) {
		return errors.New("Docker candidate changed its pinned inline seccomp profile")
	}
	return nil
}

// No setting may overflow the bounded reader or silently disable validation.
func (self CandidateCoreProfileSettings) validate() error {
	if self.DocumentBytes <= 0 || self.DocumentBytes == math.MaxInt64 || self.DocumentDepth <= 0 || self.RuleCount <= 0 || self.SyscallNameCount <= 0 ||
		self.ArgumentCount <= 0 || self.ArchitectureCount <= 0 || self.ConditionCount <= 0 || self.StringBytes <= 0 {
		return errors.New("candidate core profile requires positive finite document bounds")
	}
	return nil
}

// The authority is a regular protected document, never an executable or a
// symlink alias. An ancestor swap cannot redirect a held descriptor traversal.
func readCandidateCoreProfilePin(pin Pin, settings CandidateCoreProfileSettings, protected bool) ([]byte, error) {
	if !filepath.IsAbs(pin.Path) || filepath.Clean(pin.Path) != pin.Path || pin.Path == "/" || !digestPattern.MatchString(pin.Sha256) || strings.ContainsAny(pin.Path, "\x00\r\n") {
		return nil, errors.New("invalid candidate core profile pin")
	}
	if protected {
		if err := protectedRootPath(pin.Path, false, false); err != nil {
			return nil, err
		}
	}
	root, err := openDirectoryNoFollow("/")
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := openRelativeNoFollow(root, strings.TrimPrefix(pin.Path, "/"), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > settings.DocumentBytes {
		return nil, errors.New("candidate core profile is not a bounded regular file")
	}
	if protected {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			return nil, errors.New("candidate core profile is not protected root authority")
		}
	}
	content, err := io.ReadAll(io.LimitReader(file, settings.DocumentBytes+1))
	if err != nil || int64(len(content)) > settings.DocumentBytes || digestDockerBytes(content) != pin.Sha256 {
		return nil, errors.New("candidate core profile pin or byte bound changed")
	}
	return content, nil
}

// Conditions absent or explicitly empty have the same Docker interpretation.
func emptyCandidateSeccompCondition(condition *candidateSeccompCondition) bool {
	return condition == nil || len(condition.Caps) == 0 && len(condition.Arches) == 0 && condition.MinKernel == ""
}

// The native host is fixed to the x86-64 class, but its enabled compat entry
// points must be covered too. An absent x32 entry is not an ABI proof.
func validateCandidateSeccompArchitectures(profile *candidateSeccompProfile, settings CandidateCoreProfileSettings) error {
	check := func(architectures []string) error {
		if len(architectures) > settings.ArchitectureCount {
			return errors.New("candidate seccomp architecture count exceeds its bound")
		}
		seen := map[string]bool{}
		for _, architecture := range architectures {
			if !candidateSeccompArchPattern.MatchString(architecture) || architecture == "SCMP_ARCH_NATIVE" || seen[architecture] {
				return errors.New("candidate seccomp architecture is duplicate or implicit")
			}
			seen[architecture] = true
		}
		return nil
	}
	complete := func(architectures []string) bool {
		seen := map[string]bool{}
		for _, architecture := range architectures {
			seen[architecture] = true
		}
		return seen["SCMP_ARCH_X86_64"] && seen["SCMP_ARCH_X86"] && seen["SCMP_ARCH_X32"] && len(seen) == 3
	}
	if len(profile.Architectures) != 0 {
		if len(profile.ArchMap) != 0 || check(profile.Architectures) != nil || !complete(profile.Architectures) {
			return errors.New("candidate seccomp profile omits an explicit x86 ABI or has two architecture authorities")
		}
		return nil
	}
	if len(profile.ArchMap) == 0 || len(profile.ArchMap) > settings.ArchitectureCount {
		return errors.New("candidate seccomp profile lacks its bounded stock architecture map")
	}
	seen := map[string]bool{}
	found := false
	for _, mapping := range profile.ArchMap {
		if check([]string{mapping.Architecture}) != nil || seen[mapping.Architecture] || check(mapping.SubArchitectures) != nil {
			return errors.New("candidate seccomp profile contains an ambiguous stock architecture map")
		}
		seen[mapping.Architecture] = true
		if mapping.Architecture == "SCMP_ARCH_X86_64" {
			architectures := append([]string{mapping.Architecture}, mapping.SubArchitectures...)
			found = complete(architectures) && len(mapping.SubArchitectures) == 2
		}
	}
	if !found {
		return errors.New("candidate seccomp stock map does not cover every x86 ABI")
	}
	return nil
}

// Exact field casing and unique keys are checked before Go's case-insensitive
// typed decoder can fold them. String and depth bounds apply before allocation
// of the typed profile; the enclosing document has an independent byte bound.
func validateCandidateSeccompKeys(content []byte, settings CandidateCoreProfileSettings) error {
	allowed := map[string]bool{}
	for _, name := range strings.Fields("defaultAction defaultErrnoRet architectures archMap architecture subArchitectures flags listenerPath listenerMetadata syscalls names action errnoRet args index value valueTwo op comment includes excludes caps arches minKernel") {
		allowed[name] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	var visit func(int) error
	visit = func(depth int) error {
		if depth > settings.DocumentDepth {
			return errors.New("candidate seccomp document exceeds its depth bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if value, ok := token.(string); ok && len(value) > settings.StringBytes {
			return errors.New("candidate seccomp string exceeds its bound")
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				token, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || !allowed[key] || seen[key] {
					return errors.New("candidate seccomp has duplicate, unknown, or case-aliased fields")
				}
				seen[key] = true
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("candidate seccomp contains an invalid delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("candidate seccomp contains trailing data")
	}
	return nil
}

// All unmodified stock rules remain restrictive or allowing as originally
// pinned. Notify, trace, log-only and listener authority are not this class.
func parseCandidateCoreProfile(content []byte, settings CandidateCoreProfileSettings) (*candidateSeccompProfile, error) {
	if err := settings.validate(); err != nil {
		return nil, err
	}
	if len(content) == 0 || int64(len(content)) > settings.DocumentBytes {
		return nil, errors.New("candidate seccomp document exceeds its byte bound")
	}
	if err := validateCandidateSeccompKeys(content, settings); err != nil {
		return nil, err
	}
	profile := &candidateSeccompProfile{}
	if err := decodeStrict(content, profile); err != nil {
		return nil, err
	}
	if profile.DefaultAction != "SCMP_ACT_ERRNO" || profile.DefaultErrnoRet != nil && *profile.DefaultErrnoRet != 1 ||
		len(profile.Flags) != 0 || profile.ListenerPath != "" || profile.ListenerMetadata != "" || len(profile.Syscalls) == 0 || len(profile.Syscalls) > settings.RuleCount {
		return nil, errors.New("candidate seccomp is not the reviewed default-errno stock class")
	}
	if err := validateCandidateSeccompArchitectures(profile, settings); err != nil {
		return nil, err
	}
	nameCount := 0
	for _, rule := range profile.Syscalls {
		if len(rule.Names) == 0 || len(rule.Names) > settings.SyscallNameCount || len(rule.Args) > settings.ArgumentCount {
			return nil, errors.New("candidate seccomp rule exceeds its name or argument bound")
		}
		if nameCount > settings.SyscallNameCount-len(rule.Names) {
			return nil, errors.New("candidate seccomp total syscall-name count exceeds its bound")
		}
		nameCount += len(rule.Names)
		switch rule.Action {
		case "SCMP_ACT_ALLOW", "SCMP_ACT_ERRNO", "SCMP_ACT_KILL", "SCMP_ACT_KILL_PROCESS", "SCMP_ACT_KILL_THREAD", "SCMP_ACT_TRAP":
		default:
			return nil, errors.New("candidate seccomp rule has unreviewed action authority")
		}
		if rule.ErrnoRet != nil && rule.Action != "SCMP_ACT_ERRNO" {
			return nil, errors.New("candidate seccomp errno belongs to a non-errno action")
		}
		seen := map[string]bool{}
		for _, name := range rule.Names {
			if !candidateSeccompNamePattern.MatchString(name) || seen[name] {
				return nil, errors.New("candidate seccomp syscall name is noncanonical or duplicate")
			}
			seen[name] = true
		}
		for _, argument := range rule.Args {
			if argument.Index >= 6 {
				return nil, errors.New("candidate seccomp argument index exceeds the syscall ABI")
			}
			switch argument.Op {
			case "SCMP_CMP_NE", "SCMP_CMP_LT", "SCMP_CMP_LE", "SCMP_CMP_EQ", "SCMP_CMP_GE", "SCMP_CMP_GT", "SCMP_CMP_MASKED_EQ":
			default:
				return nil, errors.New("candidate seccomp argument has an unreviewed comparison")
			}
		}
		for _, condition := range []*candidateSeccompCondition{rule.Includes, rule.Excludes} {
			if condition == nil {
				continue
			}
			if len(condition.Caps) > settings.ConditionCount || len(condition.Arches) > settings.ConditionCount || condition.MinKernel != "" && !candidateSeccompKernelPattern.MatchString(condition.MinKernel) {
				return nil, errors.New("candidate seccomp condition exceeds its closed bounds")
			}
			for _, capability := range condition.Caps {
				if !candidateSeccompCapabilityPattern.MatchString(capability) {
					return nil, errors.New("candidate seccomp capability condition is noncanonical")
				}
			}
			for _, architecture := range condition.Arches {
				if !candidateSeccompNamePattern.MatchString(architecture) {
					return nil, errors.New("candidate seccomp architecture condition is noncanonical")
				}
			}
		}
	}
	return profile, nil
}
