//go:build !windows

package main

import "errors"

func isService() bool { return false }

func runService() error { return nil }

func serviceCommand([]string, string) error {
	return errors.New("perintah service hanya tersedia di Windows")
}
