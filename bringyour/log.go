package bringyour

import (
	"log"
)


var logger = log.Default()

func Logger() *log.Logger {
	return logger
}
