package server

import (
	// "context"
	// "sync"
	"time"
	// "slices"
	// "os"
	// "os/signal"
	// "syscall"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"strings"
	// "reflect"
	mathrand "math/rand"

	"github.com/urnetwork/glog"
)

func IsDoneError(r any) bool {
	isDoneMessage := func(message string) bool {
		switch {
		case strings.HasPrefix(message, "Done"):
			return true
		// pgx
		case strings.Contains(message, "context canceled"),
			strings.HasPrefix(message, "failed to deallocate cached statement(s)"):
			return true
		default:
			return false
		}
	}
	switch v := r.(type) {
	case error:
		return isDoneMessage(v.Error())
	case string:
		return isDoneMessage(v)
	default:
		return false
	}
}

type RetryOptions struct {
	Count    int
	MinDelay time.Duration
	MaxDelay time.Duration
}

// this is meant to handle unexpected errors and do some cleanup
func HandleError(do func(), handlers ...any) (r any) {
	return HandleErrorWithRetry(do, RetryOptions{}, handlers...)
}

func HandleErrorWithRetry(do func(), retryOptions RetryOptions, handlers ...any) (r any) {
	defer func() {
		if r = recover(); r != nil {
			if IsDoneError(r) {
				// the context was canceled and raised. this is a standard pattern, do not log
			} else {
				glog.Infof("Unexpected error: %s\n", ErrorJson(r, debug.Stack()))
			}
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("%s", r)
			}
			for _, handler := range handlers {
				switch v := handler.(type) {
				case func():
					v()
				case func(error):
					v(err)
				}
			}
		}
	}()
	for range retryOptions.Count {
		success := false
		func() {
			defer func() {
				if r = recover(); r != nil {
					randomDelay := retryOptions.MinDelay + time.Duration(mathrand.Int63n(int64(retryOptions.MaxDelay)))
					if 0 < randomDelay {
						select {
						case <-time.After(randomDelay):
						}
					}
				}
			}()
			do()
			success = true
		}()
		if success {
			return
		}
	}
	do()
	return
}

func HandleError1[R any](do func() R, handlers ...any) (result R) {
	return HandleError1WithRetry(do, RetryOptions{}, handlers...)
}

func HandleError1WithRetry[R any](do func() R, retryOptions RetryOptions, handlers ...any) (result R) {
	HandleError(func() {
		result = do()
	}, retryOptions, func(err error) {
		for _, handler := range handlers {
			switch v := handler.(type) {
			case func() R:
				result = v()
			case func(error) R:
				result = v(err)
			}
		}
	})
	return
}

func HandleError2[R1 any, R2 any](do func() (R1, R2), handlers ...any) (result1 R1, result2 R2) {
	return HandleError2WithRetry(do, RetryOptions{}, handlers...)
}

func HandleError2WithRetry[R1 any, R2 any](do func() (R1, R2), retryOptions RetryOptions, handlers ...any) (result1 R1, result2 R2) {
	HandleErrorWithRetry(func() {
		result1, result2 = do()
	}, retryOptions, func(err error) {
		for _, handler := range handlers {
			switch v := handler.(type) {
			case func() (R1, R2):
				result1, result2 = v()
			case func(error) (R1, R2):
				result1, result2 = v(err)
			}
		}
	})
	return
}

func HandleErrorWithReturn[R any](do func() R) (result R) {
	return HandleError1(do)
}

func ErrorJson(err any, stack []byte) string {
	stackLines := []string{}
	for _, line := range strings.Split(string(stack), "\n") {
		stackLines = append(stackLines, strings.TrimSpace(line))
	}
	errorJson, _ := json.Marshal(map[string]any{
		"error": fmt.Sprintf("%T=%s", err, err),
		"stack": stackLines,
	})
	return string(errorJson)
}

func ErrorJsonNoStack(err any) string {
	errorJson, _ := json.Marshal(map[string]any{
		"error": fmt.Sprintf("%T=%s", err, err),
	})
	return string(errorJson)
}

func ErrorJsonWithCustomNoStack(err any, custom map[string]any) string {
	obj := map[string]any{
		"error": fmt.Sprintf("%T=%s", err, err),
	}
	for key, value := range custom {
		obj[key] = value
	}
	errorJson, _ := json.Marshal(obj)
	return string(errorJson)
}

func Trace(tag string, do func()) {
	trace(tag, func() string {
		do()
		return ""
	})
}

func TraceWithReturn[R any](tag string, do func() R) (result R) {
	trace(tag, func() string {
		result = do()
		return fmt.Sprintf(" = %v", result)
	})
	return
}

func TraceWithReturnShallowLog[R any](tag string, do func() R) (result R) {
	trace(tag, func() string {
		result = do()
		return fmt.Sprintf(" = %T", result)
	})
	return
}

func trace(tag string, do func() string) {
	start := time.Now()
	glog.Infof("[%-8s]%s (%d)\n", "start", tag, start.UnixMilli())
	doTag := do()
	end := time.Now()
	millis := float32(end.Sub(start)) / float32(time.Millisecond)
	glog.Infof("[%-8s]%s (%.2fms) (%d)%s\n", "end", tag, millis, end.UnixMilli(), doTag)
}
