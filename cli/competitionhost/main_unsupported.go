//go:build !linux

// Host provisioning is available only on the supported evaluator platform.
package main

import (
	"fmt"
	"os"
)

func main() { fmt.Fprintln(os.Stderr, "competitionhost requires Linux"); os.Exit(1) }
