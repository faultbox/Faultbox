package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/faultbox/Faultbox/internal/doctor"
)

func doctorCmd(args []string) int { return runDoctor(args, os.Stdout, os.Stderr) }
func runDoctor(args []string, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "text", "text or json")
	lima := fs.String("lima", "", "inspect an existing Lima instance (does not start it)")
	docker := fs.Bool("docker", false, "check Docker and matching shim")
	packet := fs.Bool("packet", false, "check TUN and CAP_NET_ADMIN")
	trace := fs.Bool("trace", false, "check filesystem observation prerequisites")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 || (*format != "text" && *format != "json") {
		fmt.Fprintln(stderr, "doctor: use --format=text|json and no positional arguments")
		return 1
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if path, err := filepath.EvalSymlinks(self); err == nil {
		self = path
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report := doctor.Run(ctx, doctor.Options{Version: version, Executable: self, Lima: *lima, Docker: *docker, Packet: *packet, Trace: *trace})
	if *format == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return 1
		}
	} else {
		fmt.Fprintf(out, "Faultbox doctor — %s (%s)\n", report.Version, report.Platform)
		for _, c := range report.Checks {
			fmt.Fprintf(out, "%-5s %-22s %s\n", c.Status, c.Code, c.Message)
			if c.Remedy != "" {
				fmt.Fprintf(out, "      %s\n", c.Remedy)
			}
		}
	}
	if !report.OK() {
		return 2
	}
	return 0
}
