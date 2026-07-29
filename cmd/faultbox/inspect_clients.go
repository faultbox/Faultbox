package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/faultbox/Faultbox/internal/logging"
	"github.com/faultbox/Faultbox/internal/star"
)

// printSpecClients implements `faultbox inspect --clients <spec.star>`
// (RFC-055 Phase 4).
//
// A generated client's whole value proposition is that you don't have to
// read the contract to call the API — which only holds if there's a way to
// see what the generated surface actually is. This is that way, and it's
// what the "no operation X" error points users at.
func printSpecClients(w io.Writer, specFile string) int {
	logger := logging.New(logging.Config{Level: slog.LevelWarn})
	rt := star.New(logger)
	if err := rt.LoadFile(specFile); err != nil {
		fmt.Fprintf(os.Stderr, "error loading %s: %v\n", specFile, err)
		return 1
	}

	names := rt.ClientNames()
	if len(names) == 0 {
		fmt.Fprintf(w, "%s declares no clients.\n\n", specFile)
		fmt.Fprintln(w, "Declare one with:")
		fmt.Fprintln(w, `  api = client("mobile-app", target = orders.public, openapi = "./orders.yaml")`)
		return 0
	}

	for i, name := range names {
		c, ok := rt.Client(name)
		if !ok {
			continue
		}
		if i > 0 {
			fmt.Fprintln(w)
		}
		printOneClient(w, c)
	}
	return 0
}

func printOneClient(w io.Writer, c *star.ClientVal) {
	fmt.Fprintf(w, "%s\n", c.Name)
	fmt.Fprintf(w, "  target:    %s.%s (%s)\n",
		c.Target.Service.Name, c.Target.Interface.Name, c.Target.Interface.Protocol)
	fmt.Fprintf(w, "  contract:  %s\n", c.Table.Contract.String())
	fmt.Fprintf(w, "  validate:  %s\n", c.Validate)
	if c.BasePath != "" {
		fmt.Fprintf(w, "  base_path: %s\n", c.BasePath)
	}
	if len(c.Headers) > 0 {
		keys := make([]string, 0, len(c.Headers))
		for k := range c.Headers {
			keys = append(keys, k)
		}
		fmt.Fprintf(w, "  headers:   %s\n", strings.Join(sortedStrings(keys), ", "))
	}

	ops := c.Table.Operations()
	fmt.Fprintf(w, "  operations (%d):\n", len(ops))

	// Two columns: the call you'd write, and the wire target it maps to.
	// Width is computed rather than fixed so a long operation name doesn't
	// push the wire column off into the weeds.
	width := 0
	for _, op := range ops {
		if n := len(op.SignatureHint()); n > width {
			width = n
		}
	}
	if width > 56 {
		width = 56
	}
	for _, op := range ops {
		sig := op.SignatureHint()
		pad := width - len(sig)
		if pad < 1 {
			pad = 1
		}
		fmt.Fprintf(w, "    %s%s  %s\n", sig, strings.Repeat(" ", pad), op.Wire())
	}
}

func sortedStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
