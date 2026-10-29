package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"codeflower-launcher/internal/sandbox"
)

type taskProgressEvent struct {
	CodeflowerTask bool               `json:"codeflowerTask"`
	Type           string             `json:"type"`
	Text           string             `json:"text,omitempty"`
	Title          string             `json:"title,omitempty"`
	Entries        []string           `json:"entries,omitempty"`
	Settings       map[string]string  `json:"settings,omitempty"`
	Usage          *sandbox.TaskUsage `json:"usage,omitempty"`
	Round          int                `json:"round,omitempty"`
	MaxRounds      int                `json:"maxRounds,omitempty"`
	Reason         string             `json:"reason,omitempty"`
	Phase          string             `json:"phase,omitempty"`
	Kind           string             `json:"kind,omitempty"`
	FollowUp       bool               `json:"followUp,omitempty"`
}

// taskProgress translates the sandbox's task events into terminal-friendly
// progress while retaining a raw passthrough for images without this protocol.
type taskProgress struct {
	out        io.Writer
	terminal   bool
	agent      string
	started    time.Time
	settings   map[string]string
	usage      *sandbox.TaskUsage
	buffer     []byte
	recognized bool
	fallback   bool
	lineOpen   bool
	footer     bool
	closed     bool
	onWrapUp   func(reason string)
	mu         sync.Mutex
	stop       chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup
	writeErr   error
}

func newTaskProgress(out io.Writer, terminal bool, agent, model, effort string, started time.Time) *taskProgress {
	settings := map[string]string{}
	if model != "" {
		settings["model"] = model
	}
	if effort != "" {
		settings["effort"] = effort
	}
	return &taskProgress{
		out:      out,
		terminal: terminal,
		agent:    agent,
		started:  started,
		settings: settings,
		stop:     make(chan struct{}),
	}
}

func (p *taskProgress) Start() {
	if !p.terminal {
		return
	}
	p.mu.Lock()
	p.drawFooter()
	p.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.mu.Lock()
				if p.footer && !p.closed {
					p.drawFooter()
				}
				p.mu.Unlock()
			}
		}
	}()
}

func (p *taskProgress) SetOnWrapUp(fn func(reason string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onWrapUp = fn
}

func (p *taskProgress) Notice(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.writeText("  " + text + "\n")
}

func (p *taskProgress) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	if p.writeErr != nil {
		return 0, p.writeErr
	}
	if p.fallback {
		n, err := p.out.Write(data)
		if err != nil {
			p.writeErr = err
		}
		return n, err
	}

	p.buffer = append(p.buffer, data...)
	for {
		newline := indexByte(p.buffer, '\n')
		if newline < 0 {
			return len(data), nil
		}
		line := p.buffer[:newline]
		remaining := p.buffer[newline+1:]
		if !p.processLine(line) {
			raw := append([]byte(nil), p.buffer...)
			p.buffer = nil
			if err := p.fallbackToRaw(raw); err != nil {
				p.writeErr = err
				return len(data), err
			}
			return len(data), nil
		}
		p.buffer = remaining
		if p.writeErr != nil {
			return len(data), p.writeErr
		}
	}
}

