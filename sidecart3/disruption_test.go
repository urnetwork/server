package sidecart3

// What disruption.go's rows rest on, checked at merge with no store or process: the lb fixture still carries the
// directives warp writes, and RedisRestart finds a test database's connections in CLIENT LIST.

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// Where warp's sibling checkout is from this package's directory, which go test runs in.
const warpDirectory = "../../warp"

// Every directive the fixture copies from warp/warpctl/config.go is still what warp writes, and evs's streamable path
// is still evs.yml's: a change on warp's side fails here rather than leaving the fixture standing for an lb that no
// longer exists.
func TestLbFixtureKeepsWarpsDirectives(t *testing.T) {
	fixture, err := os.ReadFile(DefaultLbFixtureSettings().ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	warpConfig, err := os.ReadFile(warpDirectory + "/warpctl/config.go")
	if err != nil {
		t.Fatalf("warp's config generator: %v", err)
	}
	evsBlock, err := os.ReadFile(warpDirectory + "/services/sidecar-blocks/evs.yml")
	if err != nil {
		t.Fatalf("warp's evs block: %v", err)
	}
	fixtureLines := []string{}
	for _, line := range strings.Split(string(fixture), "\n") {
		fixtureLines = append(fixtureLines, strings.TrimSpace(line))
	}
	warpLines := map[string]bool{}
	for _, line := range strings.Split(string(warpConfig), "\n") {
		warpLines[strings.TrimSpace(line)] = true
	}
	for _, directive := range []string{
		"keepalive_timeout 1m;",
		"client_header_buffer_size 4k;",
		"client_body_buffer_size 64k;",
		"client_max_body_size 16m;",
		"proxy_buffering off;",
		"proxy_request_buffering on;",
		"client_header_timeout 15s;",
		"client_body_timeout 15s;",
		"proxy_http_version 1.1;",
		"proxy_socket_keepalive on;",
		"proxy_next_upstream error timeout http_503 non_idempotent;",
		"proxy_next_upstream_tries 2;",
		"proxy_connect_timeout 30s;",
		"proxy_read_timeout 30s;",
		"proxy_send_timeout 30s;",
		"send_timeout 30s;",
		"proxy_read_timeout 60s;",
		"proxy_send_timeout 60s;",
		"proxy_request_buffering off;",
		"proxy_set_header X-UR-Request-Buffering off;",
		`proxy_set_header X-UR-Request-Buffering "";`,
		"proxy_set_header Connection 'keep-alive';",
		`proxy_set_header X-Forwarded-For "";`,
		`proxy_set_header X-Forwarded-Source-Port "";`,
		"proxy_set_header Host $host;",
	} {
		if !warpLines[directive] {
			t.Errorf("warp no longer writes %q, which the fixture copies", directive)
		}
		if !slices.Contains(fixtureLines, directive) {
			t.Errorf("the fixture lacks warp's %q", directive)
		}
	}
	// large_client_header_buffers sits in warp's source with a trailing space
	if !strings.Contains(string(warpConfig), "large_client_header_buffers 8 8k;") || !slices.Contains(fixtureLines, "large_client_header_buffers 8 8k;") {
		t.Error("large_client_header_buffers differs between warp and the fixture")
	}
	const segmentPath = "/evs/v1/repos/[^/]+/segments/[^/]+"
	if !strings.Contains(string(evsBlock), "- "+segmentPath) {
		t.Errorf("evs.yml no longer streams %s", segmentPath)
	}
	if !slices.Contains(fixtureLines, `location ~ "^(?:(?:`+segmentPath+`))$" {`) {
		t.Errorf("the fixture's streamed location is not addStreamablePathsLocation's form of %s", segmentPath)
	}
	// services.DefaultKeepalive's pool
	for _, directive := range []string{"keepalive 1024;", "keepalive_requests 8192;", "keepalive_time 15m;", "keepalive_timeout 1m;"} {
		if !slices.Contains(fixtureLines, directive) {
			t.Errorf("the fixture's upstream lacks %q", directive)
		}
	}
}

// CLIENT LIST's lines on the asked database, and only theirs.
func TestRedisClientIdsOnDatabase(t *testing.T) {
	list := strings.Join([]string{
		"id=3 addr=127.0.0.1:50001 laddr=127.0.0.1:6379 fd=8 name= age=9 idle=0 flags=N db=4 sub=0 cmd=client|list",
		"id=7 addr=127.0.0.1:50002 laddr=127.0.0.1:6379 fd=9 name= age=9 idle=2 flags=N db=12 sub=0 cmd=get",
		"id=11 addr=127.0.0.1:50003 laddr=127.0.0.1:6379 fd=10 name= age=1 idle=1 flags=N db=4 sub=0 cmd=set",
		"",
	}, "\n")
	clientIds, err := redisClientIdsOnDatabase(list, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(clientIds, []int64{3, 11}) {
		t.Errorf("clients %v", clientIds)
	}
	if _, err := redisClientIdsOnDatabase("addr=127.0.0.1:50004 db=4\n", 4); err == nil {
		t.Error("a line with no id")
	}
}
