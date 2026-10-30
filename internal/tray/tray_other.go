//go:build !linux && !windows && !darwin

package tray

const notifies = false

func available() error { return ErrUnavailable }

func run(Menu, <-chan Status, <-chan Notice, <-chan struct{}) error { return ErrUnavailable }
