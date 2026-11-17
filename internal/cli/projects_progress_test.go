package cli

import "testing"

func TestThousands(t *testing.T) {
	for count, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := thousands(count); got != want {
			t.Errorf("thousands(%d) = %q, want %q", count, got, want)
		}
	}
}
