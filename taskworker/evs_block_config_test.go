package taskworker

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/evshost"
)

// evs's jobs take their instance format from the block's config (QD-854): format 2 with no file, format 1 under a
// rollback's, and no settings at all under a format this code does not know.
func TestEvsJobSettingsTakeTheBlockConfigsFormat(t *testing.T) {
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	evsSettings, err := LoadEvsJobSettings()
	if err != nil || evsSettings.InstanceFormat != sidecar.EvsRepoFormat2 {
		t.Fatalf("with no file: %v, %v", evsSettings, err)
	}
	for _, c := range []struct {
		file   string
		format uint32
	}{
		{file: "instance_format: 1\n", format: sidecar.EvsRepoFormat1},
		{file: "instance_format: 2\n", format: sidecar.EvsRepoFormat2},
		{file: "# nothing set\n", format: sidecar.EvsRepoFormat2},
	} {
		evshost.Testing_WithBlockConfig(c.file, func() {
			evsSettings, err := LoadEvsJobSettings()
			if err != nil {
				t.Errorf("%q: %v", c.file, err)
				return
			}
			if evsSettings.InstanceFormat != c.format {
				t.Errorf("%q gives instance format %d, want %d", c.file, evsSettings.InstanceFormat, c.format)
			}
			// everything else is this code's default
			defaults := evsservice.DefaultEvsSettings()
			defaults.InstanceFormat = c.format
			if evsSettings.RepoCacheTtl != defaults.RepoCacheTtl || evsSettings.PriceBlockByteCount != defaults.PriceBlockByteCount {
				t.Errorf("%q changed more than the format", c.file)
			}
		})
	}
	for _, file := range []string{
		"instance_format: 0\n",
		"instance_format: 3\n",
		"instance_format: one\n",
		"instance-format: 1\n",
		"instance_format: [1\n",
	} {
		evshost.Testing_WithBlockConfig(file, func() {
			evsSettings, err := LoadEvsJobSettings()
			if err == nil {
				t.Errorf("%q gave settings at instance format %d", file, evsSettings.InstanceFormat)
				return
			}
			if !errors.Is(err, evshost.ErrBlockConfig) {
				t.Errorf("%q: %v is no ErrBlockConfig", file, err)
			}
		})
	}
}

