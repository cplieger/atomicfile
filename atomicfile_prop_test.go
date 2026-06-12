package atomicfile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// WriteFile must round-trip arbitrary byte payloads and arbitrary valid modes,
// leaving no temp artifact behind on success.
func TestWriteFile_RapidRoundTrip(t *testing.T) {
	dir := t.TempDir()
	var counter int
	rapid.Check(t, func(t *rapid.T) {
		data := rapid.SliceOf(rapid.Byte()).Draw(t, "data")
		mode := rapid.SampledFrom([]os.FileMode{0o600, 0o644, 0o755}).Draw(t, "mode")

		counter++
		path := filepath.Join(dir, fmt.Sprintf("testfile-%d", counter))

		if _, err := WriteFile(context.Background(), path, data, WithMode(mode)); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if len(data) != len(got) {
			t.Fatalf("round-trip length mismatch: wrote %d, read %d", len(data), len(got))
		}
		if string(got) != string(data) {
			t.Fatalf("round-trip content mismatch")
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".atomicfile-") && strings.HasSuffix(e.Name(), ".tmp") {
				t.Errorf("stale temp file found: %s", e.Name())
			}
		}
	})
}

// isStaleTempName must accept exactly the os.CreateTemp output shape for the
// package pattern (".atomicfile-<digits>.tmp") and reject everything else.
func TestIsStaleTempName_RapidShape(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		middle := rapid.StringOfN(rapid.RuneFrom([]rune("0123456789")), 1, 12, -1).Draw(t, "digits")
		name := tempPrefix + middle + tempSuffix
		if !isStaleTempName(name) {
			t.Fatalf("isStaleTempName(%q) = false, want true for a digit-suffixed temp", name)
		}
	})
}

// A name whose middle segment contains a non-digit must never be reclaimed,
// protecting caller-owned files that merely share the prefix/suffix.
func TestIsStaleTempName_RapidNonDigitSpared(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Draw a middle that contains at least one non-digit rune.
		letters := rapid.StringOfN(rapid.RuneFrom([]rune("abcdefghijklmnopqrstuvwxyz._-")), 1, 10, -1).Draw(t, "letters")
		digits := rapid.StringOfN(rapid.RuneFrom([]rune("0123456789")), 0, 6, -1).Draw(t, "digits")
		middle := digits + letters
		name := tempPrefix + middle + tempSuffix
		if isStaleTempName(name) {
			t.Fatalf("isStaleTempName(%q) = true, want false (non-digit middle must be spared)", name)
		}
	})
}
