package generator

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// currentModulePath reads the module path out of go.mod in the current
// working directory. It returns an error that's safe to print directly to
// the user if go.mod is missing or malformed â€” generate commands need a
// Breeze project to work inside of.
func currentModulePath() (string, error) {
	f, err := os.Open("go.mod")
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf(
				"no go.mod found in current directory â€” run this from the root of a Breeze project",
			)
		}
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "module"))
			if idx := strings.Index(path, "//"); idx != -1 {
				path = strings.TrimSpace(path[:idx])
			}
			path = strings.Trim(path, `"`)
			if path == "" {
				return "", fmt.Errorf("go.mod has a malformed module directive")
			}
			return path, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("go.mod has no module directive")
}
