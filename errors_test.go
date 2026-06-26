package atomicfile

import (
	"errors"
	"testing"
)

func TestWritePhase_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		want  string
		phase WritePhase
	}{
		{"create", "create temp file", PhaseTempCreate},
		{"write", "write temp file", PhaseTempWrite},
		{"chmod", "chmod temp file", PhaseTempChmod},
		{"sync", "sync temp file", PhaseTempSync},
		{"close", "close temp file", PhaseTempClose},
		{"rename", "rename to final path", PhaseRename},
		{"zero_is_unknown", "unknown phase", WritePhase(0)},
		{"out_of_range_is_unknown", "unknown phase", WritePhase(99)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.phase.String(); got != tt.want {
				t.Errorf("WritePhase(%d).String() = %q, want %q", int(tt.phase), got, tt.want)
			}
		})
	}
}

func TestWriteError_Unwrap_PreservesChain(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("underlying io failure")
	we := &WriteError{Phase: PhaseTempWrite, Err: sentinel}
	if !errors.Is(we, sentinel) {
		t.Errorf("errors.Is(WriteError, sentinel) = false, want true")
	}
	if got := we.Unwrap(); got != sentinel {
		t.Errorf("WriteError.Unwrap() = %v, want %v", got, sentinel)
	}
}

func TestWriteError_Error_FormatsPhasePrefix(t *testing.T) {
	t.Parallel()
	we := &WriteError{Phase: PhaseRename, Err: errors.New("disk gone")}
	got := we.Error()
	want := "atomicfile: rename to final path: disk gone"
	if got != want {
		t.Errorf("WriteError.Error() = %q, want %q", got, want)
	}
}
