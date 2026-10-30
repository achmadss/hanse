package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultConfigPath requires the file under os.UserConfigDir.
func TestDefaultConfigPath(t *testing.T) {
	root, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("the user configuration directory is unknown: %v", err)
	}
	want := filepath.Join(root, "hanse", "config.json")
	got, err := defaultConfigPath()
	if err != nil {
		t.Fatalf("defaultConfigPath: %v", err)
	}
	if got != want {
		t.Errorf("defaultConfigPath() = %q, want %q", got, want)
	}
}

// TestSaveConfigOverwrites requires a second login to replace the file and
// keep its permissions private.
func TestSaveConfigOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hanse", "config.json")
	first := config{GatewayURL: "https://hanse.example", Token: "first-token"}
	if err := saveConfig(path, first); err != nil {
		t.Fatalf("save first config: %v", err)
	}
	second := config{GatewayURL: "https://hanse.example:8443", Token: "second-token"}
	if err := saveConfig(path, second); err != nil {
		t.Fatalf("save second config: %v", err)
	}

	got, err := loadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got != second {
		t.Errorf("loadConfig() = %+v, want %+v", got, second)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "gateway_url") || !strings.Contains(string(data), "token") {
		t.Errorf("config file %q lacks the documented keys", data)
	}
	if runtime.GOOS != "windows" {
		checkMode(t, path, fileMode)
		checkMode(t, filepath.Dir(path), dirMode)
	}
}

// TestLoadConfigErrors covers a missing and a malformed file.
func TestLoadConfigErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadConfig(filepath.Join(dir, "missing.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("loadConfig(missing) error %v, want fs.ErrNotExist", err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	_, err := loadConfig(bad)
	if err == nil {
		t.Fatal("loadConfig(bad) returned no error")
	}
	if !strings.Contains(err.Error(), "not a valid CLI configuration file") {
		t.Errorf("loadConfig(bad) error %v, want a configuration message", err)
	}
}
