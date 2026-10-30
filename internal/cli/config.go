// Package cli implements the hanse command line client
// (docs/architecture.md section 12): configuration, the read-only
// discovery commands, text and JSON output, and the error exit codes of
// section 14.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// config is the CLI configuration file. It holds the Gateway URL and the
// bearer token, nothing else.
type config struct {
	GatewayURL string `json:"gateway_url"`
	Token      string `json:"token"`
}

// configDirName is the directory under the user configuration directory
// that holds the CLI configuration.
const configDirName = "hanse"

// configFileName is the CLI configuration file name.
const configFileName = "config.json"

// fileMode keeps the token readable by its owner only.
const fileMode os.FileMode = 0o600

// dirMode keeps the configuration directory private to its owner.
const dirMode os.FileMode = 0o700

// defaultConfigPath returns the CLI configuration path under
// os.UserConfigDir(): $XDG_CONFIG_HOME/hanse/config.json on Linux,
// ~/Library/Application Support/hanse/config.json on macOS, and
// %AppData%\hanse\config.json on Windows. docs/configuration.md section
// 1.1 lists the Gateway and node agent files but not the CLI file; this is
// the location the CLI owns.
func defaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("the user configuration directory is not known: %w", err)
	}
	return filepath.Join(dir, configDirName, configFileName), nil
}

// loadConfig reads the configuration file. A missing file returns the
// error unwrapped, so the caller can report that the user is not logged
// in.
func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("%s is not a valid CLI configuration file: %w", path, err)
	}
	return cfg, nil
}

// saveConfig writes the configuration file with mode 0600 and its
// directory with mode 0700. It writes to a temporary file and renames it,
// so an interrupted write never leaves a truncated configuration.
func saveConfig(path string, cfg config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("cannot create the configuration directory %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory alone; tighten only ours.
	if err := os.Chmod(dir, dirMode); err != nil && !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("cannot set the permissions of %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode the configuration: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, configFileName+".*")
	if err != nil {
		return fmt.Errorf("cannot create a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot set the permissions of %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cannot replace %s: %w", path, err)
	}
	return nil
}
