package utils

import (
	"fmt"
	"time"
)

type DumpLogger struct {
}

func (l DumpLogger) Log(level, msg string) {
	fmt.Printf("[%s] %s => %v\r\n", level, time.Now().Format(time.RFC3339), msg)
}
func (l DumpLogger) Debug(msg string) {
	l.Log("DEBUG", msg)
}
func (l DumpLogger) Info(msg string) {
	l.Log("INFO", msg)
}
func (l DumpLogger) Warn(msg string) {
	l.Log("WARN", msg)
}
func (l DumpLogger) Error(msg string) {
	l.Log("ERROR", msg)
}
func (l DumpLogger) Fatal(msg string) {
	l.Log("FATAL", msg)
}
func (l DumpLogger) Panic(msg string) {
	l.Log("PANIC", msg)
}
