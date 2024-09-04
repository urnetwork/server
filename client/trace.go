package client

import (
	"fmt"
)

func trace(callback func()) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%s", r)
			}
		}
	}()
	callback()
	return
}

func traceWithError(callback func() error) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%s", r)
			}
		}
	}()
	returnErr = callback()
	return
}

func traceWithReturn[R any](callback func() R) (result R, returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%s", r)
			}
		}
	}()
	result = callback()
	return
}

func traceWithReturnError[R any](callback func() (R, error)) (result R, returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%s", r)
			}
		}
	}()
	result, returnErr = callback()
	return
}
