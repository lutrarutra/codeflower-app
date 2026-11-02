package sandbox

import (
	"io"
	"os"

	"github.com/moby/term"
)

// rawTerminal puts the user's terminal in raw mode, so every key reaches the
// program in the container, which has a terminal of its own; it returns how
// to put it back.
func rawTerminal(in io.Reader) func() {
	file, ok := in.(*os.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return func() {}
	}
	state, err := term.SetRawTerminal(file.Fd())
	if err != nil {
		return func() {}
	}
	return func() { _ = term.RestoreTerminal(file.Fd(), state) }
}

// followTerminalSize calls resize with the terminal's size now and whenever
// it changes, until the returned function stops it.
func followTerminalSize(out io.Writer, resize func(width, height uint)) func() {
	file, ok := out.(*os.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return func() {}
	}
	var last term.Winsize
	report := func() {
		size, err := term.GetWinsize(file.Fd())
		if err != nil || size.Width == 0 || size.Height == 0 || (size.Width == last.Width && size.Height == last.Height) {
			return
		}
		last = *size
		resize(uint(size.Width), uint(size.Height))
	}
	report()
	return watchTerminalSize(report)
}