func (p *taskProgress) Close() error {
	p.stopOnce.Do(func() {
		close(p.stop)
		p.wg.Wait()
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.writeErr
	}
	p.closed = true
	if !p.fallback && len(p.buffer) > 0 {
		if !p.processLine(p.buffer) {
			p.writeErr = p.fallbackToRaw(p.buffer)
		}
		p.buffer = nil
	}
	p.clearFooter()
	if p.lineOpen && p.terminal && p.writeErr == nil {
		_, p.writeErr = io.WriteString(p.out, "\n")
	}
	return p.writeErr
}

func (p *taskProgress) fallbackToRaw(data []byte) error {
	p.clearFooter()
	if p.writeErr != nil {
		return p.writeErr
	}
	if !p.fallback {
		p.fallback = true
		if _, err := io.WriteString(p.out, "[attached progress unavailable from this sandbox image; showing raw output]\n"); err != nil {
			return err
		}
	}
	_, err := p.out.Write(data)
	return err
}

func (p *taskProgress) processLine(line []byte) bool {
	var event taskProgressEvent
	if err := json.Unmarshal(line, &event); err != nil || !event.CodeflowerTask {
		if p.recognized {
			p.clearFooter()
			_, p.writeErr = fmt.Fprintf(p.out, "[invalid attached progress event] %s\n", line)
			return true
		}
		return false
	}
	p.recognized = true
	p.render(event)
	return true
}

func (p *taskProgress) render(event taskProgressEvent) {
	switch event.Type {
	case "text":
		p.writeText(event.Text)
	case "tool":
		p.writeText("\n  ▸ " + event.Title + "\n")
	case "plan":
		var lines strings.Builder
		lines.WriteString("\nPlan\n")
		for _, entry := range event.Entries {
			fmt.Fprintf(&lines, "  - %s\n", entry)
		}
		p.writeText(lines.String())
	case "notice":
		p.writeText("  " + event.Text + "\n")
	case "phase":
		label := event.Phase
		if event.Round > 0 {
			label = fmt.Sprintf("Round %d/%d · %s", event.Round, event.MaxRounds, label)
		}
		p.writeText("\n" + label + "\n")
	case "round":
		text := fmt.Sprintf("\nNot done yet; starting round %d/%d", event.Round, event.MaxRounds)
		if event.Reason != "" {
			text += ": " + event.Reason
		}
		p.writeText(text + "\n")
	case "continuation":
		p.writeText("\nAttempt: " + describeContinuation(sandbox.TaskContinuation{Kind: event.Kind, FollowUp: event.FollowUp}) + "\n")
	case "wrap-up":
		if p.onWrapUp != nil {
			p.onWrapUp(event.Reason)
		}
		p.writeText("\nWrapping up: " + wrapUpReason(event.Reason) + "\n")
	case "settings":
		for key, value := range event.Settings {
			p.settings[key] = value
		}
		p.statusChanged("Settings selected")
	case "stats":
		p.usage = event.Usage
		p.statusChanged("Usage updated")
	case "auth-start":
		p.clearFooter()
		if event.Text != "" {
			_, p.writeErr = fmt.Fprintf(p.out, "\n%s\n", event.Text)
		}
		p.lineOpen = false
	case "auth-end":
		if p.terminal {
			_, p.writeErr = io.WriteString(p.out, "\n")
			p.lineOpen = false
			p.drawFooter()
		}
	default:
		p.writeText(fmt.Sprintf("\n[unsupported attached progress event: %s]\n", event.Type))
	}
}

func (p *taskProgress) statusChanged(label string) {
	if p.writeErr != nil {
		return
	}
	if !p.terminal {
		p.writeText(fmt.Sprintf("  %s: %s\n", label, p.statusLine()))
		return
	}
	if !p.lineOpen {
		p.drawFooter()
	}
}

func (p *taskProgress) writeText(text string) {
	if p.writeErr != nil {
		return
	}
	if p.terminal {
		p.clearFooter()
		if p.writeErr != nil {
			return
		}
	}
	if _, err := io.WriteString(p.out, text); err != nil {
		p.writeErr = err
		return
	}
	if text != "" {
		p.lineOpen = !strings.HasSuffix(text, "\n")
	}
	if p.terminal && !p.lineOpen {
		p.drawFooter()
	}
}

func (p *taskProgress) drawFooter() {
	if !p.terminal || p.lineOpen || p.writeErr != nil {
		return
	}
	_, p.writeErr = fmt.Fprintf(p.out, "\r\x1b[2K%s", p.statusLine())
	p.footer = p.writeErr == nil
}

func (p *taskProgress) clearFooter() {
	if !p.footer || p.writeErr != nil {
		return
	}
	_, p.writeErr = io.WriteString(p.out, "\r\x1b[2K")
	p.footer = false
}

func (p *taskProgress) statusLine() string {
	parts := []string{"Running " + elapsed(time.Since(p.started))}
	model, effort := p.settings["model"], p.settings["effort"]
	if model != "" && effort != "" {
		parts = append(parts, strings.TrimSpace(p.agent+" "+model+"/"+effort))
	} else if model != "" {
		parts = append(parts, "model "+model)
	} else if effort != "" {
		parts = append(parts, "effort "+effort)
	} else if p.agent != "" {
		parts = append(parts, p.agent)
	}
	if p.usage == nil {
		return strings.Join(parts, " | ")
	}
	tokens := p.usage.Totals.Tokens.Total
	if tokens > 0 {
		parts = append(parts, formatCount(tokens)+" tokens")
	}
	if cost := p.usage.Totals.Cost; cost != nil {
		parts = append(parts, formatCost(cost, p.usage.CostEstimate))
	}
	if credits := p.usage.Credits; credits != nil {
		if credits.Unlimited {
			parts = append(parts, "unlimited credits")
		} else if credits.Balance != "" {
			parts = append(parts, "credits "+credits.Balance)
		}
	}
	limits := make([]string, 0, 2)
	for _, limit := range p.usage.Limits {
		if limit.UsedPercent == nil {
			continue
		}
		limits = append(limits, shortLimitName(limit.Label)+" "+formatPercent(*limit.UsedPercent))
		if len(limits) == 2 {
			break
		}
	}
	if len(limits) > 0 {
		parts = append(parts, strings.Join(limits, ", "))
	}
	return strings.Join(parts, " | ")
}

func elapsed(duration time.Duration) string {
	duration = duration.Round(time.Second)
	if duration < time.Minute {
		return duration.String()
	}
	if duration < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(duration.Minutes()), int(duration.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(duration.Hours()), int(duration.Minutes())%60)
}

func formatCount(count int64) string {
	if count >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	}
	if count >= 100_000 {
		return fmt.Sprintf("%.0fk", float64(count)/1_000)
	}
	if count >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(count)/1_000)
	}
	return fmt.Sprint(count)
}

