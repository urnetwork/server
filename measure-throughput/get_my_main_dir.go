package main

import (
	"fmt"
	"path/filepath"
	"runtime"
)

func getMyMainDir() (string, error) {

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot get current file")
	}

	return filepath.Dir(file), nil

}
