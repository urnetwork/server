package model

import (
	"bytes"
	"maps"
	"slices"
	"testing"
)

// Pure: the difference a provide writes, against every shape of stored set.
func TestProvideKeyDifference(t *testing.T) {
	keyA, keyB, keyC := []byte("synthetic-key-a"), []byte("synthetic-key-b"), []byte("synthetic-key-c")
	cases := []struct {
		name        string
		stored      map[ProvideMode][]byte
		provided    map[ProvideMode][]byte
		wantRemoved []ProvideMode
		wantWritten map[ProvideMode][]byte
	}{
		{
			name:     "unchanged",
			stored:   map[ProvideMode][]byte{ProvideModePublic: keyA, ProvideModeStream: keyB},
			provided: map[ProvideMode][]byte{ProvideModePublic: bytes.Clone(keyA), ProvideModeStream: bytes.Clone(keyB)},
		},
		{
			name: "both empty",
		},
		{
			name:        "first provide",
			provided:    map[ProvideMode][]byte{ProvideModePublic: keyA},
			wantWritten: map[ProvideMode][]byte{ProvideModePublic: keyA},
		},
		{
			name:        "provides nothing",
			stored:      map[ProvideMode][]byte{ProvideModeStream: keyB, ProvideModePublic: keyA},
			provided:    map[ProvideMode][]byte{},
			wantRemoved: []ProvideMode{ProvideModePublic, ProvideModeStream},
		},
		{
			name:        "rotated secret",
			stored:      map[ProvideMode][]byte{ProvideModePublic: keyA},
			provided:    map[ProvideMode][]byte{ProvideModePublic: keyB},
			wantWritten: map[ProvideMode][]byte{ProvideModePublic: keyB},
		},
		{
			name:        "mixed",
			stored:      map[ProvideMode][]byte{ProvideModeNetwork: keyA, ProvideModePublic: keyB, ProvideModeStream: keyC},
			provided:    map[ProvideMode][]byte{ProvideModeNetwork: keyA, ProvideModePublic: keyC, ProvideModeFriendsAndFamily: keyB},
			wantRemoved: []ProvideMode{ProvideModeStream},
			wantWritten: map[ProvideMode][]byte{ProvideModePublic: keyC, ProvideModeFriendsAndFamily: keyB},
		},
	}
	for _, c := range cases {
		difference := newProvideKeyDifference(c.stored, c.provided)
		if !slices.Equal(difference.removedProvideModes, c.wantRemoved) {
			t.Errorf("%s: removed %v, want %v", c.name, difference.removedProvideModes, c.wantRemoved)
		}
		wantWritten := c.wantWritten
		if wantWritten == nil {
			wantWritten = map[ProvideMode][]byte{}
		}
		if !maps.EqualFunc(difference.writeSecretKeys, wantWritten, bytes.Equal) {
			t.Errorf("%s: written %v, want %v", c.name, difference.writeSecretKeys, wantWritten)
		}
		if empty := len(c.wantRemoved) == 0 && len(wantWritten) == 0; difference.empty() != empty {
			t.Errorf("%s: empty = %t, want %t", c.name, difference.empty(), empty)
		}
	}
}
