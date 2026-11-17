//go:build !linux

// No alternate operating system can interpret this Linux cgroup authority.
package main

import "fmt"

// Unsupported hosts do not offer a simulated maintenance operation.
func main() { panic(fmt.Errorf("private engine cache maintenance requires Linux cgroup v2")) }
