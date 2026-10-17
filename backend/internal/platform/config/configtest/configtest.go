// Package configtest loads the repository's config.toml for tests, so tests
// run with the same rules and timings as the backend.
package configtest

import (
	"path/filepath"
	"runtime"
	"testing"

	"labyrinth/internal/platform/config"
)

// Settings loads backend/config.toml, failing the test if it's invalid.
func Settings(t testing.TB) config.Settings {
	t.Helper()
	_, file, _, _ := runtime.Caller(0) // …/backend/internal/platform/config/configtest/configtest.go
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "config.toml")
	s, err := config.LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
