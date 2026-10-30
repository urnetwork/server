// Synthetic controls use native construction/accounting, without credentials,
// live storage, competition policy or database access.
package main

import (
	"context"
	_ "embed"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Embed the actual helper for a boundary check independent of test working dir.
//
//go:embed capacity.go
var capacitySource string

// Direct native construction does not need a valid competition policy. The
// caller-selected bucket changes only this owner; local fallback is refused.
func TestCapacityDirectNativeConfiguration(t *testing.T) {
	config := &server.BlobStoreConfig{Authority: "storage.example:9000", Bucket: "synthetic-original", Prefix: "synthetic/"}
	store, err := newCapacityStore(config, true, "synthetic-selected")
	if err != nil || store.Bucket() != "synthetic-selected" || store.Prefix() != config.Prefix || store.Authority() != config.Authority || config.Bucket != "synthetic-original" {
		t.Fatal("direct native construction or configuration ownership failed")
	}
	for _, invalid := range []*server.BlobStoreConfig{nil, {}, {Local: true, Authority: "storage.example:9000"}, {Authority: "local"}, {Authority: " "}} {
		if _, err := newCapacityStore(invalid, true, "synthetic-selected"); err == nil {
			t.Fatal("missing or local configuration was accepted")
		}
	}
	if _, err := newCapacityStore(config, false, "synthetic-selected"); err == nil {
		t.Fatal("absent configuration was accepted")
	}
}

// Only the explicitly supplied read methods exist; any accidental inherited
// write would panic. No goroutines, timers, service or external endpoint exists.
type syntheticCapacityStore struct {
	server.RetainedBlobStore
	Objects       []server.BlobObjectVersion
	RetentionErr  error
	ListErr       error
	RetentionRead bool
	ListRead      bool
}

// Preserve separate retained-configuration and version-listing order.
func (self *syntheticCapacityStore) CheckRetention(ctx context.Context) error {
	self.RetentionRead = true
	if err := ctx.Err(); err != nil {
		return err
	}
	return self.RetentionErr
}

// Exercise the actual native accumulator and its partial-list refusal.
func (self *syntheticCapacityStore) ListVersions(ctx context.Context, prefix string, visit func(server.BlobObjectVersion) error) error {
	if !self.RetentionRead || prefix != "synthetic/namespace/" {
		return errors.New("synthetic read order or scope mismatch")
	}
	self.ListRead = true
	for _, object := range self.Objects {
		if err := visit(object); err != nil {
			return err
		}
	}
	return self.ListErr
}

// Synthetic backing identity is never a live route.
func (self *syntheticCapacityStore) Authority() string { return "storage.example:9000" }

// Synthetic bucket is independent of the production selector.
func (self *syntheticCapacityStore) Bucket() string { return "synthetic-bucket" }

// Full object keys use this synthetic namespace root.
func (self *syntheticCapacityStore) Prefix() string { return "synthetic/" }

// Current and noncurrent body bytes both count; a delete marker counts as an
// object but cannot hide the retained older body.
func TestCapacityUsesEveryNativeVersion(t *testing.T) {
	store := &syntheticCapacityStore{Objects: []server.BlobObjectVersion{
		{Key: "synthetic/namespace/object", Size: 12, VersionId: "synthetic-new"},
		{Key: "synthetic/namespace/object", Size: 7, VersionId: "synthetic-old"},
		{Key: "synthetic/namespace/object", DeleteMarker: true, VersionId: "synthetic-marker"},
	}}
	usage, err := readCapacity(context.Background(), store, "synthetic/namespace/", 23)
	if err != nil || usage == nil || usage.UsedBytes != 19 || usage.FreeBytes != 4 || usage.ObjectCount != 3 || !usage.IncludesObjectVersions || !store.RetentionRead || !store.ListRead {
		t.Fatal("native version accounting did not retain complete history")
	}
}

// Authentication/read errors, partial discovery, invalid metadata, allocation
// exhaustion and explicit cancellation never produce a success receipt.
func TestCapacityRefusesIncompleteReadProof(t *testing.T) {
	for _, store := range []*syntheticCapacityStore{
		{RetentionErr: errors.New("synthetic-auth-refusal")},
		{ListErr: errors.New("synthetic-partial-list"), Objects: []server.BlobObjectVersion{{Key: "synthetic/namespace/object", Size: 2}}},
		{Objects: []server.BlobObjectVersion{{Key: "synthetic/namespace/object", Size: 24}}},
		{Objects: []server.BlobObjectVersion{{Key: "synthetic/namespace/object", Size: 1, DeleteMarker: true}}},
		{Objects: []server.BlobObjectVersion{{Key: "synthetic/outside/object", Size: 1}}},
	} {
		usage, err := readCapacity(context.Background(), store, "synthetic/namespace/", 23)
		if err == nil || usage != nil || strings.Contains(err.Error(), "synthetic-") {
			t.Fatal("partial, invalid or sensitive backend error qualified")
		}
		if store.RetentionErr != nil && store.ListRead {
			t.Fatal("listing followed failed retention authentication")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if usage, err := readCapacity(ctx, &syntheticCapacityStore{}, "synthetic/namespace/", 23); err == nil || usage != nil {
		t.Fatal("canceled read qualified")
	}
}

// The pre-fix controller import/LoadSettings necessarily fails this actual
// source check. Native server calls are restricted to configuration/client
// construction and version accounting; blob mutations and commands are absent.
func TestCapacityReadOnlyPolicyIndependentBoundary(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "capacity.go", capacitySource, 0)
	if err != nil {
		t.Fatal(err)
	}
	allowedImports := map[string]bool{"context": true, "crypto/sha256": true, "encoding/hex": true,
		"encoding/json": true, "errors": true, "fmt": true, "os": true, "strings": true,
		"syscall": true, "time": true, "github.com/urnetwork/server": true}
	for _, imported := range source.Imports {
		name, err := strconv.Unquote(imported.Path.Value)
		if err != nil || !allowedImports[name] || imported.Name != nil {
			t.Fatal("unexpected policy, network, command or renamed import")
		}
	}
	allowedServerCalls := map[string]bool{"LoadBlobStoreConfig": true, "NewBlobStore": true, "MeasureBlobVersionUsageAtPrefix": true}
	seenServerCalls := map[string]int{}
	ast.Inspect(source, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "server" {
			if !allowedServerCalls[selector.Sel.Name] {
				t.Errorf("unexpected native server call %s", selector.Sel.Name)
			}
			seenServerCalls[selector.Sel.Name]++
		}
		for _, forbidden := range []string{"Put", "PutIfAbsent", "PutRetained", "SetLifecycle", "MakeBucket", "RemoveObject", "SetBucketReplication", "Command", "CommandContext", "LoadSettings"} {
			if selector.Sel.Name == forbidden {
				t.Errorf("forbidden mutation or policy coupling %s", forbidden)
			}
		}
		return true
	})
	for name := range allowedServerCalls {
		if seenServerCalls[name] != 1 {
			t.Errorf("native call %s count=%d", name, seenServerCalls[name])
		}
	}
}
