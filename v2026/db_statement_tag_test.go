package server

import (
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestPgStatementTagBoundedIdentityAndPrivacy(t *testing.T) {
	pattern := regexp.MustCompile(`^urn1/(api|connect|taskworker|proxy|alt|config-updater|mcp|web|app|gossip|grafana|other)/[0-9a-f]{16}/[0-9a-f]{24}$`)
	nonce := [12]byte{1, 2, 3}
	for _, service := range []string{"api", "connect", "taskworker", "config-updater", "", "private/account*/ SELECT 1"} {
		name := pgStatementTag(service, "synthetic-host", "synthetic-block", "synthetic-version", nonce)
		if len(name) > 61 || !pattern.MatchString(name) {
			t.Fatal("tag is outside the closed bounded grammar")
		}
		for _, private := range []string{"synthetic-host", "synthetic-block", "synthetic-version", "private", "account"} {
			if strings.Contains(name, private) {
				t.Fatal("unfiltered identity in statement tag")
			}
		}
	}
	base := pgStatementTag("api", "host", "block", "version", nonce)
	for _, changed := range []string{
		pgStatementTag("connect", "host", "block", "version", nonce),
		pgStatementTag("api", "host2", "block", "version", nonce),
		pgStatementTag("api", "host", "block2", "version", nonce),
		pgStatementTag("api", "host", "block", "version2", nonce),
		pgStatementTag("api", "host", "block", "version", [12]byte{2, 2, 3}),
	} {
		if base == changed {
			t.Fatal("changed process/deployment identity retained its tag")
		}
	}
}

func TestPgStatementTagPreservesFixedBodyAndConcurrentIdentity(t *testing.T) {
	const query = "SELECT value FROM synthetic WHERE id=$1 FOR UPDATE"
	want := "/*" + processPgStatementTag() + "*/ " + query
	const count = 32
	names := make(chan string, count)
	var workers sync.WaitGroup
	for range count {
		workers.Go(func() { names <- TaggedDatabaseStatement(query) })
	}
	workers.Wait()
	close(names)
	for name := range names {
		if name != want {
			t.Fatal("fixed statement changed between calls or goroutines")
		}
		end := strings.Index(name, "*/ ")
		if end < 0 || name[end+3:] != query {
			t.Fatal("fixed statement body changed")
		}
	}
}
