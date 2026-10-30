// Command codeflower starts code flower sandboxes (workers) and signs in to
// their agents with the official CLIs.
package main

import (
	"os"

	"codeflower-app/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
