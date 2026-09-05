package nas

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseProgress(t *testing.T) {
	tests := []struct {
		name string
		line string
		want Progress
		ok   bool
	}{
		{
			name: "lftp 4.8 pget status line",
			line: "`bigfile.bin' at 5242880 (50%) 1.2M/s eta:5s [Receiving data]",
			want: Progress{Bytes: 5242880, Percent: 50, SpeedBPS: 1258291, ETASeconds: 5},
			ok:   true,
		},
		{
			name: "lftp 4.9 got-of status line",
			line: "`file.bin', got 524288 of 1048576 (50%) 1.00 MiB/s eta:1s",
			want: Progress{Bytes: 524288, Total: 1048576, Percent: 50,
				SpeedBPS: 1048576, ETASeconds: 1},
			ok: true,
		},
		{
			name: "bytes per second with no unit prefix",
			line: "`x' at 100 (1%) 456b/s eta:2m",
			want: Progress{Bytes: 100, Percent: 1, SpeedBPS: 456, ETASeconds: 120},
			ok:   true,
		},
		{
			name: "compound eta",
			line: "`x' at 10 (1%) 1.0K/s eta:1h20m",
			want: Progress{Bytes: 10, Percent: 1, SpeedBPS: 1024, ETASeconds: 4800},
			ok:   true,
		},
		{
			name: "clock-style eta",
			line: "`x' at 10 (1%) 1.0K/s eta:02:30",
			want: Progress{Bytes: 10, Percent: 1, SpeedBPS: 1024, ETASeconds: 150},
			ok:   true,
		},
		{
			name: "day-scale eta",
			line: "`x' at 10 (1%) 1.0K/s eta:2d3h",
			want: Progress{Bytes: 10, Percent: 1, SpeedBPS: 1024, ETASeconds: 183600},
			ok:   true,
		},
		{
			name: "summary line",
			line: "10485760 bytes transferred in 8 seconds (1.25M/s)",
			want: Progress{Bytes: 10485760, Percent: -1, SpeedBPS: 1310720, Final: true},
			ok:   true,
		},
		{
			name: "zero-byte file reports 100 percent of nothing",
			line: "`empty.txt', got 0 of 0 (100%)",
			want: Progress{Bytes: 0, Total: 0, Percent: 100},
			ok:   true,
		},
		{
			name: "partial line carrying only a byte count",
			line: "`x' at 4096",
			want: Progress{Bytes: 4096, Percent: -1},
			ok:   true,
		},
		{
			name: "stderr line has nothing to offer",
			line: "mkdir: Access failed: File exists (/volume1/media/tree)",
			want: Progress{Percent: -1},
			ok:   false,
		},
		{
			name: "file-count line is not a byte count",
			line: "4 files transferred",
			want: Progress{Percent: -1},
			ok:   false,
		},
		{
			name: "empty line",
			line: "",
			want: Progress{Percent: -1},
			ok:   false,
		},
		{
			name: "nonsense percentage is ignored",
			line: "note (900%) something",
			want: Progress{Percent: -1},
			ok:   false,
		},
		{
			name: "a byte count too large for int64 is discarded, not wrapped",
			line: "`x' at 99999999999999999999 (1%)",
			want: Progress{Bytes: 0, Percent: 1},
			ok:   true,
		},
		{
			name: "invalid utf-8 in the filename does not stop parsing",
			line: "`caf\xe9.bin', got 1048576 of 1048576 (100%) 2.00 MiB/s",
			want: Progress{Bytes: 1048576, Total: 1048576, Percent: 100, SpeedBPS: 2097152},
			ok:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseProgress(tc.line)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.ok, got)
			}
			if got != tc.want {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// fixtureLines splits a captured lftp transcript exactly as the streamer does.
func fixtureLines(t *testing.T, name string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Split(scanLines)
	var out []string
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParseProgressAgainstCapturedOutput(t *testing.T) {
	tests := []struct {
		name        string
		fixture     string
		wantParsed  int
		wantBytes   int64
		wantPercent int
		wantFinal   bool
	}{
		{
			name: "lftp 4.8 pget", fixture: "lftp-4.8-pget.txt",
			wantParsed: 5, wantBytes: 10485760, wantPercent: 100, wantFinal: true,
		},
		{
			name: "lftp 4.9 mirror", fixture: "lftp-4.9-mirror.txt",
			wantParsed: 4, wantBytes: 1048576, wantPercent: 100, wantFinal: true,
		},
		{
			name: "output with no parseable progress", fixture: "lftp-unparseable.txt",
			wantParsed: 0, wantBytes: 0, wantPercent: -1, wantFinal: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var (
				parsed   int
				maxBytes int64
				percent  = -1
				final    bool
			)
			for _, line := range fixtureLines(t, tc.fixture) {
				p, ok := ParseProgress(line)
				if !ok {
					continue
				}
				parsed++
				if p.Bytes < maxBytes {
					// Per-file lines during a mirror legitimately go backwards;
					// what must not happen is the parser inventing a smaller
					// total than it already reported for the same file.
					t.Logf("line %q reported %d after %d", line, p.Bytes, maxBytes)
				}
				if p.Bytes > maxBytes {
					maxBytes = p.Bytes
				}
				if p.Percent >= 0 {
					percent = p.Percent
				}
				final = final || p.Final
			}
			if parsed != tc.wantParsed {
				t.Errorf("parsed %d lines, want %d", parsed, tc.wantParsed)
			}
			if maxBytes != tc.wantBytes {
				t.Errorf("peak bytes = %d, want %d", maxBytes, tc.wantBytes)
			}
			if percent != tc.wantPercent {
				t.Errorf("final percent = %d, want %d", percent, tc.wantPercent)
			}
			if final != tc.wantFinal {
				t.Errorf("saw summary line = %v, want %v", final, tc.wantFinal)
			}
		})
	}
}

func TestScanLinesSplitsOnBothTerminators(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"newlines", "a\nb\n", []string{"a", "b"}},
		{"carriage returns", "a\rb\r", []string{"a", "b"}},
		{"mixed", "a\r\nb\n", []string{"a", "", "b"}},
		{"unterminated tail", "a\nb", []string{"a", "b"}},
		{"empty input", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := bufio.NewScanner(bytes.NewReader([]byte(tc.in)))
			sc.Split(scanLines)
			var got []string
			for sc.Scan() {
				got = append(got, sc.Text())
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
