//go:build !linux

package main

import "io"

func runCaptureHostInventory(output io.Writer, args []string) error { return invalid }
