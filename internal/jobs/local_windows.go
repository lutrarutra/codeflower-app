//go:build windows

package jobs

import (
	"context"
	"errors"
)

type localBackend struct{}

var errNoLocal = errors.New("local jobs need Linux or macOS")

func (localBackend) submit(context.Context, ScriptData, *Record) (string, error) {
	return "", errNoLocal
}
func (localBackend) status(context.Context, []*Record) (map[string]string, error) {
	return nil, errNoLocal
}
func (localBackend) history(context.Context, []*Record) (map[string]map[int]taskEnd, error) {
	return nil, errNoLocal
}
func (localBackend) cancel(context.Context, *Record) error { return errNoLocal }
