package runtime

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// ReadNetDev sums the received and sent bytes in a /proc/.../net/dev file over
// the interfaces keep accepts (all but loopback when keep is nil).
func ReadNetDev(path string, keep func(iface string) bool) (rx, tx uint64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue // the two header lines
		}
		name = strings.TrimSpace(name)
		if name == "lo" || (keep != nil && !keep(name)) {
			continue
		}
		// receive: bytes packets errs drop fifo frame compressed multicast | transmit: bytes …
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(fields[0], 10, 64)
		t, _ := strconv.ParseUint(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx, true
}
