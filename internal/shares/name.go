package shares

import (
	"strings"
	"unicode/utf8"
)

var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

// ValidName reports whether name can be created as a single path component
// on the given OS (a runtime.GOOS value). Bytes are never transformed.
func ValidName(goos, name string) bool {
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) {
		return false
	}
	if strings.ContainsAny(name, "\x00/") {
		return false
	}
	if goos != "windows" {
		return true
	}
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`<>:"|?*\`, r) {
			return false
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.ToUpper(strings.TrimRight(base, " "))
	if windowsReserved[base] {
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return false
	}
	return true
}
