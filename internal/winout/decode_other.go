//go:build !windows

package winout

// Decode returns console output as UTF-8. Elsewhere it already is.
func Decode(b []byte) string { return string(b) }
