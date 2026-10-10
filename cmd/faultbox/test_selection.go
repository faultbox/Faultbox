package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// One exact discovered name per line. Blank lines and full-line comments are
// accepted; regex, shorthand and matrix groups are not expanded in this mode.
func readTestList(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--tests-from: %w", err)
	}
	defer file.Close()
	result := []string{}
	seen := map[string]bool{}
	scan := bufio.NewScanner(file)
	scan.Buffer(make([]byte, 4096), 1024*1024)
	for line := 1; scan.Scan(); line++ {
		value := strings.TrimSpace(scan.Text())
		if line == 1 {
			value = strings.TrimSpace(strings.TrimPrefix(value, "\uFEFF"))
		}
		if value == "" || strings.HasPrefix(value, "#") {
			continue
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("--tests-from %s:%d: expected one exact test name", path, line)
		}
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("--tests-from %s: %w", path, err)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("--tests-from %s: no test names", path)
	}
	return result, nil
}
