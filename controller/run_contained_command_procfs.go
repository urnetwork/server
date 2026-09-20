//go:build !darwin

// Retains the procfs process-group inspection used by Linux evaluator workers.
package controller

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Ignores zombies awaiting their external reaper; missing processes are ordinary races.
func containedProcessGroupRunning(processGroupId int) (bool, error) {
	if err := syscall.Kill(-processGroupId, 0); errors.Is(err, syscall.ESRCH) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return false, err
		}
		// The parenthesized command name may itself contain spaces or ')'.
		end := bytes.LastIndexByte(stat, ')')
		if end < 0 {
			return false, errors.New("invalid process status")
		}
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) < 3 {
			return false, errors.New("incomplete process status")
		}
		groupId, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, err
		}
		if groupId == processGroupId && fields[0] != "Z" && fields[0] != "X" {
			return true, nil
		}
	}
	return false, nil
}
