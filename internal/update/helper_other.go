//go:build !windows

package update

import "time"

func platformOps() (HelperOps, error) { return HelperOps{}, ErrUnsupported }

// ProcessImage is the executable of a process; only Windows needs it.
func ProcessImage(int) (string, error) { return "", ErrUnsupported }

// WaitExit waits for a process to exit; only Windows needs it.
func WaitExit(int, time.Duration) error { return ErrUnsupported }
