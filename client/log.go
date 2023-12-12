package client


import (
	"fmt"
	"log"
)


var defaultLogger = log.Default()

func logger() *log.Logger {
    return defaultLogger
}

func logFn(tag string) func(string, ...any) {
    return func(format string, a ...any) {
        m := fmt.Sprintf(format, a...)
        logger().Printf("%s: %s\n", tag, m)
    }
}

