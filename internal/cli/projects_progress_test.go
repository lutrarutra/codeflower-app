package cli

import (
	"testing"
	"time"

	"codeflower-app/internal/sandbox"
)

func TestThousands(t *testing.T) {
	for count, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := thousands(count); got != want {
			t.Errorf("thousands(%d) = %q, want %q", count, got, want)
		}
	}
}

func TestProgressLinesSayWhatIsLeft(t *testing.T) {
	for _, test := range []struct {
		report sandbox.ProgressReport
		want   string
	}{
		{sandbox.ProgressReport{Files: 1204, Bytes: 6_300_000_000, Written: 2_100_000_000, Elapsed: 10 * time.Second},
			"1,204 files, 6.3 GB, 210.0 MB/s"},
		{sandbox.ProgressReport{Files: 1204, Bytes: 2_100_000_000, TotalFiles: 3000, TotalBytes: 9_800_000_000},
			"1,204 of ≥3,000 files, 2.1 GB of ≥9.8 GB"},
		{sandbox.ProgressReport{Files: 1950, Bytes: 6_300_000_000, TotalFiles: 4112, TotalBytes: 18_400_000_000, Counted: true},
			"1,950 of 4,112 files, 6.3 GB of 18.4 GB (34%)"},
		{sandbox.ProgressReport{Files: 120, TotalFiles: 400, Counted: true}, "120 of 400 files (30%)"},
		{sandbox.ProgressReport{Files: 7}, "7 files"},
	} {
		if got := progressLine(test.report); got != test.want {
			t.Errorf("%+v: %q, want %q", test.report, got, test.want)
		}
	}
}
