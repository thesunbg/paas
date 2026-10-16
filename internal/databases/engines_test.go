package databases

import "testing"

func TestParseStats(t *testing.T) {
	for in, want := range map[string][2]int64{
		"8123456 3\n":            {8123456, 3},
		"8123456\t3\n":           {8123456, 3},
		"Warning: x\n1048576 12": {1048576, 12},
	} {
		size, conns, err := ParseStats(in)
		if err != nil || size != want[0] || int64(conns) != want[1] {
			t.Errorf("%q: got %d %d %v", in, size, conns, err)
		}
	}
	if _, _, err := ParseStats(""); err == nil {
		t.Error("empty output must fail")
	}
}
