// The resource-bomb fixture deterministically exhausts either CPU or memory so
// the live evaluator cleanup boundary can be tested under hostile load.
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

var retainedBuffers [][]byte

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: resource-bomb cpu CPUS|memory")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "cpu":
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: resource-bomb cpu CPUS")
			os.Exit(2)
		}
		if err := runCPUBomb(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "memory":
		if len(os.Args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: resource-bomb memory")
			os.Exit(2)
		}
		debug.SetGCPercent(-1)
		fmt.Println("memory-bomb-ready")
		for {
			buffer := make([]byte, 8*1024*1024)
			for offset := 0; offset < len(buffer); offset += 4096 {
				buffer[offset] = byte(offset)
			}
			retainedBuffers = append(retainedBuffers, buffer)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", os.Args[1])
		os.Exit(2)
	}
}

// Parse the explicit Docker CPU list without accepting ambiguous ranges or
// duplicate worker targets.
func parseCPUs(value string) ([]int, error) {
	if value == "" {
		return nil, fmt.Errorf("CPU set is empty")
	}
	cpuSeen := map[int]bool{}
	cpus := make([]int, 0, strings.Count(value, ",")+1)
	for _, field := range strings.Split(value, ",") {
		cpu, err := strconv.Atoi(field)
		if err != nil || cpu < 0 {
			return nil, fmt.Errorf("invalid CPU %q", field)
		}
		if cpuSeen[cpu] {
			return nil, fmt.Errorf("duplicate CPU %d", cpu)
		}
		cpuSeen[cpu] = true
		cpus = append(cpus, cpu)
	}
	return cpus, nil
}

// Pin one locked worker to every allowed CPU and report readiness only after
// each worker has executed on its assigned core.
func runCPUBomb(value string) error {
	cpus, err := parseCPUs(value)
	if err != nil {
		return err
	}
	runtime.GOMAXPROCS(len(cpus))
	readyCPUs := make(chan int, len(cpus))
	errs := make(chan error, len(cpus))
	for _, cpu := range cpus {
		go func() {
			runtime.LockOSThread()
			if err := pinCurrentThreadToCPU(cpu); err != nil {
				errs <- err
				return
			}
			readyCPUs <- cpu
			value := uint64(cpu + 1)
			for {
				value = value*6364136223846793005 + 1442695040888963407
				if value == 0 {
					fmt.Fprintln(os.Stderr, value)
				}
			}
		}()
	}
	for range cpus {
		select {
		case <-readyCPUs:
		case err := <-errs:
			return err
		}
	}
	fmt.Println("cpu-bomb-ready")
	select {}
}
