// Package input contracts keep every service's Docker recipe joined to the
// reviewed byte lock. Tests are offline and need neither services nor Docker.
package runtimepackages

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The manifest records candidate input provenance, not an image approval.
type runtimePackageLock struct {
	Schema            string                `json:"schema"`
	BaseImage         string                `json:"base_image"`
	Snapshot          string                `json:"snapshot"`
	ArchiveSigningKey string                `json:"archive_signing_key"`
	Indexes           []runtimePackageIndex `json:"indexes"`
	Packages          []runtimePackage      `json:"packages"`
	Sets              map[string][]string   `json:"sets"`
	Services          map[string]string     `json:"services"`
}

// Package tables are authenticated by the retained signed parent indexes.
type runtimePackageIndex struct {
	Id              string `json:"id"`
	InReleaseSha256 string `json:"inrelease_sha256"`
	PackagesPath    string `json:"packages_path"`
	PackagesSha256  string `json:"packages_sha256"`
}

// Each destination has exactly one reviewed payload and target architecture.
type runtimePackage struct {
	Id           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Architecture string   `json:"architecture"`
	Filename     string   `json:"filename"`
	Sha256       string   `json:"sha256"`
	Size         int64    `json:"size"`
	IndexIds     []string `json:"index_ids"`
}

// Resolve checked-in inputs from the package directory, where go test runs; the
// caller's path is the module path under -trimpath, which names no directory.
func loadRuntimePackageLock(t *testing.T) (string, runtimePackageLock) {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal("cannot locate runtime package contract: ", err)
	}
	cli := filepath.Clean(filepath.Join(directory, "..", "..", "cli"))
	raw, err := os.ReadFile(filepath.Join(cli, "runtime-packages.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock runtimePackageLock
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatal("trailing package lock data")
	}
	if err := checkRuntimePackageLock(lock); err != nil {
		t.Fatal(err)
	}
	return cli, lock
}

