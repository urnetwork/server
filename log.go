package server

import (
	"fmt"
	"log"
	"os"
)

// use glog
const LogLevelUrgent = 0
const LogLevelInfo = 50
const LogLevelDebug = 100

var GlobalLogLevel = LogLevelInfo

var logger = log.New(os.Stdout, "", log.Ldate|log.Ltime|log.Lshortfile)

func Logger() *log.Logger {
	return logger
}

func SetLogger(l *log.Logger) {
	logger = l
}

type Log struct {
	level int
}

func LogFn(level int, tag string) LogFunction {
	return func(format string, a ...any) {
		if level <= GlobalLogLevel {
			m := fmt.Sprintf(format, a...)
			Logger().Printf("%s: %s\n", tag, m)
		}
	}
}

func SubLogFn(level int, log LogFunction, tag string) LogFunction {
	return func(format string, a ...any) {
		if level <= GlobalLogLevel {
			m := fmt.Sprintf(format, a...)
			log("%s: %s", tag, m)
		}
	}
}

type LogFunction func(string, ...any)

func (self *LogFunction) Set() {

}
