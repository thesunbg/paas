package server

import (
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/proto"
)

// Moving any constant out of its proven relation must turn this red.
func TestHAConstantsKeepTheTimingProof(t *testing.T) {
	if haPrimaryGone < proto.LeaseTTL+25*time.Second {
		t.Errorf("haPrimaryGone %v < LeaseTTL %v + 25s", haPrimaryGone, proto.LeaseTTL)
	}
	if haWitnessDown < proto.WalSenderTimeout+proto.FenceCheckEvery+10*time.Second {
		t.Errorf("haWitnessDown %v < WalSenderTimeout %v + FenceCheckEvery %v + 10s", haWitnessDown, proto.WalSenderTimeout, proto.FenceCheckEvery)
	}
	if haLeaderGrace <= haWitnessDown {
		t.Errorf("haLeaderGrace %v must exceed haWitnessDown %v (witness state dies with the leader)", haLeaderGrace, haWitnessDown)
	}
}

func TestFailoverVerdict(t *testing.T) {
	const MiB = 1 << 20
	ok := haInput{
		sinceLeader: 5 * time.Minute, nodesTotal: 2, nodesOffline: 1, primaryLastSeenAge: 60 * time.Second,
		witness:    haWitness{ok: true, streaming: false, downFor: 40 * time.Second},
		standbyLSN: 100 * MiB, primaryLSN: 100 * MiB,
	}
	with := func(f func(*haInput)) haInput { in := ok; f(&in); return in }
	tests := []struct {
		name string
		in   haInput
		want string
	}{
		{"all conditions met", ok, verdictPromote},
		{"within leader grace", with(func(i *haInput) { i.sinceLeader = 30 * time.Second }), verdictWait},
		{"2 of 3 nodes offline", with(func(i *haInput) { i.nodesTotal, i.nodesOffline = 3, 2 }), verdictWait},
		{"1 of 2 nodes offline is allowed", with(func(i *haInput) { i.nodesTotal, i.nodesOffline = 2, 1 }), verdictPromote},
		{"primary last seen 40s ago", with(func(i *haInput) { i.primaryLastSeenAge = 40 * time.Second }), verdictWait},
		{"primary last seen exactly 45s ago", with(func(i *haInput) { i.primaryLastSeenAge = haPrimaryGone }), verdictWait},
		{"witness exec failed", with(func(i *haInput) { i.witness = haWitness{ok: false} }), verdictWait},
		{"witness still streaming", with(func(i *haInput) { i.witness = haWitness{ok: true, streaming: true} }), verdictWait},
		{"witness down 20s", with(func(i *haInput) { i.witness.downFor = 20 * time.Second }), verdictWait},
		{"lag 20 MiB", with(func(i *haInput) { i.primaryLSN = i.standbyLSN + 20*MiB }), verdictBlockedLag},
		{"lag 20 MiB with force", with(func(i *haInput) { i.primaryLSN, i.force = i.standbyLSN+20*MiB, true }), verdictPromote},
		{"lag 16 MiB is within the limit", with(func(i *haInput) { i.primaryLSN = i.standbyLSN + haMaxLagBytes }), verdictPromote},
		{"standby ahead of the recorded LSN", with(func(i *haInput) { i.standbyLSN = i.primaryLSN + 50*MiB }), verdictPromote},
		{"force inside leader grace", with(func(i *haInput) { i.sinceLeader, i.force = time.Second, true }), verdictWait},
		{"force while the witness streams", with(func(i *haInput) { i.witness.streaming, i.force = true, true }), verdictWait},
		{"force while the primary is recent", with(func(i *haInput) { i.primaryLastSeenAge, i.force = time.Second, true }), verdictWait},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := failoverVerdict(tc.in)
			if got != tc.want {
				t.Errorf("verdict = %s (%s), want %s", got, reason, tc.want)
			}
			if reason == "" {
				t.Error("empty reason")
			}
		})
	}
}

func TestParseLSN(t *testing.T) {
	for in, want := range map[string]uint64{"0/0": 0, "0/16B3748": 0x16B3748, "1/0": 1 << 32, "A/FF": 10<<32 | 0xFF} {
		if got, ok := parseLSN(in); !ok || got != want {
			t.Errorf("parseLSN(%q) = %x, %v; want %x", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "garbage", "0/zz"} {
		if _, ok := parseLSN(bad); ok {
			t.Errorf("parseLSN(%q) accepted", bad)
		}
	}
}
