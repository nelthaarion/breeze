//go:build linux

package breeze

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// hasTimeWaitOn reports whether a connection whose local port is port is in TIME_WAIT.
func hasTimeWaitOn(port int) bool {
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) > 3 && fields[3] == "06" && strings.HasSuffix(fields[1], fmt.Sprintf(":%04X", port)) {
				f.Close()
				return true
			}
		}
		f.Close()
	}
	return false
}
