package server

import "testing"

func TestNetDelta(t *testing.T) {
	s := &Server{}
	if d := s.netDelta("a", netCounters{100, 50}, false); d != (netCounters{}) {
		t.Fatalf("first sample of an old counter must count nothing, got %v", d)
	}
	if d := s.netDelta("a", netCounters{160, 80}, false); d != (netCounters{60, 30}) {
		t.Fatalf("growth: %v", d)
	}
	if d := s.netDelta("a", netCounters{10, 5}, false); d != (netCounters{10, 5}) {
		t.Fatalf("counter reset counts from zero: %v", d)
	}
	if d := s.netDelta("b", netCounters{70, 7}, true); d != (netCounters{70, 7}) {
		t.Fatalf("a fresh instance counts its whole counter: %v", d)
	}
	s.forgetNet(map[string]bool{"b": true})
	if _, ok := s.netLast["a"]; ok {
		t.Fatal("a is gone and must be forgotten")
	}
}
