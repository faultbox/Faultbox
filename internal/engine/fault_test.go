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
			input: "openat=ENOENT:100%:/data/*",
			want:  FaultRule{Syscall: "openat", Errno: syscall.ENOENT, Probability: 1.0, PathGlob: "/data/*"},
		},
		{
			input: "openat=EIO:50%:/tmp/test-*",
			want:  FaultRule{Syscall: "openat", Errno: syscall.EIO, Probability: 0.5, PathGlob: "/tmp/test-*"},
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
			if got.Syscall != tt.want.Syscall || got.Errno != tt.want.Errno || got.Probability != tt.want.Probability || got.PathGlob != tt.want.PathGlob {
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

	// With path glob
	r2 := FaultRule{Syscall: "openat", Errno: syscall.ENOENT, Probability: 1.0, PathGlob: "/data/*"}
	got2 := r2.String()
	want2 := "openat=ENOENT:100%:/data/*"
	if got2 != want2 {
		t.Errorf("FaultRule.String() = %q, want %q", got2, want2)
	}
}

func TestIsSystemPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/usr/lib/libc.so.6", true},
		{"/lib/x86_64-linux-gnu/libc.so.6", true},
		{"/proc/self/status", true},
		{"/sys/class/net", true},
		{"/dev/null", true},
		{"/etc/ld.so.cache", true},
		{"/data/mydb/wal", false},
		{"/tmp/test", false},
		{"/home/user/app", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := IsSystemPath(tt.path)
			if got != tt.want {
				t.Errorf("IsSystemPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestMatchPath(t *testing.T) {
	tests := []struct {
		glob string
		path string
		want bool
	}{
		{"", "/anything", true},            // no glob matches all
		{"/data/*", "/data/foo", true},     // glob matches
		{"/data/*", "/data/foo/bar", false}, // * doesn't cross /
		{"/data/*", "/other/foo", false},   // no match
		{"/tmp/test-*", "/tmp/test-123", true},
	}

	for _, tt := range tests {
		t.Run(tt.glob+"_"+tt.path, func(t *testing.T) {
			r := FaultRule{PathGlob: tt.glob}
			got := r.MatchPath(tt.path)
			if got != tt.want {
				t.Errorf("MatchPath(%q) with glob %q = %v, want %v", tt.path, tt.glob, got, tt.want)
			}
		})
	}
}

func TestIsFileSyscall(t *testing.T) {
	if !IsFileSyscall("openat") {
		t.Error("expected openat to be a file syscall")
	}
	if !IsFileSyscall("open") {
		t.Error("expected open to be a file syscall")
	}
	if IsFileSyscall("write") {
		t.Error("expected write to not be a file syscall")
	}
	if IsFileSyscall("connect") {
		t.Error("expected connect to not be a file syscall")
	}
}
