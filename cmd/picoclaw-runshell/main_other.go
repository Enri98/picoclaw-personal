//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "picoclaw-runshell: Linux-only binary; build with GOOS=linux")
	os.Exit(1)
}
