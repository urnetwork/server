package monitor

// The storage signals' shared Mimir path, through a synthetic services host.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A services host that answers every command with answer, and keeps the commands.
type sidecarStorageTestSource struct {
	answer   string
	commands []string
}

func (self *sidecarStorageTestSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	return nil, errors.New("no postgres")
}

func (self *sidecarStorageTestSource) Redis(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
	return "", errors.New("no redis")
}

func (self *sidecarStorageTestSource) Host(ctx context.Context, host HostSettings, command string) (string, error) {
	self.commands = append(self.commands, command)
	return self.answer, nil
}

// One bounded curl to the loopback Mimir of a services host, its partitions read back with their labels.
func TestSidecarStorageMimirQueryReadsOneGateway(t *testing.T) {
	source := &sidecarStorageTestSource{answer: `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"monitor_check":"bucket","cluster":"meta-1"},"value":[1762480800,"42"]},` +
		`{"metric":{"monitor_check":"dwpd","cluster":"meta-1","window":"7d"},"value":[1762480800,"0.4"]}]}}`}
	settings := DefaultSidecarStorageSettings()
	env := &probeEnv{
		cfg:    &monitorConfig{env: "synthetic", sidecarStorage: settings, hosts: []*host{{name: "services-1", roles: []string{"services"}}}},
		runner: &sourceRunner{source: source},
		now:    func() time.Time { return fileClusterTestNow },
	}
	samples, err := sidecarStorageMimirQuery(t.Context(), env, `up{env="synthetic"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || samples[0].check != "bucket" || samples[0].label("cluster") != "meta-1" || samples[0].value != 42 || samples[0].label("monitor_check") != "" {
		t.Fatalf("samples %+v", samples)
	}
	if len(source.commands) != 1 {
		t.Fatalf("%d commands", len(source.commands))
	}
	for _, part := range []string{"--max-time 15", "--max-filesize 1048576", `query=up{env="synthetic"}`, "time=" + strconv.FormatInt(fileClusterTestNow.Unix(), 10), "http://127.0.0.1:3100/prometheus/api/v1/query"} {
		if !strings.Contains(source.commands[0], part) {
			t.Errorf("the command lacks %s: %s", part, source.commands[0])
		}
	}

	source.answer = `{"status":"error","error":"bad query"}`
	if _, err := sidecarStorageMimirQuery(t.Context(), env, "up"); err == nil {
		t.Error("an error answer read")
	}
	env.cfg.hosts = nil
	if _, err := sidecarStorageMimirQuery(t.Context(), env, "up"); err == nil {
		t.Error("no services host read")
	}
}
