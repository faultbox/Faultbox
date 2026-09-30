package main

import (
	"bytes"
	"encoding/json"
	"github.com/faultbox/Faultbox/internal/doctor"
	"testing"
)

func TestDoctorCLIJSONAndFlags(t *testing.T) {
	var out, errout bytes.Buffer
	code := runDoctor([]string{"--format=json"}, &out, &errout)
	if code != 0 && code != 2 {
		t.Fatalf("code=%d stderr=%s", code, errout.String())
	}
	var report doctor.Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Version != version || len(report.Checks) == 0 {
		t.Fatal(report, err)
	}
	for _, args := range [][]string{{"--format=yaml"}, {"unexpected"}, {"--unknown"}} {
		out.Reset()
		errout.Reset()
		if code := runDoctor(args, &out, &errout); code != 1 {
			t.Fatalf("args=%v code=%d", args, code)
		}
	}
}
