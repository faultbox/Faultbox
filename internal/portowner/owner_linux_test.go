package portowner

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestLookupListenerDoesNotExposeArguments(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(port)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	owners, err := Lookup(ctx, "tcp", "127.0.0.1", n)
	for _, owner := range owners {
		if owner.PID == os.Getpid() {
			if owner.Start != Start(os.Getpid()) || owner.Command == "" {
				t.Fatal(owner)
			}
			return
		}
	}
	t.Fatalf("own listener missing: owners=%v partial=%v", owners, err)
}
func TestLookupHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Lookup(ctx, "tcp", "127.0.0.1", 1); err == nil {
		t.Fatal("expected incomplete observation")
	}
}
