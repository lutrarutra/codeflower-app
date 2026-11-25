package cli

import (
	"github.com/spf13/cobra"

	auditcli "github.com/lutrarutra/codeflower-audit/cli"
)

// auditCommand is codeflower-audit's check, score, seal and verify, from the
// version of github.com/lutrarutra/codeflower-audit this app requires. The
// sandbox image pins its own release of the same module; the formats it
// seals (audit, rubric, seal) are what keeps the two compatible.
func auditCommand() *cobra.Command {
	return auditcli.New("audit", Version, false)
}
