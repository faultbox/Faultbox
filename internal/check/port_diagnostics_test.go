package check

import (
	"fmt"
	"testing"

	"github.com/faultbox/Faultbox/internal/star"
)

func TestCheckReportsHostEphemeralPortWarning(t *testing.T) {
	low, _, err := star.HostEphemeralPortRange()
	if err != nil {
		t.Skipf("host range unavailable: %v", err)
	}
	spec := fmt.Sprintf(`
svc = service("sut", "/bin/true", interface("main", "tcp", %d))
def test_ok():
    assert_true(True)
`, low)
	res := Run(write(t, "fixed.star", spec), -1)
	if !res.OK {
		t.Fatalf("warning should not fail check: %+v", res)
	}
	if len(res.Findings) != 1 || res.Findings[0].Code != "PORT_IN_EPHEMERAL_RANGE" || res.Findings[0].Level != "warning" {
		t.Fatalf("findings: %+v", res.Findings)
	}
}
