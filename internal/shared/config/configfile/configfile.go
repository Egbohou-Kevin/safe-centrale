// Package configfile holds what every program's configuration package does the same
// way: decode a JSON file strictly, read the log level and the durations with an
// error that says how to write them, read a secret kept in a file of its own, and
// replace a file atomically. It knows no program and no domain.
package configfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Decode reads the file at path into v, strictly (see DecodeBytes). A file that
// cannot be read gives the system's error, which names it; one that does not decode
// gives an error that starts with path.
func Decode(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := DecodeBytes(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// DecodeBytes decodes one JSON value into v. It is strict on purpose: a misspelt key
// is an error, not a default applied in silence, and so is anything after the value
// (a second object pasted below the first).
func DecodeBytes(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	// Token rather than More: More misses a stray closing brace or bracket.
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("invalid JSON: trailing data after the configuration")
	}
	return nil
}

// Level reads the log_level key; empty is info.
func Level(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("log_level %q: use debug, info, warn or error", s)
}

// Duration sets dst from value, the key named field; empty leaves dst (its default)
// alone.
func Duration(field, value string, dst *time.Duration) error {
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s %q: %w (write for example 45s, 2m or 1h30m)", field, value, err)
	}
	*dst = d
	return nil
}

// Path returns path as written in a configuration file kept in dir: a relative path
// is taken from that folder, not from wherever the program was started.
func Path(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// Secret reads a secret kept in a file of its own (path, see Path), without the
// spaces and line end around it: the configuration can then be shown or copied
// without it. The caller checks that it is not empty, and never logs it.
func Secret(dir, path string) (string, error) {
	b, err := os.ReadFile(Path(dir, path))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
