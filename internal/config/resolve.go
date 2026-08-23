package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// configSearchPaths returns the fixed locations Resolve checks when no explicit
// path is given, in order. The current working directory is deliberately NOT
// included — config must come from an explicit flag/env or a fixed location, so
// running sandbar from a random folder can't silently load a stray config.yaml.
func configSearchPaths() []string {
	var paths []string
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		paths = append(paths, filepath.Join(base, "sandbar", "config.yaml"))
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".config", "sandbar", "config.yaml"))
	}
	paths = append(paths, "/etc/sandbar/config.yaml")
	return paths
}

// DBPath returns the database file path for a config's `database` value. An
// absolute value is used as-is; a relative value resolves under the user data
// dir ($XDG_DATA_HOME or ~/.local/share/sandbar) — never next to the config
// file — so the DB always lands somewhere writable regardless of where the
// config lives. The directory is created if needed.
//
// NOTE (deployment): a relative `database` value resolves under the XDG data
// dir, which is the right convention for a per-user CLI; a packaged or service
// install should instead set an absolute path such as /var/lib/sandbar/ in its
// config.
func DBPath(dbField string) string {
	if dbField == "" {
		dbField = "sandbar.db"
	}
	if filepath.IsAbs(dbField) {
		return dbField
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".local", "share")
		}
	}
	dir := filepath.Join(base, "sandbar")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, filepath.Base(dbField))
}

// Resolve returns the path of the config file to load. Precedence:
//
//  1. explicit (the --config flag value), if non-empty
//  2. $SANDBAR_CONFIG
//  3. the first existing fixed location (see configSearchPaths)
//
// It never searches the working directory. If nothing is found it returns an
// error that lists every location it looked in.
func Resolve(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if p := os.Getenv("SANDBAR_CONFIG"); p != "" {
		return p, nil
	}
	paths := configSearchPaths()
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no config found — pass --config <path>, set $SANDBAR_CONFIG, or create one of:\n  %s",
		strings.Join(paths, "\n  "))
}
