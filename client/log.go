package client


import (
	"fmt"
	"log"
)


var logger = log.Default()

func Logger() *log.Logger {
    return logger
}

func LogFn(tag string) func(string, ...any) {
    return func(format string, a ...any) {
        m := fmt.Sprintf(format, a...)
        Logger().Printf("%s: %s\n", tag, m)
    }
}