// Validate identities and require both targets for each architecture-specific
// dependency; dropping an arm64 sibling must not silently narrow qualification.
func checkRuntimePackageLock(lock runtimePackageLock) error {
	hexWidth := func(value string, size int) bool {
		raw, err := hex.DecodeString(value)
		return err == nil && len(raw) == size && strings.ToLower(value) == value && strings.Trim(value, "0") != ""
	}
	base, hash, found := strings.Cut(lock.BaseImage, "@sha256:")
	if lock.Schema != "urnetwork-server-runtime-packages-v1" || !found || base != "ubuntu:24.04" || !hexWidth(hash, 32) ||
		!strings.HasPrefix(lock.Snapshot, "https://") || !strings.HasSuffix(lock.Snapshot, "/") || len(lock.ArchiveSigningKey) != 40 {
		return errors.New("invalid package lock identity")
	}
	indexes := map[string]runtimePackageIndex{}
	for _, index := range lock.Indexes {
		_, duplicate := indexes[index.Id]
		parts := strings.Split(index.Id, "/")
		if duplicate || len(parts) != 2 || !slices.Contains([]string{"amd64", "arm64"}, parts[1]) ||
			index.PackagesPath != "main/binary-"+parts[1]+"/Packages.xz" || !hexWidth(index.InReleaseSha256, 32) || !hexWidth(index.PackagesSha256, 32) {
			return errors.New("invalid signed index reference")
		}
		indexes[index.Id] = index
	}
	packages := map[string]runtimePackage{}
	for _, pkg := range lock.Packages {
		_, duplicate := packages[pkg.Id]
		if duplicate || pkg.Id != pkg.Name+"_"+pkg.Architecture || strings.ContainsAny(pkg.Name, " /\\\t\n$*") || pkg.Name == "" || pkg.Version == "" ||
			!slices.Contains([]string{"all", "amd64", "arm64"}, pkg.Architecture) || !hexWidth(pkg.Sha256, 32) || pkg.Size <= 0 || pkg.Size > 8*1024*1024 ||
			path.Clean(pkg.Filename) != pkg.Filename || !strings.HasPrefix(pkg.Filename, "pool/") || strings.ContainsAny(pkg.Filename, " \t\n$?#") || len(pkg.IndexIds) == 0 {
			return fmt.Errorf("invalid package identity %q", pkg.Id)
		}
		architectures := map[string]bool{}
		for _, id := range pkg.IndexIds {
			index, exists := indexes[id]
			if !exists {
				return fmt.Errorf("unknown index %s", id)
			}
			arch := strings.Split(index.Id, "/")[1]
			if pkg.Architecture != "all" && arch != pkg.Architecture {
				return errors.New("package/index architecture mismatch")
			}
			architectures[arch] = true
		}
		if pkg.Architecture == "all" && len(architectures) != 2 {
			return errors.New("shared package is absent from one target's signed index")
		}
		packages[pkg.Id] = pkg
	}
	for name, ids := range lock.Sets {
		if name == "" || len(ids) == 0 {
			return errors.New("empty package set")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			pkg, exists := packages[id]
			if !exists || seen[id] {
				return errors.New("missing or duplicate package in set")
			}
			seen[id] = true
			if pkg.Architecture != "all" {
				peerArch := "arm64"
				if pkg.Architecture == "arm64" {
					peerArch = "amd64"
				}
				peer, exists := packages[pkg.Name+"_"+peerArch]
				if !exists || peer.Version != pkg.Version || !slices.Contains(ids, peer.Id) {
					return fmt.Errorf("incomplete target pair for %s", pkg.Name)
				}
			}
		}
	}
	wantServices := map[string]string{"alt": "certificates", "api": "certificates", "compute": "certificates", "connect": "certificates", "dns": "certificates", "dnsedge": "certificates", "evs": "certificates", "gkm": "certificates", "gossip": "certificates", "mcp": "certificates", "mxedge": "certificates", "mxedge-platform": "certificates", "smtpedge": "certificates", "sockets": "certificates", "taskworker": "certificates", "tlsedge": "certificates", "proxy": "proxy"}
	if !reflect.DeepEqual(lock.Services, wantServices) {
		return errors.New("service package coverage changed")
	}
	wantCertificates := []string{"ca-certificates_all", "libssl3t64_amd64", "libssl3t64_arm64", "openssl_amd64", "openssl_arm64"}
	if !slices.Equal(lock.Sets["certificates"], wantCertificates) || len(lock.Sets) != 2 {
		return errors.New("certificate installation lost its complete target inputs")
	}
	if !slices.Contains(lock.Sets["proxy"], "curl_amd64") || !slices.Contains(lock.Sets["proxy"], "curl_arm64") {
		return errors.New("proxy lost curl")
	}
	for _, id := range lock.Sets["certificates"] {
		if !slices.Contains(lock.Sets["proxy"], id) {
			return errors.New("proxy lost certificate dependency")
		}
	}
	return nil
}

// A role whose binary carries a file capability (owner decision 597; sidecar DESIGN §19.4): the stage between the
// package stage and the final one where the role's static helper, bind-mounted from the build context so it never
// ships, writes the attribute, and the final stage's copy of the binary with it and the helper's check, each
// exactly as reviewed. Nothing after the check, and nothing else in the final stage, may name the binary or the
// helper: a copy over the binary, or a chown of it, drops the attribute.
type fileCapabilityRecipe struct {
	binary string
	helper string
	stage  []string
	final  []string
}

var fileCapabilityRecipes = map[string]fileCapabilityRecipe{
	"smtpedge": {
		binary: "/usr/local/sbin/bringyour-smtpedge",
		helper: "build/$TARGETPLATFORM/filecap",
		stage: []string{
			"FROM scratch AS file-capability",
			"ARG TARGETPLATFORM",
			"COPY build/$TARGETPLATFORM/smtpedge /usr/local/sbin/bringyour-smtpedge",
			`RUN --network=none --mount=type=bind,source=build/$TARGETPLATFORM/filecap,target=/filecap ["/filecap", "set", "/usr/local/sbin/bringyour-smtpedge"]`,
		},
		final: []string{
			"COPY --from=file-capability /usr/local/sbin/bringyour-smtpedge /usr/local/sbin/bringyour-smtpedge",
			`RUN --network=none --mount=type=bind,source=build/$TARGETPLATFORM/filecap,target=/filecap ["/filecap", "check", "/usr/local/sbin/bringyour-smtpedge"]`,
		},
	},
}

