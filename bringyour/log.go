package bringyour

import (
	"os"
	"log"
)


var logger = log.New(os.Stderr, "", log.Ldate | log.Ltime | log.Lshortfile)


func Logger() *log.Logger {
	return logger
}
