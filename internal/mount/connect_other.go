//go:build !windows

package mount

import "errors"

// DefaultConnector has no Windows API to call here.
func DefaultConnector(ConnectArgs) (string, error) {
	return "", errors.New("the Windows connection is only available on Windows")
}
