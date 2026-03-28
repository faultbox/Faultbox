package engine

import (
	"syscall"
	"testing"
	"time"
)

func TestParseFaultRule(t *testing.T) {
	tests := []struct {
		input    string
		want     FaultRule
		wantErr  bool
	}{
		{
			input: "open=ENOENT:50%",
			want:  FaultRule{Syscall: "open", Action: ActionDeny, Errno: syscall.ENOENT, Probability: 0.5},
		},
		{
			input: "write=EIO:100%",
			want:  FaultRule{Syscall: "write", Action: ActionDeny, Errno: syscall.EIO, Probability: 1.0},
		},
		{
			input: "connect=ECONNREFUSED:10%",
			want:  FaultRule{Syscall: "connect", Action: ActionDeny, Errno: syscall.ECONNREFUSED, Probability: 0.1},
		},
		{
			input: "openat=ENOENT:100%:/data/*",
			want:  FaultRule{Syscall: "openat", Action: ActionDeny, Errno: syscall.ENOENT, Probability: 1.0, PathGlob: "/data/*"},
		},
		{
			input: "openat=EIO:50%:/tmp/test-*",
			want:  FaultRule{Syscall: "openat", Action: ActionDeny, Errno: syscall.EIO, Probability: 0.5, PathGlob: "/tmp/test-*"},
		},
		// Delay rules
		{
			input: "connect=delay:200ms:100%",
			want:  FaultRule{Syscall: "connect", Action: ActionDelay, Delay: 200 * time.Millisecond, Probability: 1.0},
		},
		{
			input: "sendto=delay:50ms:20%",
			want:  FaultRule{Syscall: "sendto", Action: ActionDelay, Delay: 50 * time.Millisecond, Probability: 0.2},
		},
		{
			input: "write=delay:1s:100%",
			want:  FaultRule{Syscall: "write", Action: ActionDelay, Delay: time.Second, Probability: 1.0},
		},
		{
			input: "read=delay:500us:50%",
			want:  FaultRule{Syscall: "read", Action: ActionDelay, Delay: 500 * time.Microsecond, Probability: 0.5},
		},
		// Errors
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
		{
			input:   "connect=delay:badtime:100%",
			wantErr: true,
		},
		{
			input:   "connect=delay:200ms:200%",
			wantErr: true,
		},
		{
			input:   "connect=delay:200ms",
			wantErr: true,
		},
		// Trigger rules
		{
			input: "fsync=EIO:100%:after=2",
			want:  FaultRule{Syscall: "fsync", Action: ActionDeny, Errno: syscall.EIO, Probability: 1.0, Trigger: TriggerAfter, TriggerN: 2},
		},
		{
			input: "openat=ENOENT:100%:nth=3",
			want:  FaultRule{Syscall: "openat", Action: ActionDeny, Errno: syscall.ENOENT, Probability: 1.0, Trigger: TriggerNth, TriggerN: 3},
		},
		{
			input: "openat=ENOENT:100%:/data/*:after=5",
			want:  FaultRule{Syscall: "openat", Action: ActionDeny, Errno: syscall.ENOENT, Probability: 1.0, PathGlob: "/data/*", Trigger: TriggerAfter, TriggerN: 5},
		},
		{
			input: "connect=delay:200ms:100%:nth=1",
			want:  FaultRule{Syscall: "connect", Action: ActionDelay, Delay: 200 * time.Millisecond, Probability: 1.0, Trigger: TriggerNth, TriggerN: 1},
		},
		{
			input:   "fsync=EIO:100%:nth=0",
			wantErr: true,
		},
		{
			input:   "fsync=EIO:100%:nth=abc",
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
			if got.Syscall != tt.want.Syscall ||
				got.Action != tt.want.Action ||
				got.Errno != tt.want.Errno ||
				got.Delay != tt.want.Delay ||
				got.Probability != tt.want.Probability ||
				got.PathGlob != tt.want.PathGlob ||
				got.Trigger != tt.want.Trigger ||
				got.TriggerN != tt.want.TriggerN {
				t.Errorf("ParseFaultRule(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestFaultRuleString(t *testing.T) {
	// Deny rule
	r := FaultRule{Syscall: "open", Action: ActionDeny, Errno: syscall.ENOENT, Probability: 0.5}
	got := r.String()
	want := "open=ENOENT:50%"
	if got != want {
		t.Errorf("FaultRule.String() = %q, want %q", got, want)
	}

	// With path glob
	r2 := FaultRule{Syscall: "openat", Action: ActionDeny, Errno: syscall.ENOENT, Probability: 1.0, PathGlob: "/data/*"}
	got2 := r2.String()
	want2 := "openat=ENOENT:100%:/data/*"
	if got2 != want2 {
		t.Errorf("FaultRule.String() = %q, want %q", got2, want2)
	}

	// Delay rule
	r3 := FaultRule{Syscall: "connect", Action: ActionDelay, Delay: 200 * time.Millisecond, Probability: 1.0}
	got3 := r3.String()
	want3 := "connect=delay:200ms:100%"
	if got3 != want3 {
		t.Errorf("FaultRule.String() = %q, want %q", got3, want3)
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

func TestShouldFire(t *testing.T) {
	// TriggerAlways — always fires
	r1 := &FaultRule{Trigger: TriggerAlways}
	for i := 0; i < 5; i++ {
		if !r1.ShouldFire() {
			t.Errorf("TriggerAlways: call %d should fire", i+1)
		}
	}

	// TriggerNth — fires only on Nth call
	r2 := &FaultRule{Trigger: TriggerNth, TriggerN: 3}
	results := make([]bool, 5)
	for i := 0; i < 5; i++ {
		results[i] = r2.ShouldFire()
	}
	expected := []bool{false, false, true, false, false}
	for i, want := range expected {
		if results[i] != want {
			t.Errorf("TriggerNth(3): call %d = %v, want %v", i+1, results[i], want)
		}
	}

	// TriggerAfter — fires after N calls succeed
	r3 := &FaultRule{Trigger: TriggerAfter, TriggerN: 2}
	results = make([]bool, 5)
	for i := 0; i < 5; i++ {
		results[i] = r3.ShouldFire()
	}
	expected = []bool{false, false, true, true, true}
	for i, want := range expected {
		if results[i] != want {
			t.Errorf("TriggerAfter(2): call %d = %v, want %v", i+1, results[i], want)
		}
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
