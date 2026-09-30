package doctor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func testProbe() probe {
	return probe{os: "linux", arch: "arm64", read: func(path string) ([]byte, error) {
		if strings.HasSuffix(path, "actions_avail") {
			return []byte("kill_process allow user_notif"), nil
		}
		return []byte("CapEff:\t0000000000000000\n"), nil
	}, stat: func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }, command: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("not available") }}
}
func hasCode(r Report, code, status string) bool {
	for _, c := range r.Checks {
		if c.Code == code && c.Status == status {
			return true
		}
	}
	return false
}
func TestDoctorRequiresOnlySelectedCapabilities(t *testing.T) {
	p := testProbe()
	calls := 0
	p.command = func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, errors.New("offline") }
	r := run(context.Background(), Options{Version: "1.0", Executable: "/bin/faultbox"}, p)
	if !r.OK() || calls != 0 {
		t.Fatal(r, calls)
	}
	r = run(context.Background(), Options{Docker: true, Packet: true}, p)
	for _, code := range []string{"DOCKER", "SHIM", "TUN", "CAP_NET_ADMIN"} {
		if !hasCode(r, code, "error") {
			t.Fatal(code, r)
		}
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	p.os = "darwin"
	r = run(context.Background(), Options{}, p)
	if r.OK() || !hasCode(r, "LINUX_RUNNER", "error") {
		t.Fatal(r)
	}
}
func TestDoctorLimaVersionAndCapabilityMismatch(t *testing.T) {
	p := testProbe()
	p.os = "darwin"
	p.command = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "limactl" || strings.Join(args, " ") != "shell --workdir /tmp faultbox faultbox doctor --format=json --docker" {
			t.Fatal(name, args)
		}
		return []byte(`{"version":"0.18.2","platform":"linux/arm64","checks":[{"code":"DOCKER","status":"error","message":"unavailable"}]}`), errors.New("exit 2")
	}
	r := run(context.Background(), Options{Version: "0.18.3", Lima: "faultbox", Docker: true}, p)
	if r.OK() || !hasCode(r, "LIMA_VERSION", "error") || !hasCode(r, "LIMA_DOCKER", "error") {
		t.Fatal(r)
	}
	p.command = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("unsafe name invoked command")
		return nil, nil
	}
	r = run(context.Background(), Options{Lima: "-bad"}, p)
	if !hasCode(r, "LIMA_NAME", "error") {
		t.Fatal(r)
	}
}
func TestDoctorUnavailableGuestIsNotSuccess(t *testing.T) {
	p := testProbe()
	p.command = func(context.Context, string, ...string) ([]byte, error) { return []byte("unknown command doctor"), nil }
	r := run(context.Background(), Options{Lima: "faultbox"}, p)
	if r.OK() {
		t.Fatal(r)
	}
}
func TestNetAdminCapability(t *testing.T) {
	if !hasNetAdmin("Name:\tdoctor\nCapEff:\t0000000000001000\n") || hasNetAdmin("CapEff:\t0000000000000000\n") {
		t.Fatal("capability parsing")
	}
}
func TestCommandCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := command(ctx, "sleep", "60"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
