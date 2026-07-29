//go:build linux

package gvisor

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/gvisor/seccheck"
)

// These need a real SOCK_SEQPACKET socket, which only Linux provides — macOS
// returns "protocol not supported". The rest of the package is deliberately
// testable without Linux (the decoder runs against a captured fixture), but
// binding behaviour cannot be faked without testing the fake instead.

func TestAcquireSinkBindsAndReleases(t *testing.T) {
	o := testOpts(t)
	o.SinkPath = filepath.Join(t.TempDir(), "seccheck.sock")
	if _, err := Apply(o); err != nil {
		t.Fatal(err)
	}

	sink, err := AcquireSink(o.TraceConfig, nil, nil)
	if err != nil {
		t.Fatalf("AcquireSink: %v", err)
	}
	if _, err := os.Stat(o.SinkPath); err != nil {
		t.Errorf("socket was not created at the configured path: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(o.SinkPath); !os.IsNotExist(err) {
		t.Errorf("socket outlived the run; the next run would see it as stale litter")
	}
}

// The important one. The sink path is fixed by host config, so every run binds
// the same address. Listen used to os.Remove it unconditionally, which let a
// second run silently steal the socket from a first — breaking the first run's
// sandbox connections and losing its observation with neither run noticing.
func TestSecondRunRefusesToStealALiveSink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seccheck.sock")

	first, err := seccheck.Listen(seccheck.Config{Path: path})
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer first.Close()

	_, err = seccheck.Listen(seccheck.Config{Path: path})
	if err == nil {
		t.Fatal("a second Listen on a live socket must fail, not steal it")
	}
	for _, want := range []string{"already in use", "another Faultbox run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name the cause, not just fail to bind; want %q in: %v", want, err)
		}
	}

	// And the first sink must still work — the failed attempt must not have
	// removed its socket on the way out.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the live socket was removed by the failed attempt: %v", err)
	}
	if first.Path() != path {
		t.Errorf("first sink lost its path")
	}
}

// A socket left by a crashed run is litter, not an owner. It must be cleared,
// or one crash would make the feature unusable until someone deleted a file by
// hand — the same shape as the leaked TUN device v0.14.1 fixed.
func TestStaleSocketIsReclaimed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seccheck.sock")

	// Simulate a crash faithfully: bind, then drop the listener WITHOUT
	// unlinking, which is what a killed process leaves behind. Done with the
	// net package directly rather than by adding a "close but do not clean up"
	// method to Sink — production API should not exist to serve a test.
	ln, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("simulating a crash: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the crash simulation did not leave a socket behind: %v", err)
	}

	second, err := seccheck.Listen(seccheck.Config{Path: path})
	if err != nil {
		t.Fatalf("a stale socket must be reclaimed, not treated as an owner: %v", err)
	}
	second.Close()
}

func TestReportSnapshotsCounters(t *testing.T) {
	if got := Report(nil); got.Points != 0 || got.Dropped != 0 {
		t.Errorf("Report(nil) should be zero, got %+v", got)
	}
	path := filepath.Join(t.TempDir(), "s.sock")
	s, err := seccheck.Listen(seccheck.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := Report(s); got.Points != 0 || got.Dropped != 0 {
		t.Errorf("a fresh sink should report zero, got %+v", got)
	}
}
