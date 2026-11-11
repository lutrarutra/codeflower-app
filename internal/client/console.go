package client

import (
	"bytes"
	"strings"
	"time"

	"codeflower-app/internal/sandbox"
)

// console collects what a sandbox or a download prints, as lines for the
// page. Its owner holds the lock around every call.
type console struct {
	lines []string
	// partial holds output up to its next newline. A carriage return starts
	// it again, as a progress bar redraws its line; the page shows it as the
	// last line meanwhile.
	partial bytes.Buffer
	// carriage: the last byte was a carriage return, which a newline may follow.
	carriage bool
	// last is when anything was printed, so the page can tell slow from stuck.
	last time.Time
	// progress is the image download's, while one runs.
	progress *sandbox.PullProgress
}

func (c *console) write(data []byte, redact func(string) string) {
	if len(data) > 0 {
		c.last = time.Now()
	}
	for _, b := range data {
		switch {
		case b == '\n':
			c.add(redact(c.partial.String()))
			c.partial.Reset()
			c.carriage = false
		case b == '\r':
			c.carriage = true
		default:
			if c.carriage {
				c.partial.Reset()
				c.carriage = false
			}
			c.partial.WriteByte(b)
		}
	}
}

func (c *console) add(line string) {
	c.lines = append(c.lines, line)
	if len(c.lines) > maxLogLines {
		c.lines = c.lines[len(c.lines)-maxLogLines:]
	}
}

// flush ends the line in progress.
func (c *console) flush(redact func(string) string) {
	if c.partial.Len() > 0 {
		c.add(redact(c.partial.String()))
		c.partial.Reset()
	}
	c.carriage = false
}

// view is the lines with the one in progress.
func (c *console) view(redact func(string) string) []string {
	lines := append(make([]string, 0, len(c.lines)+1), c.lines...)
	if rest := strings.TrimSpace(c.partial.String()); rest != "" {
		lines = append(lines, redact(c.partial.String()))
	}
	return lines
}

func (c *console) setProgress(progress sandbox.PullProgress) {
	c.progress = &progress
}

func (c *console) progressView() *sandbox.PullProgress {
	if c.progress == nil {
		return nil
	}
	progress := *c.progress
	return &progress
}
