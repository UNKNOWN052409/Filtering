//go:build !windows

package main

// enableVTWindows is a no-op off Windows — terminals already process ANSI.
func enableVTWindows() {}
