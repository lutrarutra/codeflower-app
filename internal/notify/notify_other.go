//go:build !linux

package notify

func newNotifier(func(string)) (Notifier, error) { return nil, ErrUnavailable }
