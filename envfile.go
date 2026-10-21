package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// parseEnvLine splits a KEY=VALUE line from an env file. Blank lines and
// comments yield ok=false. Matching outer quotes around the value are removed.
func parseEnvLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	key, value, ok = strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return "", "", false
	}
	value = strings.TrimSpace(value)
	if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
		value = value[1 : n-1]
	}
	return key, value, true
}

// loadEnvFile reads KEY=VALUE pairs (the .env format used by Compose) into the
// environment. Variables that are already set take precedence over the file.
func loadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("gagal membaca config %s: %w", path, err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		key, value, ok := parseEnvLine(sc.Text())
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, value)
		}
	}
	return sc.Err()
}

// ensureSecret gives the env file a random SECRET_KEY when it has none, so the
// installer never needs to pass the secret on a command line.
func ensureSecret(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var lines []string
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	idx := -1
	for i, line := range lines {
		if key, value, ok := parseEnvLine(line); ok && key == "SECRET_KEY" {
			if value != "" {
				return nil
			}
			idx = i
		}
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	entry := "SECRET_KEY=" + hex.EncodeToString(buf)
	if idx >= 0 {
		lines[idx] = entry
	} else {
		lines = append(lines, entry)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}
