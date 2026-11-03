package shares

import (
	"strings"
	"unicode/utf8"
)

var windowsReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}

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
	if num, ok := strings.CutPrefix(base, "COM"); ok && windowsPortNumber(num) {
		return false
	}
	if num, ok := strings.CutPrefix(base, "LPT"); ok && windowsPortNumber(num) {
		return false
	}
	return true
}

// windowsPortNumber reports whether s completes a COM or LPT device name: the
// digits 1-9 and the superscripts Windows also takes for 1, 2 and 3.
func windowsPortNumber(s string) bool {
	switch s {
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "\u00b9", "\u00b2", "\u00b3":
		return true
	}
	return false
}
