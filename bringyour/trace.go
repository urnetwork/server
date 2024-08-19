package bringyour

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
	// mathrand "math/rand"

	"github.com/golang/glog"
)

func IsDoneError(r any) bool {
	isDoneMessage := func(message string) bool {
		switch message {
		case "Done", "Done.":
			return true
		// pgx
		case "context canceled", "timeout: context already done: context canceled":
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

// this is meant to handle unexpected errors and do some cleanup
func HandleError(do func(), handlers ...any) (r any) {
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
	do()
	return
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
