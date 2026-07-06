package applog

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

var (
	Info  = log.New(os.Stdout, "[remote] ", log.LstdFlags)
	Error = log.New(os.Stderr, "[ERROR] ", log.LstdFlags)
)

func Errorf(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	_, file, line, _ := runtime.Caller(1)
	Error.Printf("%s:%d %s", filepath.Base(file), line, msg)
}
