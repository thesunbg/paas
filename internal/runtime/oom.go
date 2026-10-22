package runtime

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// oomKills is how many times the kernel killed a process of the container for
// exceeding its memory limit (its cgroup's oom_kill counter; 0 when unknown).
// Containers live in the /paas/<id> cgroup.
func oomKills(id string) int {
	for _, f := range []string{
		filepath.Join("/sys/fs/cgroup/paas", id, "memory.events"),             // cgroup v2
		filepath.Join("/sys/fs/cgroup/memory/paas", id, "memory.oom_control"), // cgroup v1
	} {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), " "); ok && k == "oom_kill" {
				n, _ := strconv.Atoi(strings.TrimSpace(v))
				fh.Close()
				return n
			}
		}
		fh.Close()
	}
	return 0
}
