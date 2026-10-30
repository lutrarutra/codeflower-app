package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"codeflower-app/internal/sandbox"
)

func TestTaskOutputModeSelectsRawOrAttached(t *testing.T) {
	for _, test := range []struct {
		requested string
		terminal  bool
		want      string
	}{
		{"auto", true, "attached"},
		{"auto", false, "raw"},
		{"raw", true, "raw"},
		{"raw", false, "raw"},
		{"attached", true, "attached"},
		{"attached", false, "attached"},
	} {
		got, err := taskOutputMode(test.requested, test.terminal)
		if err != nil || got != test.want {
			t.Errorf("taskOutputMode(%q, %t) = %q, %v; want %q", test.requested, test.terminal, got, err, test.want)
		}
	}
	if _, err := taskOutputMode("pretty", true); err == nil {
		t.Fatal("accepted an unknown output mode")
	}
	if progressProtocol("attached") != "json" || progressProtocol("raw") != "" {
		t.Fatal("selected output modes are not sent to the sandbox correctly")
	}
}

func TestTaskProgressParsesStructuredEventsAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	started := time.Now().Add(-12 * time.Second)
	progress := newTaskProgress(&out, false, "Claude Code", "", "", started)
	progress.Start()

	usage := &sandbox.TaskUsage{CostEstimate: true}
	usage.Totals.Tokens.Total = 12_300
	usage.Totals.Cost = &sandbox.TaskCost{Amount: 0.25, Currency: "USD"}
	percent := 25.0
	usage.Limits = []sandbox.TaskUsageLimit{{Label: "5-hour", UsedPercent: &percent}}
	events := []string{
		`{"codeflowerTask":true,"type":"phase","phase":"Round 1/2 · working"}`,
		`{"codeflowerTask":true,"type":"tool","title":"Run analysis.py"}`,
		`{"codeflowerTask":true,"type":"text","text":"Working on the report.\n"}`,
		`{"codeflowerTask":true,"type":"settings","settings":{"model":"opus","effort":"high"}}`,
		fmt.Sprintf(`{"codeflowerTask":true,"type":"stats","usage":%s}`, mustJSON(t, usage)),
	}
	stream := []byte(strings.Join(events, "\n") + "\n")
	for index := 0; index < len(stream); index += 7 {
		end := min(index+7, len(stream))
		if _, err := progress.Write(stream[index:end]); err != nil {
			t.Fatal(err)
		}
	}
	if err := progress.Close(); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	for _, want := range []string{"Round 1/2 · working", "▸ Run analysis.py", "Working on the report.", "Settings selected:", "opus/high", "12.3k tokens", "~$0.25", "5h 25%"} {
		if !strings.Contains(output, want) {
			t.Errorf("attached output %q does not contain %q", output, want)
		}
	}
}

func TestTaskProgressPassesThroughOlderSandboxOutput(t *testing.T) {
	var out bytes.Buffer
	progress := newTaskProgress(&out, false, "Claude Code", "", "", time.Now())
	if _, err := progress.Write([]byte("legacy task output")); err != nil {
		t.Fatal(err)
	}
	if err := progress.Close(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[attached progress unavailable from this sandbox image; showing raw output]\nlegacy task output" {
		t.Fatalf("got %q", out.String())
	}
}

func TestTaskProgressPausesForTerminalSignIn(t *testing.T) {
	var out bytes.Buffer
	progress := newTaskProgress(&out, true, "Claude Code", "", "", time.Now())
	progress.Start()
	for _, event := range []string{
		`{"codeflowerTask":true,"type":"auth-start","text":"Signing in to Claude Code…"}`,
		`{"codeflowerTask":true,"type":"auth-end"}`,
	} {
		if _, err := progress.Write([]byte(event + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := progress.Close(); err != nil {
		t.Fatal(err)
	}
	if output := out.String(); !strings.Contains(output, "Signing in to Claude Code") || strings.Contains(output, "unsupported attached progress event") {
		t.Fatalf("sign-in progress was not handled: %q", output)
	}
}

func TestTaskProgressFooterAndFinalUsage(t *testing.T) {
	var out bytes.Buffer
	started := time.Now().Add(-time.Minute)
	progress := newTaskProgress(&out, true, "Claude Code", "", "", started)
	progress.Start()
	percent := 40.0
	usage := &sandbox.TaskUsage{CostEstimate: true}
	usage.Totals.Tokens.Total = 1_234
	usage.Totals.Cost = &sandbox.TaskCost{Amount: 1.25, Currency: "USD"}
	usage.Limits = []sandbox.TaskUsageLimit{{Label: "5-hour", UsedPercent: &percent}}
	data := fmt.Sprintf(
		"{\"codeflowerTask\":true,\"type\":\"settings\",\"settings\":{\"model\":\"opus\",\"effort\":\"high\"}}\n"+
			"{\"codeflowerTask\":true,\"type\":\"stats\",\"usage\":%s}\n",
		mustJSON(t, usage),
	)
	if _, err := progress.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := progress.Close(); err != nil {
		t.Fatal(err)
	}
	output := out.String()
	for _, want := range []string{"opus/high", "1.2k tokens", "~$1.25", "5h 40%"} {
		if !strings.Contains(output, want) {
			t.Errorf("footer %q does not contain %q", output, want)
		}
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTaskProgressSaysWhyATaskContinuesAndWrapsUp(t *testing.T) {
	var out bytes.Buffer
	progress := newTaskProgress(&out, false, "Claude Code", "", "", time.Now())
	var wrappedUpReason string
	progress.SetOnWrapUp(func(reason string) {
		wrappedUpReason = reason
	})
	progress.Start()
	stream := `{"codeflowerTask":true,"type":"continuation","kind":"unmet","followUp":true}` + "\n" +
		`{"codeflowerTask":true,"type":"wrap-up","reason":"interrupt"}` + "\n"
	if _, err := progress.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	progress.Notice("Interrupt received; wrapping up...")
	if err := progress.Close(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Attempt: continued the session: the task was not done, with a follow-up",
		"Wrapping up: interrupted",
		"Interrupt received; wrapping up...",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("attached output %q does not contain %q", out.String(), want)
		}
	}
	if wrappedUpReason != "interrupt" {
		t.Errorf("expected wrappedUpReason %q, got %q", "interrupt", wrappedUpReason)
	}
	if strings.Contains(out.String(), "unsupported") {
		t.Errorf("attached output %q has unsupported events", out.String())
	}
}

func TestTerminalWatcherDetectsCtrlDAndForwardsInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	tw := newTerminalWatcher(r, true)
	defer tw.close()

	// Write normal input with an embedded Ctrl+D (0x04)
	if _, err := w.Write([]byte("hello\x04world\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case <-tw.ctrlD:
		// Succeeded in detecting Ctrl+D byte
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ctrlD event from byte 4")
	}

	buf := make([]byte, 100)
	n, err := tw.pipeReader.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "helloworld\n" {
		t.Fatalf("expected forwarded input %q, got %q", "helloworld\n", got)
	}

	// Close write end (simulates terminal EOF / Ctrl+D on empty line)
	_ = w.Close()

	select {
	case <-tw.ctrlD:
		// Succeeded in detecting EOF
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ctrlD event from EOF")
	}
}
