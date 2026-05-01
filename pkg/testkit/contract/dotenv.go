//go:build docker

package contract

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dotenvDefaultPath is the canonical location for cloud-nightly env vars
// when not provided via the process environment (i.e., local-dev mode
// instead of CI). Resolved relative to $HOME (or $XDG_CONFIG_HOME when
// set). Override with $OMC_DOTENV_PATH for tests / non-default layouts.
const dotenvDefaultPath = ".config/oss-client/oss-client-cloud.env"

// init auto-loads the dotenv file at package-import time so every
// driver_test.go that imports pkg/testkit/contract picks up
// OMC_<VENDOR>_NIGHTLY_* values from a local file without callers
// having to `export` them per shell session.
//
// The dotenv file is a fallback: any variable already set in the
// process environment (e.g., from `export`, GitHub Actions secrets,
// or a parent shell) takes precedence and is NOT overridden.
//
// File format (one KEY=VALUE per line):
//
//	# Comments and blank lines are ignored.
//	OMC_VOLCENGINE_NIGHTLY_KEY=AKLT...
//	OMC_VOLCENGINE_NIGHTLY_SECRET=WVdab...
//	OMC_VOLCENGINE_NIGHTLY_BUCKET=oss-client
//	OMC_VOLCENGINE_NIGHTLY_REGION=cn-shanghai
//	# Quotes are optional and stripped if present:
//	OMC_GCS_NIGHTLY_KEY="/path/to/sa.json"
//
// Path resolution order:
//  1. $OMC_DOTENV_PATH (explicit override)
//  2. $XDG_CONFIG_HOME/oss-client/oss-client-cloud.env (XDG path)
//  3. $HOME/.config/oss-client/oss-client-cloud.env (default)
//
// A missing file is silently ignored — CI environments populate vars
// from secrets and never need the file.
func init() {
	loadDotEnv()
}

func loadDotEnv() {
	path := resolveDotEnvPath()
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return // missing file is the common case
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue // skip malformed lines silently
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if n := len(val); n >= 2 {
			if (val[0] == '"' && val[n-1] == '"') || (val[0] == '\'' && val[n-1] == '\'') {
				val = val[1 : n-1]
			}
		}
		if key == "" {
			continue
		}
		if _, set := os.LookupEnv(key); set {
			continue // pre-set env wins
		}
		_ = os.Setenv(key, val)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "contract.loadDotEnv: parse error in %s line %d: %v\n", path, lineNo, err)
	}
}

func resolveDotEnvPath() string {
	if p := os.Getenv("OMC_DOTENV_PATH"); p != "" {
		return p
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "oss-client", "oss-client-cloud.env")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, dotenvDefaultPath)
}
