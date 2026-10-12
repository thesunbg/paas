package resources

import (
	"testing"

	"github.com/thesunbg/paas/internal/model"
)

func TestNormalizeDefaults(t *testing.T) {
	spec := model.ServiceSpec{}
	if err := Normalize(model.TypeWeb, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Replicas != 1 || spec.Port != 8080 || spec.QoS != model.QoSBurstable || spec.Resources.MemoryLimitMB != 512 {
		t.Fatalf("unexpected defaults %+v", spec)
	}
	db := model.ServiceSpec{}
	_ = Normalize(model.TypeDatabase, &db)
	if db.QoS != model.QoSGuaranteed || db.Resources.MemoryRequestMB != db.Resources.MemoryLimitMB {
		t.Fatalf("database must be guaranteed: %+v", db)
	}
}

func TestClassOf(t *testing.T) {
	if ClassOf(model.Resources{MemoryLimitMB: 256}) != model.QoSBestEffort {
		t.Fatal("no requests -> best-effort")
	}
	if ClassOf(model.Resources{MemoryLimitMB: 256, MemoryRequestMB: 256}) != model.QoSGuaranteed {
		t.Fatal("request==limit -> guaranteed")
	}
	if ClassOf(model.Resources{MemoryLimitMB: 256, MemoryRequestMB: 64}) != model.QoSBurstable {
		t.Fatal("request<limit -> burstable")
	}
}

func TestTuningEnv(t *testing.T) {
	r := model.Resources{MemoryLimitMB: 512, CPULimitMillis: 1500}
	if got := TuningEnv("node", r)["NODE_OPTIONS"]; got != "--max-old-space-size=384" {
		t.Fatal(got)
	}
	g := TuningEnv("go", r)
	if g["GOMEMLIMIT"] != "460MiB" || g["GOMAXPROCS"] != "2" {
		t.Fatal(g)
	}
	if w := TuningEnv("python", r)["WEB_CONCURRENCY"]; w != "4" {
		t.Fatal(w)
	}
}

func TestRecommend(t *testing.T) {
	rec := Recommend(model.Resources{MemoryLimitMB: 512}, 80)
	if rec == nil || rec.SuggestedLimitMB != 128 || rec.SavingsMB != 384 {
		t.Fatalf("%+v", rec)
	}
	if Recommend(model.Resources{MemoryLimitMB: 512}, 300) != nil {
		t.Fatal("well sized service should have no recommendation")
	}
	if rec := Recommend(model.Resources{MemoryLimitMB: 256}, 250); rec == nil || rec.SuggestedLimitMB != 384 {
		t.Fatalf("%+v", rec)
	}
}