func formatCost(cost *sandbox.TaskCost, estimate bool) string {
	if cost == nil {
		return ""
	}
	prefix := ""
	switch strings.ToUpper(cost.Currency) {
	case "USD":
		prefix = "$"
	case "EUR":
		prefix = "€"
	case "GBP":
		prefix = "£"
	default:
		prefix = strings.ToUpper(cost.Currency) + " "
	}
	if estimate {
		prefix = "~" + prefix
	}
	return prefix + fmt.Sprintf("%.2f", cost.Amount)
}

func formatPercent(percent float64) string {
	return fmt.Sprintf("%.0f%%", percent)
}

func shortLimitName(label string) string {
	switch strings.ToLower(label) {
	case "5-hour":
		return "5h"
	case "weekly", "7-day":
		return "7d"
	case "weekly (opus)":
		return "7d opus"
	case "weekly (sonnet)":
		return "7d sonnet"
	default:
		return label
	}
}

func taskUsageSummary(usage *sandbox.TaskUsage) string {
	if usage == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if total := usage.Totals.Tokens.Total; total > 0 {
		parts = append(parts, fmt.Sprintf("%s tokens", formatCount(total)))
	}
	if cost := usage.Totals.Cost; cost != nil {
		parts = append(parts, formatCost(cost, usage.CostEstimate))
	}
	limits := make([]string, 0, len(usage.Limits))
	for _, limit := range usage.Limits {
		if limit.UsedPercent != nil {
			limits = append(limits, limit.Label+" "+formatPercent(*limit.UsedPercent))
		}
	}
	sort.Strings(limits)
	if len(limits) > 0 {
		parts = append(parts, "quota "+strings.Join(limits, ", "))
	}
	if usage.Credits != nil {
		if usage.Credits.Unlimited {
			parts = append(parts, "unlimited credits")
		} else if usage.Credits.Balance != "" {
			parts = append(parts, "credits "+usage.Credits.Balance)
		}
	}
	return strings.Join(parts, " · ")
}

func indexByte(data []byte, target byte) int {
	for index, value := range data {
		if value == target {
			return index
		}
	}
	return -1
}
