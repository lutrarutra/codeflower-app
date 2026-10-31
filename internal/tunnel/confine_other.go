//go:build !linux

package tunnel

import "errors"

// Confine is Linux-only (Landlock).
func Confine(string, string) error {
	return errors.New("confining the connector needs Linux (Landlock)")
}
