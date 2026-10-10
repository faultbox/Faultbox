package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadTestList(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
		bad  bool
	}{
		{"\uFEFF  # rerun\r\n test_z \r\ntest_a\ntest_z\n", []string{"test_z", "test_a"}, false},
		{"# empty\n\n", nil, true}, {"test_name with spaces", []string{"test_name with spaces"}, false}, {"test_a\x00", nil, true},
	} {
		p := filepath.Join(t.TempDir(), "tests.txt")
		if err := os.WriteFile(p, []byte(tc.text), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := readTestList(p)
		if (err != nil) != tc.bad || !tc.bad && !reflect.DeepEqual(got, tc.want) {
			t.Fatal(got, err)
		}
	}
}
func TestTestsFromCLISelectionAndUnknownName(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.star")
	list := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(spec, []byte("def test_ok(): assert_true(True)\ndef test_unselected(): fail(\"must not run\")\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(list, []byte("test_ok\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := testCmd([]string{spec, "--tests-from", list, "--seed=42", "--no-bundle"}); code != 0 {
		t.Fatalf("selected pass code=%d", code)
	}
	if err := os.WriteFile(list, []byte("test_missing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := testCmd([]string{spec, "--tests-from=" + list, "--seed=42", "--no-bundle"}); code != 1 {
		t.Fatalf("unknown code=%d", code)
	}
	if code := testCmd([]string{spec, "--tests-from", list, "--test=test_ok", "--seed=42", "--no-bundle"}); code != 1 {
		t.Fatalf("conflicting selection code=%d", code)
	}
	if code := testCmd([]string{spec, "--tests-from", "", "--seed=42", "--no-bundle"}); code != 1 {
		t.Fatalf("empty path code=%d", code)
	}
}
