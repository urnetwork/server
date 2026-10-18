// Contract operations resolve the optional provider-work source from the vault
// once per vault version, not once per operation. Production connect logged
// the resolver's probe lines for provider_work_session.json on every contract
// creation and close; these tests count those exact lines through the real
// creation entry points and the operation pin. Paths and keys are synthetic.
package model

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/urnetwork/server"
)

// Collects what this process writes to stderr while run executes. The
// descriptor is redirected rather than os.Stderr, so concurrent writers keep
// their file and the race detector sees no shared variable change. glog writes
// synchronously, so every line run produced is captured when it returns. The
// capture is replayed to the original stderr so test output keeps it. Under
// go test -json the testing package points os.Stderr at os.Stdout, so glog
// then writes through descriptor 1.
func captureStderr(t testing.TB, run func()) string {
	t.Helper()
	capture, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	descriptor := 2
	if os.Stderr == os.Stdout {
		descriptor = 1
	}
	original, err := unix.Dup(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Dup2(int(capture.Fd()), descriptor); err != nil {
		unix.Close(original)
		t.Fatal(err)
	}
	var captured []byte
	func() {
		defer func() {
			// restore first, so a failing run still reports to the real stderr
			if err := unix.Dup2(original, descriptor); err != nil {
				t.Errorf("restore stderr: %v", err)
			}
			unix.Close(original)
			captured, _ = os.ReadFile(capture.Name())
			os.Stderr.Write(captured)
		}()
		run()
	}()
	return string(captured)
}

// The probe line the resolver writes once per full resolution of a top-level
// resource under its literal home.
func providerWorkSessionResolverProbe(root string) string {
	return "[env]try " + root + ", [provider_work_session.json]"
}

// Every public alias and direct owner of contract creation pins the optional
// source before pg. With an unchanged vault, only the first creation may read
// the vault: the root and its parent are absent, so the resolution has no
// recent timestamp to settle and the current semantics are the missing file.
func TestProviderWorkSessionSourceResolvesOnceAcrossContractCreation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent", "vault")
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "synthetic-resolution")
	probe := providerWorkSessionResolverProbe(root)
	entries := providerWorkCreationSourceEntries()
	create := func(entry providerWorkCreationSourceEntry) {
		ctx := server.WithoutPostgres(t.Context())
		result := providerWorkCreationSourceAttempt(ctx, entry)
		raised, ok := result.(error)
		if !ok || !errors.Is(raised, server.ErrPacketPostgres) || server.PacketPostgresAttempts(ctx) != 1 {
			t.Fatalf("%s did not reach its pg boundary after pinning the source: result=%v attempts=%d", entry.name, result, server.PacketPostgresAttempts(ctx))
		}
	}

	// the cold resolution is the control that the probe is observable at all
	cold := strings.Count(captureStderr(t, func() { create(entries[0]) }), probe)
	if cold != 1 {
		t.Fatalf("the first creation wrote %d resolver probes for provider_work_session.json; want 1 (glog must log to stderr)", cold)
	}
	rounds := 4
	warm := strings.Count(captureStderr(t, func() {
		for round := 0; round < rounds; round += 1 {
			for _, entry := range entries {
				create(entry)
			}
		}
	}), probe)
	if warm != 0 {
		t.Fatalf("%d more contract creations resolved provider_work_session.json %d more times; want 0 while the vault is unchanged", rounds*len(entries), warm)
	}
}

