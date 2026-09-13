// Package dotenv loads a local .env file's KEY=VALUE pairs into the
// process environment before anything else reads one — cmd/nexusd,
// cmd/signerd, cmd/nexusctl, and evals/cmd/runner each call Load as the
// very first line of main(), before obs.InitLogger or any of this
// codebase's many other envOr/os.Getenv call sites. .env.example at the
// repo root documents every environment variable this codebase reads.
//
// This is a pure local-dev convenience, the same zero-setup spirit --dev
// already follows elsewhere: every one of these variables has worked, and
// keeps working, via a plain shell export with no .env file at all — Load
// only fills in what the real environment didn't already set, and a
// missing .env is not an error (most invocations, and every CI/production
// deploy, won't have one).
package dotenv

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Load reads .env in the current working directory and calls os.Setenv
// for each KEY=VALUE line it finds, but ONLY when the real environment
// doesn't already have that key — an explicit `FOO=bar make run` or a
// deploy's own environment always wins over whatever a checked-in-adjacent
// .env says, the same precedence every other dotenv loader follows. Blank
// lines and lines starting with # are skipped; a value may optionally be
// wrapped in matching single or double quotes, stripped before use. No
// other expansion (no ${VAR} substitution, no multiline values, no
// `export` prefix) — every variable this codebase reads is a plain flat
// string, so there's nothing here that needs more than that.
func Load() error {
	f, err := os.Open(".env")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("dotenv: open .env: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		if n := len(value); n >= 2 {
			if (value[0] == '"' && value[n-1] == '"') || (value[0] == '\'' && value[n-1] == '\'') {
				value = value[1 : n-1]
			}
		}
		if _, present := os.LookupEnv(key); present {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("dotenv: set %s: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("dotenv: read .env: %w", err)
	}
	return nil
}
