package connowner

import (
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestOwnershipChildHelper(t *testing.T) {
	addr := os.Getenv("FAULTBOX_CONNOWNER_TEST_ADDRESS")
	if addr == "" {
		t.Skip("subprocess helper")
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestLiveManagedChildOwnershipDirectAndForwarded(t *testing.T) {
	for _, bindAddress := range []string{"127.0.0.1:0", "[::1]:0"} {
		t.Run(bindAddress, func(t *testing.T) {
			front, err := net.Listen("tcp", bindAddress)
			if err != nil {
				t.Skipf("loopback unavailable: %v", err)
			}
			defer front.Close()
			front.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
			cmd := exec.Command(os.Args[0], "-test.run=^TestOwnershipChildHelper$")
			cmd.Env = append(os.Environ(), "FAULTBOX_CONNOWNER_TEST_ADDRESS="+front.Addr().String())
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
			client, err := front.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			tracker := New()
			direct := tracker.Bind(client.RemoteAddr(), client.LocalAddr())
			defer direct.Close()
			if direct.Source().Known() {
				t.Fatal("unregistered client was attributed")
			}
			undo, err := tracker.Register("managed-child", cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			defer undo()
			source := direct.Source()
			if !source.Known() || source.Service != "managed-child" || source.PID != cmd.Process.Pid || !tracker.IsActive(source) {
				t.Fatalf("direct source=%+v", source)
			}

			back, err := net.Listen("tcp", bindAddress)
			if err != nil {
				t.Fatal(err)
			}
			defer back.Close()
			back.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
			upstream, err := net.DialTimeout("tcp", back.Addr().String(), 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer upstream.Close()
			server, err := back.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			observed := tracker.Bind(server.RemoteAddr(), server.LocalAddr())
			defer observed.Close()
			if observed.Source().Known() {
				t.Fatal("the unregistered proxy process was inferred to be the child")
			}
			cleanup := tracker.Forward(client, upstream)
			defer cleanup()
			if got := observed.Source(); got != source {
				t.Fatalf("forwarded source=%+v, want %+v", got, source)
			}
			undo()
			if tracker.IsActive(source) {
				t.Fatal("unregistered child remains active")
			}
			observed.Close()
			if observed.Source() != source {
				t.Fatal("lost historical proven source")
			}
		})
	}
}
