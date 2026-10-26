//go:build !windows

package main

import "os"

func signingTempDir() (string, error) {
	return os.MkdirTemp("", "viva-winsign-")
}