// Check the executable instructions rather than matching comments or whitespace.
// This intentionally covers the simple service recipes, not arbitrary Dockerfiles.
func checkRuntimePackageRecipe(lock runtimePackageLock, role, recipe string) error {
	wanted := map[string]bool{}
	for _, id := range lock.Sets[lock.Services[role]] {
		for _, pkg := range lock.Packages {
			if pkg.Id == id {
				wanted["ADD --checksum=sha256:"+pkg.Sha256+" "+lock.Snapshot+pkg.Filename+" /runtime-packages/"+pkg.Architecture+"/"+pkg.Name+".deb"] = true
			}
		}
	}
	stage, installs := 0, 0
	pending := ""
	capability, hasCapability := fileCapabilityRecipes[role]
	// how many of the capability stage's instructions, and of the final stage's, have come
	capabilityStageCount, capabilityFinalCount := 0, 0
	for _, line := range strings.Split(recipe, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			pending += strings.TrimSpace(strings.TrimSuffix(line, "\\")) + " "
			continue
		}
		instruction := pending + line
		pending = ""
		if hasCapability {
			if stage == 1 && capabilityStageCount == 0 && instruction == capability.stage[0] {
				if len(wanted) != 0 {
					return errors.New("unlocked or incomplete package base stage")
				}
				capabilityStageCount++
				continue
			}
			if 0 < capabilityStageCount && capabilityStageCount < len(capability.stage) {
				if instruction != capability.stage[capabilityStageCount] {
					return errors.New("file capability stage gained, lost or changed an instruction")
				}
				capabilityStageCount++
				continue
			}
			if stage == 2 {
				if capabilityFinalCount < len(capability.final) && instruction == capability.final[capabilityFinalCount] {
					capabilityFinalCount++
					continue
				}
				if (strings.Contains(instruction, capability.binary) && !strings.HasPrefix(instruction, "CMD ")) || strings.Contains(instruction, capability.helper) {
					return errors.New("final stage writes the binary without its file capability, or ships the helper")
				}
				verb, _, _ := strings.Cut(instruction, " ")
				if capabilityFinalCount == len(capability.final) && !slices.Contains([]string{"ARG", "ENV", "STOPSIGNAL", "CMD"}, verb) {
					return errors.New("final stage changes the image after the file capability check")
				}
			}
		}
		switch {
		case strings.HasPrefix(instruction, "FROM "):
			stage++
			want := "FROM " + lock.BaseImage
			if stage == 1 {
				want += " AS runtime-packages"
			}
			if stage > 2 || instruction != want || stage == 2 && len(wanted) != 0 {
				return errors.New("unlocked or incomplete package base stage")
			}
		case strings.HasPrefix(instruction, "ADD "):
			if stage != 1 || !wanted[instruction] {
				return errors.New("unlocked, changed or repeated remote package input")
			}
			delete(wanted, instruction)
		case strings.HasPrefix(instruction, "RUN "):
			if stage != 2 {
				return errors.New("package download stage gained execution")
			}
			if instruction == "RUN ln -s /srv/warp/vault/.aws /root/.aws" {
				continue
			}
			want := "RUN --network=none --mount=from=runtime-packages,source=/runtime-packages,target=/runtime-packages,ro dpkg -i /runtime-packages/\"$(dpkg --print-architecture)\"/*.deb /runtime-packages/all/*.deb && update-ca-certificates && rm -f /var/cache/ldconfig/aux-cache /var/log/dpkg.log"
			if instruction != want {
				return errors.New("package installation gained an unreviewed command, resolver or network")
			}
			installs++
		default:
			if stage != 2 {
				return errors.New("unreviewed package stage instruction")
			}
			verb, _, _ := strings.Cut(instruction, " ")
			if !slices.Contains([]string{"ARG", "ENV", "COPY", "STOPSIGNAL", "CMD"}, verb) || strings.Contains(instruction, "COPY --from=") {
				return errors.New("unreviewed image input or deferred execution")
			}
		}
	}
	if pending != "" || stage != 2 || installs != 1 || len(wanted) != 0 {
		return errors.New("incomplete package install")
	}
	if hasCapability && (capabilityStageCount != len(capability.stage) || capabilityFinalCount != len(capability.final)) {
		return errors.New("binary lost its file capability stage, copy or check")
	}
	return nil
}

