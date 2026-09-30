package star

import (
	"fmt"
	"os"

	"github.com/faultbox/Faultbox/internal/seccomp"
)

// Runtime integration tests re-exec this test binary when launching native
// services. Dispatch the launch shim before testing.Main, just as the CLI does,
// so a service launch cannot recursively run the entire test suite.
func init() {
	if seccomp.IsShimChild() {
		if err := seccomp.RunShimChild(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}