// Contract close pins the source inside its settlement transaction. Contracts
// created with the fixture's pinned source never load it; their closes carry
// no source, so each final close resolves the vault exactly as production
// connect does, and only the first may read an unchanged vault.
func TestProviderWorkSessionSourceResolvesOnceAcrossContractClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		contractIds := []server.Id{}
		for i := 0; i < 4; i += 1 {
			contractIds = append(contractIds, f.contract(t))
		}
		// after the fixture's setup, so only the closes read this vault
		root := filepath.Join(t.TempDir(), "absent", "vault")
		t.Setenv("WARP_VAULT_HOME", root)
		probes := strings.Count(captureStderr(t, func() {
			for _, contractId := range contractIds {
				server.Raise(CloseContract(t.Context(), contractId, f.sourceId, 121, false))
				server.Raise(CloseContract(t.Context(), contractId, f.destinationId, 121, false))
			}
		}), providerWorkSessionResolverProbe(root))
		if probes != 1 {
			t.Fatalf("closing %d contracts resolved provider_work_session.json %d times; want 1 while the vault is unchanged", len(contractIds), probes)
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			var closed, outcomes int
			server.Raise(conn.QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NOT NULL),
 (SELECT count(*) FROM provider_work_outcome_original WHERE contract_id=ANY($1))`, contractIds).Scan(&closed, &outcomes))
			if closed != len(contractIds) || outcomes != 0 {
				t.Fatalf("closes without a source settled %d of %d contracts with %d outcome originals; want all settled, none signed", closed, len(contractIds), outcomes)
			}
		})
	})
}

// One operation's source, resolved and pinned exactly as contract code does.
func pinnedProviderWorkSessionSource(t testing.TB) *ProviderWorkSessionSource {
	return providerWorkSessionSourceFromContext(providerWorkSessionContext(t.Context()))
}

// The operation pin keeps the optional semantics across vault changes on disk:
// absent is no source, a source written later is used by the next operation,
// and removing it is absence again.
func TestProviderWorkSessionContextFollowsVaultChanges(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "synthetic-resolution")
	if source := pinnedProviderWorkSessionSource(t); source != nil {
		t.Fatal("an absent provider_work_session.json pinned a source")
	}

	config := newProviderWorkInspectionConfig()
	path := filepath.Join(root, "provider_work_session.json")
	if err := os.WriteFile(path, providerWorkInspectionBytes(t, config), 0o600); err != nil {
		t.Fatal(err)
	}
	if source := pinnedProviderWorkSessionSource(t); source == nil || !reflect.DeepEqual(source.authority, config.Authority) {
		t.Fatal("the next operation after the source was written did not pin it")
	}

	replacement := newProviderWorkInspectionConfig()
	replacement.Authority.Generation = "00000000-0000-0000-0000-000000000103"
	if err := os.WriteFile(path, providerWorkInspectionBytes(t, replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if source := pinnedProviderWorkSessionSource(t); source == nil || source.authority.Generation != replacement.Authority.Generation {
		t.Fatal("the next operation after the source was rewritten in place kept the old source")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if source := pinnedProviderWorkSessionSource(t); source != nil {
		t.Fatal("a removed provider_work_session.json stayed pinned")
	}
}

// While the vault is unchanged, each operation still owns an independent
// source: clearing or editing one cannot reach a later operation. An override
// over an absent root keeps the resolution unchanged between operations, and
// replacing or removing the override is seen by the next operation.
func TestProviderWorkSessionContextOwnsEachSource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent", "vault")
	t.Setenv("WARP_VAULT_HOME", root)
	t.Setenv("WARP_ENV", "synthetic-resolution")
	config := newProviderWorkInspectionConfig()
	pop := server.Vault.PushSimpleResource("provider_work_session.json", providerWorkInspectionBytes(t, config))
	defer pop()

	var first, second *ProviderWorkSessionSource
	probes := strings.Count(captureStderr(t, func() {
		first = pinnedProviderWorkSessionSource(t)
		second = pinnedProviderWorkSessionSource(t)
	}), providerWorkSessionResolverProbe(root))
	if first == nil || second == nil || !reflect.DeepEqual(first.authority, config.Authority) || !reflect.DeepEqual(second.authority, config.Authority) {
		t.Fatal("the pushed source was not pinned")
	}
	if probes != 1 {
		t.Fatalf("two operations wrote %d resolver probes for provider_work_session.json; want 1", probes)
	}
	if second == first || &second.key[0] == &first.key[0] || &second.authority.DirectoryPublicKeys[0] == &first.authority.DirectoryPublicKeys[0] {
		t.Fatal("operations share one source owner")
	}
	clear(first.key)
	first.authority.DirectoryPublicKeys[0][0] ^= 1
	third := pinnedProviderWorkSessionSource(t)
	if third == nil || !bytes.Equal(third.key, config.PrivateKey) || !reflect.DeepEqual(third.authority, config.Authority) {
		t.Fatal("changing one operation's source changed a later operation's source")
	}

	replacement := newProviderWorkInspectionConfig()
	replacement.Authority.Generation = "00000000-0000-0000-0000-000000000103"
	popReplacement := server.Vault.PushSimpleResource("provider_work_session.json", providerWorkInspectionBytes(t, replacement))
	if source := pinnedProviderWorkSessionSource(t); source == nil || source.authority.Generation != replacement.Authority.Generation {
		t.Fatal("the next operation after the override changed kept the old source")
	}
	popReplacement()
	pop()
	if source := pinnedProviderWorkSessionSource(t); source != nil {
		t.Fatal("a removed override stayed pinned")
	}
}
