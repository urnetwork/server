// The resource-bomb fixture deterministically exhausts either CPU or memory so
// the live evaluator cleanup boundary can be tested under hostile load.
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
)

var retainedBuffers [][]byte

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: resource-bomb cpu|memory")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "cpu":
		fmt.Println("cpu-bomb-ready")
		for index := 0; index < 4*runtime.NumCPU(); index++ {
			go func() {
				value := uint64(1)
				for {
					value = value*6364136223846793005 + 1442695040888963407
					if value == 0 {
						fmt.Fprintln(os.Stderr, value)
					}
				}
			}()
		}
		select {}
	case "memory":
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