// Every adjacent service consumes the same checked-in package authority.
func TestRuntimePackagesJoinAllServiceRecipes(t *testing.T) {
	cli, lock := loadRuntimePackageLock(t)
	paths, err := filepath.Glob(filepath.Join(cli, "*", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, filename := range paths {
		role := filepath.Base(filepath.Dir(filename))
		raw, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if role == "competitionworker" && strings.HasPrefix(string(raw), "FROM scratch\n") {
			continue
		}
		if _, exists := lock.Services[role]; !exists {
			t.Fatalf("unreviewed service Dockerfile %s", role)
		}
		if err := checkRuntimePackageRecipe(lock, role, string(raw)); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		seen[role] = true
	}
	if len(seen) != len(lock.Services) {
		t.Fatal("missing service recipe")
	}
}

// The original moving-repository recipe and adjacent input/boundary mutations
// must fail even when the base image remains pinned.
func TestRuntimePackagesRejectMovingAndIncompleteInputs(t *testing.T) {
	cli, lock := loadRuntimePackageLock(t)
	raw, err := os.ReadFile(filepath.Join(cli, "proxy", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	good := string(raw)
	if err := checkRuntimePackageRecipe(lock, "proxy", good); err != nil {
		t.Fatal(err)
	}
	start := strings.Index(good, "ADD ")
	end := start + strings.IndexByte(good[start:], '\n') + 1
	for _, fault := range []string{"moving-repository", "missing-payload", "wrong-hash", "override", "online-install", "tag-only", "duplicate-payload", "deferred-resolver"} {
		changed := good
		switch fault {
		case "moving-repository":
			changed = strings.Replace(changed, "RUN --network=none --mount=from=runtime-packages,source=/runtime-packages,target=/runtime-packages,ro \\", "RUN apt-get update && apt-get install -y curl && \\", 1)
		case "missing-payload":
			changed = changed[:start] + changed[end:]
		case "wrong-hash":
			changed = strings.Replace(changed, "--checksum=sha256:", "--checksum=sha256:0", 1)
		case "override":
			changed = strings.Replace(changed, lock.Snapshot, "${PACKAGE_MIRROR}/", 1)
		case "online-install":
			changed = strings.Replace(changed, "--network=none", "--network=default", 1)
		case "tag-only":
			changed = strings.ReplaceAll(changed, lock.BaseImage, "ubuntu:24.04")
		case "duplicate-payload":
			changed = changed[:end] + changed[start:end] + changed[end:]
		case "deferred-resolver":
			changed += "\nONBUILD RUN apt-get update && apt-get install -y curl\n"
		}
		if err := checkRuntimePackageRecipe(lock, "proxy", changed); err == nil {
			t.Fatalf("%s escaped package admission", fault)
		}
	}
}

// Architecture and role omissions are rejected independently of recipe text.
func TestRuntimePackagesRejectCrossTargetAndCurlGaps(t *testing.T) {
	_, lock := loadRuntimePackageLock(t)
	for _, fault := range []string{"arm64-pair", "curl", "duplicate", "index-architecture"} {
		raw, _ := json.Marshal(lock)
		var changed runtimePackageLock
		if err := json.Unmarshal(raw, &changed); err != nil {
			t.Fatal(err)
		}
		switch fault {
		case "arm64-pair":
			changed.Sets["certificates"] = slices.DeleteFunc(changed.Sets["certificates"], func(id string) bool { return id == "openssl_arm64" })
		case "curl":
			changed.Sets["proxy"] = slices.DeleteFunc(changed.Sets["proxy"], func(id string) bool { return strings.HasPrefix(id, "curl_") })
		case "duplicate":
			changed.Packages = append(changed.Packages, changed.Packages[0])
		case "index-architecture":
			for i, pkg := range changed.Packages {
				if pkg.Architecture == "arm64" {
					changed.Packages[i].IndexIds = []string{"noble-updates/amd64"}
					break
				}
			}
		}
		if err := checkRuntimePackageLock(changed); err == nil {
			t.Fatalf("%s lost package coverage", fault)
		}
	}
}

// smtpedge's binary keeps its file capability stage, copy and check exactly (owner decision 597): dropping or
// changing any of them, copying the binary in from the context, shipping the helper, or changing the image after
// the check is refused, and no other role may take the stage.
func TestRuntimePackagesHoldSmtpEdgeFileCapability(t *testing.T) {
	cli, lock := loadRuntimePackageLock(t)
	raw, err := os.ReadFile(filepath.Join(cli, "smtpedge", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	good := string(raw)
	if err := checkRuntimePackageRecipe(lock, "smtpedge", good); err != nil {
		t.Fatal(err)
	}
	helperRun := func(mode string) string {
		return `RUN --network=none --mount=type=bind,source=build/$TARGETPLATFORM/filecap,target=/filecap \` + "\n" +
			`    ["/filecap", "` + mode + `", "/usr/local/sbin/bringyour-smtpedge"]` + "\n"
	}
	stage := "FROM scratch AS file-capability\nARG TARGETPLATFORM\nCOPY build/$TARGETPLATFORM/smtpedge /usr/local/sbin/bringyour-smtpedge\n" + helperRun("set")
	copyOn := "COPY --from=file-capability /usr/local/sbin/bringyour-smtpedge /usr/local/sbin/bringyour-smtpedge\n"
	for _, part := range []string{stage, copyOn + helperRun("check")} {
		if strings.Count(good, part) != 1 {
			t.Fatalf("the recipe does not hold %q once", part)
		}
	}
	beforeStop := func(instruction string) string {
		return strings.Replace(good, "STOPSIGNAL SIGTERM", instruction+"\nSTOPSIGNAL SIGTERM", 1)
	}
	for _, fault := range []string{"no-stage", "unset", "networked-set", "stage-on-base", "stage-gained", "plain-copy", "unchecked", "check-before-copy",
		"copy-after-check", "chown-after-check", "config-after-check", "shipped-helper", "second-copy"} {
		changed := good
		switch fault {
		case "no-stage":
			changed = strings.Replace(changed, stage, "", 1)
		case "unset":
			changed = strings.Replace(changed, helperRun("set"), "", 1)
		case "networked-set":
			changed = strings.Replace(changed, helperRun("set"), strings.Replace(helperRun("set"), "--network=none ", "", 1), 1)
		case "stage-on-base":
			changed = strings.Replace(changed, "FROM scratch AS file-capability", "FROM "+lock.BaseImage+" AS file-capability", 1)
		case "stage-gained":
			changed = strings.Replace(changed, stage, stage+"RUN [\"/filecap\", \"set\", \"/etc/passwd\"]\n", 1)
		case "plain-copy":
			changed = strings.Replace(changed, copyOn, "COPY build/$TARGETPLATFORM/smtpedge /usr/local/sbin/bringyour-smtpedge\n", 1)
		case "unchecked":
			changed = strings.Replace(changed, copyOn+helperRun("check"), copyOn, 1)
		case "check-before-copy":
			changed = strings.Replace(changed, copyOn+helperRun("check"), helperRun("check")+copyOn, 1)
		case "copy-after-check":
			changed = beforeStop("COPY build/$TARGETPLATFORM/smtpedge /usr/local/sbin/bringyour-smtpedge")
		case "chown-after-check":
			changed = beforeStop("RUN chown 65532 /usr/local/sbin/bringyour-smtpedge")
		case "config-after-check":
			changed = beforeStop("COPY smtpedge.yml /etc/smtpedge.yml")
		case "shipped-helper":
			changed = strings.Replace(changed, copyOn, "COPY build/$TARGETPLATFORM/filecap /usr/local/sbin/filecap\n"+copyOn, 1)
		case "second-copy":
			changed = strings.Replace(changed, copyOn, copyOn+copyOn, 1)
		}
		if changed == good || checkRuntimePackageRecipe(lock, "smtpedge", changed) == nil {
			t.Fatalf("%s escaped file capability admission", fault)
		}
	}
	// a role with no reviewed capability stage may not take one
	apiRaw, err := os.ReadFile(filepath.Join(cli, "api", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	api := string(apiRaw)
	if err := checkRuntimePackageRecipe(lock, "api", api); err != nil {
		t.Fatal(err)
	}
	baseAt := strings.Index(api, "\nFROM "+lock.BaseImage+"\n")
	if baseAt < 0 {
		t.Fatal("api's recipe has no final stage")
	}
	adopted := api[:baseAt+1] + stage + api[baseAt+1:]
	if checkRuntimePackageRecipe(lock, "api", adopted) == nil {
		t.Fatal("api took a file capability stage")
	}
}
