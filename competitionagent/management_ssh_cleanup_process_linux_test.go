//go:build linux

// Exact serve/registry/typed-manager paths retain real service and monitor
// subprocess joins. Only PID1 issuance/readback is a private synthetic seam.
package competitionagent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Cancellation cannot count a synthetic stop boolean as physical closure.
// The real service's TERM/Wait precedes stopack; the real monitor's original
// Wait and native-empty checks precede reset and exact metadata retirement.
func TestManagementSshCanceledOwnerPhysicallyJoinsServiceBeforeRetirement(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	settings.Agent.CancelGrace = 100 * time.Millisecond
	settings.JoinTimeout = 2 * time.Second
	host := managementSshTestHost(config, policy, settings)
	inRead, _, _, outWrite, _, diagWrite := managementSshTestPipes(t)
	watchdog, finish := context.WithTimeout(t.Context(), 10*time.Second)
	defer finish()
	request, cancel := context.WithCancel(watchdog)
	defer cancel()
	serviceJoined, monitorJoined, collected := &atomic.Bool{}, &atomic.Bool{}, &atomic.Bool{}
	ready := make(chan struct{})
	serviceDone, monitorDone := make(chan error, 1), make(chan error, 1)
	serviceExited, monitorExited := make(chan struct{}), make(chan struct{})
	var service, monitor *exec.Cmd
	var stopWrite *os.File
	var plan managementSshPlan
	started, stopped, observed := false, false, false
	host.start = func(_ context.Context, _ *Config, current managementSshPlan, _ string, files []*os.File) (managementSshProcess, error) {
		plan = current
		launch := func(mode string, stop *os.File) (*exec.Cmd, error) {
			read, write, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			defer read.Close()
			defer write.Close()
			command := exec.CommandContext(watchdog, os.Args[0], "-test.run=^TestManagementSshCleanupProcess$", "--", mode)
			command.Stdin, command.Stdout, command.Stderr = files[0], files[1], files[2]
			command.ExtraFiles = []*os.File{write}
			if stop != nil {
				command.ExtraFiles = append(command.ExtraFiles, stop)
			}
			if err := command.Start(); err != nil {
				return nil, err
			}
			write.Close()
			var acknowledged [1]byte
			if _, err := io.ReadFull(read, acknowledged[:]); err != nil || acknowledged[0] != 1 {
				_ = command.Process.Kill()
				_ = command.Wait()
				return nil, errors.New("synthetic native process did not acknowledge readiness")
			}
			return command, nil
		}
		var err error
		service, err = launch("service", nil)
		if err != nil {
			return nil, err
		}
		go func() { err := service.Wait(); serviceJoined.Store(true); serviceDone <- err; close(serviceExited) }()
		stopRead, write, err := os.Pipe()
		if err != nil {
			_ = service.Process.Kill()
			<-serviceExited
			return nil, err
		}
		defer stopRead.Close()
		stopWrite = write
		monitor, err = launch("monitor", stopRead)
		if err != nil {
			_ = service.Process.Kill()
			<-serviceExited
			_ = stopWrite.Close()
			return nil, err
		}
		go func() { err := monitor.Wait(); monitorJoined.Store(true); monitorDone <- err; close(monitorExited) }()
		started = true
		return &managementSshTestProcess{wait: func() error { return <-monitorDone }, kill: func() error { return monitor.Process.Kill() }}, nil
	}
	host.observe = func(_ context.Context, _ *Config, _ *ManagementResourcePolicy, _ Pin, current managementSshPlan) (managementSshUnit, error) {
		values := managementSshTestProjection(config, policy, current, settings)
		if !started || collected.Load() {
			managementSshRetirementMissing(values)
		} else {
			values["MainPID"] = strconv.Itoa(service.Process.Pid)
			values["ExecStart"] = strings.ReplaceAll(values["ExecStart"], "pid=17", "pid="+values["MainPID"])
			if stopped {
				values["ActiveState"], values["SubState"], values["MainPID"], values["ControlGroup"] = "inactive", "dead", "0", ""
			}
		}
		unit, err := validateManagementSshUnit(managementSshTestProjectionBytes(values), config, &policy, current, settings)
		if started && !observed && err == nil {
			observed = true
			close(ready)
		}
		return unit, err
	}
	host.stop = func(ctx context.Context, _ Pin, unit string) error {
		if unit != plan.UnitName {
			return errors.New("synthetic stop selected another service")
		}
		marker := filepath.Join(config.StateDirectory, "managed-controls", plan.Id+".admit")
		if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
			return errors.New("actual service stop retained admission")
		}
		if err := service.Process.Signal(syscall.SIGTERM); err != nil {
			return err
		}
		select {
		case err := <-serviceDone:
			if err != nil || !serviceJoined.Load() || service.ProcessState == nil || !service.ProcessState.Exited() {
				return errors.New("actual service did not positively join before stop acknowledgement")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		if _, err := stopWrite.Write([]byte{1}); err != nil {
			return err
		}
		stopped = true
		return nil
	}
	host.empty = func(context.Context, string) (bool, error) {
		return stopped && serviceJoined.Load() && service.ProcessState != nil && service.ProcessState.Exited(), nil
	}
	host.resetFailed = func(context.Context, Pin, string) error {
		if !serviceJoined.Load() || !monitorJoined.Load() || monitor.ProcessState == nil || !monitor.ProcessState.Exited() {
			return errors.New("manager collection preceded actual service or monitor Wait")
		}
		collected.Store(true)
		return nil
	}
	done := make(chan error, 1)
	ownerExited := make(chan struct{})
	go func() {
		defer close(ownerExited)
		done <- host.serve(request, config.configPath, inRead, outWrite, diagWrite)
	}()
	t.Cleanup(func() {
		cancel()
		finish()
		<-ownerExited
		if stopWrite != nil {
			_ = stopWrite.Close()
		}
		if service != nil {
			<-serviceExited
		}
		if monitor != nil {
			<-monitorExited
		}
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("native service fixture ended before observed admission: %v", err)
	case <-watchdog.Done():
		t.Fatal("native service fixture did not establish readiness")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || errors.Is(err, ErrManagedSshUnknown) {
		t.Fatal("canceled owner lost positively joined native custody", err)
	}
	if !serviceJoined.Load() || !monitorJoined.Load() || !collected.Load() {
		t.Fatal("canceled owner returned before actual native service/monitor collection")
	}
	for _, suffix := range []string{"json", "admit", "admitted", "closed"} {
		if _, err := os.Lstat(filepath.Join(config.StateDirectory, "managed-controls", plan.Id+"."+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("positively collected canceled control retained authority metadata")
		}
	}
}

// Each helper owns physical stdio. The service responds only to scoped TERM;
// the separate monitor cannot exit until stop's service Wait has completed.
func TestManagementSshCleanupProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode != "service" && mode != "monitor" {
		os.Exit(92)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	ready := os.NewFile(3, "synthetic-process-ready")
	if _, err := ready.Write([]byte{1}); err != nil {
		os.Exit(93)
	}
	ready.Close()
	if mode == "service" {
		<-ctx.Done()
	} else {
		stop := os.NewFile(4, "synthetic-exact-stop")
		var value [1]byte
		if _, err := io.ReadFull(stop, value[:]); err != nil || value[0] != 1 {
			os.Exit(94)
		}
		stop.Close()
	}
	os.Exit(0)
}
