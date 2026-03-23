package engine

import (
	"syscall"
	"testing"
)

func TestParseFaultRule(t *testing.T) {
	tests := []struct {
		input   string
		want    FaultRule
		wantErr bool
	}{
		{
			input: "open=ENOENT:50%",
			want:  FaultRule{Syscall: "open", Errno: syscall.ENOENT, Probability: 0.5},
		},
		{
			input: "write=EIO:100%",
			want:  FaultRule{Syscall: "write", Errno: syscall.EIO, Probability: 1.0},
		},
		{
			input: "connect=ECONNREFUSED:10%",
			want:  FaultRule{Syscall: "connect", Errno: syscall.ECONNREFUSED, Probability: 0.1},
		},
		{
			input:   "bad",
			wantErr: true,
		},
		{
			input:   "open=UNKNOWN:50%",
			wantErr: true,
		},
		{
			input:   "open=EIO:200%",
			wantErr: true,
		},
		{
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseFaultRule(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseFaultRule(%q) = %v, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFaultRule(%q) error: %v", tt.input, err)
			}
			if got.Syscall != tt.want.Syscall || got.Errno != tt.want.Errno || got.Probability != tt.want.Probability {
				t.Errorf("ParseFaultRule(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestFaultRuleString(t *testing.T) {
	r := FaultRule{Syscall: "open", Errno: syscall.ENOENT, Probability: 0.5}
	got := r.String()
	want := "open=ENOENT:50%"
	if got != want {
		t.Errorf("FaultRule.String() = %q, want %q", got, want)
	}
}