// A taskworker whose block config names a format this code does not know builds no evs host and reads no store for
// one, so every evs job waits rather than run under a format its operator did not deploy.
func TestEvsJobsWaitUnderAnUnknownInstanceFormat(t *testing.T) {
	if evsJobHost.Load() != nil {
		t.Fatal("a host held before the test")
	}
	home := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", filepath.Join(home, "vault"))
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(home, "config"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var lifetime sync.WaitGroup
	evshost.Testing_WithBlockConfig("instance_format: 3\n", func() {
		startEvsJobsFromVault(ctx, &lifetime)
	})
	if evsJobHost.Load() != nil || evsJobHostSettingsCurrent.Load() != nil {
		t.Fatal("an evs host under an unknown instance format")
	}
	cancel()
	lifetime.Wait()
}

// Each run takes the engine settings read for the host it is on, bound to its context: a rollback's runtime runs its
// jobs at format 1, a job format 2 added waits there, and a run on a host no settings were read for waits too.
func TestEvsJobsRunAtTheBlockConfigsFormat(t *testing.T) {
	if evsJobHost.Load() != nil || evsJobHostSettingsCurrent.Load() != nil {
		t.Fatal("a host held before the test")
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	tasks := sidecartest.NewFakeTasks(clock, sidecartest.NewFaultQueue(), nil)
	var stateLock sync.Mutex
	runFormats := map[string][]uint32{}
	record := func(name string) func(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
		return func(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
			stateLock.Lock()
			defer stateLock.Unlock()
			runFormats[name] = append(runFormats[name], evsJobSettings(ctx).InstanceFormat)
			return nil
		}
	}
	formats := func(name string) []uint32 {
		stateLock.Lock()
		defer stateLock.Unlock()
		return append([]uint32{}, runFormats[name]...)
	}
	jobs := []*evsJob{
		{name: "evs.test_format1", run: record("evs.test_format1")},
		{name: "evs.test_format2", run: record("evs.test_format2"), format2: true},
	}
	registerEvsJobs(tasks, jobs, evsJobHost.Load)
	schedule := func(key string) {
		t.Helper()
		for _, job := range jobs {
			// a once key is the queue's, not the job's
			if err := tasks.Schedule(t.Context(), &sidecar.TaskRequest{Name: job.name, OnceKey: job.name + ":" + key}); err != nil {
				t.Fatal(err)
			}
		}
	}
	runDue := func() {
		t.Helper()
		clock.Advance(sidecartest.DefaultFakeTasksSettings().RetryTimeout)
		if _, err := tasks.RunDue(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	pending := func(name string) int {
		count := 0
		for _, request := range tasks.Pending() {
			if request.Name == name {
				count += 1
			}
		}
		return count
	}

	// a rollback's runtime: format 1's job runs at format 1, and format 2's waits
	rollbackSettings := evsservice.DefaultEvsSettings()
	rollbackSettings.InstanceFormat = sidecar.EvsRepoFormat1
	rollbackHost := &sidecar.ServiceHost{Tasks: tasks, Clock: clock}
	rollbackCtx, rollbackCancel := context.WithCancel(t.Context())
	var rollbackLifetime sync.WaitGroup
	startEvsJobsOnHost(rollbackCtx, &rollbackLifetime, rollbackHost, rollbackSettings, jobs, func() {})
	schedule("rollback")
	runDue()
	if got := formats("evs.test_format1"); len(got) != 1 || got[0] != sidecar.EvsRepoFormat1 {
		t.Fatalf("format 1's job in a rollback's runtime ran at %v", got)
	}
	if got := formats("evs.test_format2"); len(got) != 0 || pending("evs.test_format2") != 1 {
		t.Fatalf("format 2's job in a rollback's runtime ran at %v, %d waiting", got, pending("evs.test_format2"))
	}
	// and it keeps waiting, however often it is retried
	runDue()
	if got := formats("evs.test_format2"); len(got) != 0 || pending("evs.test_format2") != 1 {
		t.Fatalf("format 2's job retried in a rollback's runtime ran at %v, %d waiting", got, pending("evs.test_format2"))
	}

	// a host no settings were read for, as a runtime's that ended leaves a run that still holds it: both wait
	strayHost := &sidecar.ServiceHost{Tasks: tasks, Clock: clock}
	evsJobHost.Store(strayHost)
	schedule("stray")
	runDue()
	if got := formats("evs.test_format1"); len(got) != 1 {
		t.Fatalf("a job ran on a host no settings were read for: %v", got)
	}
	if got := formats("evs.test_format2"); len(got) != 0 {
		t.Fatalf("format 2's job ran on a host no settings were read for: %v", got)
	}
	if _, err := evsJobSettingsOfHost(strayHost); !errors.Is(err, errNoEvsJobSettings) {
		t.Fatalf("settings for a host none were read for: %v", err)
	}
	evsJobHost.Store(rollbackHost)
	rollbackCancel()
	rollbackLifetime.Wait()
	if evsJobHost.Load() != nil || evsJobHostSettingsCurrent.Load() != nil {
		t.Fatal("the rollback's runtime left its host or its settings")
	}

	// rolled forward: the next runtime runs both at format 2, the waiting runs among them
	forwardHost := &sidecar.ServiceHost{Tasks: tasks, Clock: clock}
	forwardCtx, forwardCancel := context.WithCancel(t.Context())
	var forwardLifetime sync.WaitGroup
	startEvsJobsOnHost(forwardCtx, &forwardLifetime, forwardHost, nil, jobs, func() {})
	runDue()
	if got := formats("evs.test_format1"); len(got) != 2 || got[1] != sidecar.EvsRepoFormat2 {
		t.Fatalf("format 1's job rolled forward ran at %v", got)
	}
	if got := formats("evs.test_format2"); len(got) != 2 || got[0] != sidecar.EvsRepoFormat2 || got[1] != sidecar.EvsRepoFormat2 {
		t.Fatalf("format 2's job rolled forward ran at %v", got)
	}
	if pending("evs.test_format1") != 0 || pending("evs.test_format2") != 0 {
		t.Fatalf("runs left waiting: %v", tasks.Pending())
	}
	forwardCancel()
	forwardLifetime.Wait()
}

// The jobs format 2 added are the drop job and the one-off sweeps, which delete what format 2's rules name; every
// other job is one a format-1 binary holds.
func TestEvsJobsOfFormat2AreTheDropJobAndTheSweeps(t *testing.T) {
	format2Names := map[string]bool{}
	for _, job := range evsJobs {
		if job.format2 {
			format2Names[job.name] = true
		}
	}
	if len(format2Names) != 2 || !format2Names[evsservice.EvsDropTaskName] || !format2Names[evsservice.EvsSweepTaskName] {
		t.Fatalf("format 2's jobs are %v", format2Names)
	}
}

// A job's function called outside the registry finds no settings bound and runs this code's defaults; one the
// registry bound takes exactly those.
func TestEvsJobSettingsOfARun(t *testing.T) {
	if format := evsJobSettings(t.Context()).InstanceFormat; format != evsservice.DefaultEvsSettings().InstanceFormat {
		t.Fatalf("an unbound run takes instance format %d", format)
	}
	bound := evsservice.DefaultEvsSettings()
	bound.InstanceFormat = sidecar.EvsRepoFormat1
	if evsJobSettings(withEvsJobSettings(t.Context(), bound)) != bound {
		t.Fatal("a bound run does not take its settings")
	}
}

// Every evs job that builds a part of the engine builds it under the settings bound to its run, the block config's:
// a run bound to settings no part takes, a format this code does not know and a block size with no price table,
// fails as that part's refusal, where the same run on the same host otherwise goes on to its stores.
func TestEvsJobsBuildTheirPartsUnderTheRunsSettings(t *testing.T) {
	names := map[string]*evsJob{}
	for _, job := range evsJobs {
		names[job.name] = job
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	sidecartest.RegisterEvsTwins(fake.Redis)
	host := fake.ServiceHost()
	clusterId := host.Blobs.MetadataClusterIds()[0]
	repoId, refId, deletionId := sidecar.Id{1}, sidecar.Id{2}, sidecar.Id{3}
	refused := evsservice.DefaultEvsSettings()
	refused.InstanceFormat = 3
	refused.PriceBlockByteCount = 7
	ctx := withEvsJobSettings(t.Context(), refused)
	for _, c := range []struct {
		name string
		args []byte
	}{
		{name: evsservice.EvsEvacuateTaskName},
		{name: evsservice.EvsEvacuateTaskName, args: []byte(clusterId)},
		{name: evsservice.EvsNightlyTaskName},
		{name: evsservice.EvsCollectTaskName, args: evsservice.EvsCollectArgs(clusterId, repoId, refId)},
		{name: evsservice.EvsSweepTaskName, args: evsservice.EvsSweepTurnOnTaskRequest().Args},
		{name: evsservice.EvsDropTaskName, args: evsDropRepoRequest(clusterId, repoId, clock.Now()).Args},
		{name: evsservice.EvsEpochCopyTaskName, args: evsservice.EvsEpochCopyArgs(repoId, refId, 2)},
		{name: evsservice.EvsShredTaskName, args: evsservice.EvsShredArgs(repoId, deletionId, false)},
		{name: evsservice.EvsReconcileTaskName},
	} {
		job := names[c.name]
		if job == nil {
			t.Errorf("no %s job", c.name)
			continue
		}
		err := job.run(ctx, host, c.args)
		if !errors.Is(err, evsservice.ErrEvsSettings) && !errors.Is(err, evsservice.ErrPriceTableBlockSize) {
			t.Errorf("%s with args %q under settings no part takes: %v", c.name, c.args, err)
		}
		// and the refusal is the bound settings', not the run's own
		if err := job.run(t.Context(), host, c.args); errors.Is(err, evsservice.ErrEvsSettings) || errors.Is(err, evsservice.ErrPriceTableBlockSize) {
			t.Errorf("%s with args %q under this code's defaults: %v", c.name, c.args, err)
		}
	}
}
