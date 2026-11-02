package attach

import (
	"context"
	"errors"
	"fmt"
)

// Listing is a sandbox's chats or terminals, for `codeflower chats` and
// `codeflower terminals`.
type Listing struct {
	Target    Target
	Agent     string
	Chats     []Chat
	Terminals []Terminal
}

// connect resolves the sandbox and connects to it; with several, the
// argument must name one.
func connect(ctx context.Context, sandboxArgument string) (Target, *conn, status, error) {
	targets := Targets()
	target, err := ResolveTarget(sandboxArgument, targets)
	if errors.Is(err, errAmbiguous) {
		err = fmt.Errorf("%w; they are:\n%s", err, describeTargets(targets))
	}
	if err != nil {
		return Target{}, nil, status{}, err
	}
	c, err := dial(ctx, target.Connection)
	if err != nil {
		return Target{}, nil, status{}, err
	}
	var bridge status
	if err := c.call(ctx, "_codeflower/status", map[string]any{}, &bridge); err != nil {
		c.close()
		return Target{}, nil, status{}, err
	}
	return target, c, bridge, nil
}

func describeTargets(targets []Target) string {
	text := ""
	for index, target := range targets {
		text += fmt.Sprintf("  %2d  %-24s %-10s %s\n", index+1, target.Label(), target.Agent, target.ID)
	}
	return text
}

// ListChats lists a sandbox's chats, newest first.
func ListChats(ctx context.Context, sandboxArgument string) (Listing, error) {
	target, c, bridge, err := connect(ctx, sandboxArgument)
	if err != nil {
		return Listing{}, err
	}
	defer c.close()
	list, err := chats(ctx, c)
	if err != nil {
		return Listing{}, err
	}
	return Listing{Target: target, Agent: bridge.AgentLabel, Chats: list}, nil
}

// ListTerminals lists the terminal UIs running in a sandbox.
func ListTerminals(ctx context.Context, sandboxArgument string) (Listing, error) {
	target, c, bridge, err := connect(ctx, sandboxArgument)
	if err != nil {
		return Listing{}, err
	}
	defer c.close()
	if !bridge.Capabilities.ChatTerminals {
		return Listing{Target: target, Agent: bridge.AgentLabel}, nil
	}
	var listed struct {
		Terminals []Terminal `json:"terminals"`
	}
	if err := c.call(ctx, "_codeflower/terminal/list", map[string]any{}, &listed); err != nil {
		return Listing{}, err
	}
	return Listing{Target: target, Agent: bridge.AgentLabel, Terminals: listed.Terminals}, nil
}

// CloseTerminal quits a terminal UI in a sandbox, by its id or its chat's key.
func CloseTerminal(ctx context.Context, sandboxArgument, id string) error {
	listing, err := ListTerminals(ctx, sandboxArgument)
	if err != nil {
		return err
	}
	for _, terminal := range listing.Terminals {
		if terminal.TerminalID == id || terminal.SessionKey == id {
			_, c, _, err := connect(ctx, sandboxArgument)
			if err != nil {
				return err
			}
			defer c.close()
			return c.call(ctx, "_codeflower/terminal/close", map[string]any{"terminalId": terminal.TerminalID}, nil)
		}
	}
	return fmt.Errorf("no terminal %q runs in %s", id, listing.Target.Label())
}
