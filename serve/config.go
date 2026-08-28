package serve

import (
	"os"
	"path/filepath"
	"strings"
)

// IncludeDirOf finds the directory a master config includes pool files from.
//
// It is read rather than guessed because the layouts genuinely differ: RHEL puts
// the master at /etc/php-fpm.conf including /etc/php-fpm.d/*.conf, while Debian
// uses /etc/php/8.2/fpm/php-fpm.conf including .../pool.d/*.conf. Deriving it
// from the master's own directory would be wrong on one of them, and writing a
// pool fragment somewhere PHP-FPM does not read is a silent no-op.
func IncludeDirOf(configPath string) string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "include") {
			continue
		}

		_, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		pattern := strings.TrimSpace(value)
		if pattern == "" {
			continue
		}
		dir := filepath.Dir(pattern)
		if dir == "." || dir == "/" {
			continue
		}
		// The include may be relative to the master config's own directory.
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(configPath), dir)
		}

		return dir
	}

	return ""
}

// PIDFileOf reads the master's pid file location from its config.
//
// Discovery can find the master by scanning the process table, but the pid file
// is the authoritative answer when it exists and does not need permission to
// inspect another user's /proc entry.
func PIDFileOf(configPath string) string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "pid") {
			continue
		}

		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != "pid" {
			continue
		}

		return strings.TrimSpace(value)
	}

	return ""
}
