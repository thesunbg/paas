package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadNetDev(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dev")
	_ = os.WriteFile(p, []byte(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  900       9    0    0    0     0          0         0      900       9    0    0    0     0       0          0
  eth0: 1000      10    0    0    0     0          0         0     2000      20    0    0    0     0       0          0
 veth1:  500       5    0    0    0     0          0         0      700       7    0    0    0     0       0          0
`), 0o644)
	if rx, tx, ok := ReadNetDev(p, nil); !ok || rx != 1500 || tx != 2700 {
		t.Fatalf("all: %d %d %v", rx, tx, ok)
	}
	if rx, tx, _ := ReadNetDev(p, func(i string) bool { return i == "eth0" }); rx != 1000 || tx != 2000 {
		t.Fatalf("eth0: %d %d", rx, tx)
	}
}
