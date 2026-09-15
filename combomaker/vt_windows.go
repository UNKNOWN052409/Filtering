//go:build windows

package main

import (
	"runtime"
	"syscall"
	"unsafe"
)

// enableVTWindows turns on ANSI escape processing so the TUI renders
// colors/cursor control on cmd.exe and PowerShell.
func enableVTWindows() {
	if runtime.GOOS != "windows" {
		return
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	consoleHandle, _ := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	var mode uint32
	getConsoleMode.Call(uintptr(consoleHandle), uintptr(unsafe.Pointer(&mode)))
	const ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004
	setConsoleMode.Call(uintptr(consoleHandle), uintptr(mode|ENABLE_VIRTUAL_TERMINAL_PROCESSING))
}
