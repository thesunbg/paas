package server

import (
	"context"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/templates"
)

// A template that works without requests (n8n) does not sleep in a batch unless asked;
// batches are restricted unless told otherwise.
func TestBatchDefaultsFollowTemplate(t *testing.T) {
	n8n, ok := templates.ByID("n8n")
	if !ok || !n8n.KeepAwake() {
		t.Fatal("the n8n template should keep awake")
	}
	in := batchInput{}
	in.applyDefaults(n8n)
	if in.Sleep == nil || *in.Sleep || in.Restricted == nil || !*in.Restricted {
		t.Fatalf("defaults: sleep %v restricted %v", in.Sleep, in.Restricted)
	}
	if o := in.installOptions(); o.Sleep.Enabled || o.SleepExplicit || !o.Restricted {
		t.Fatalf("install options: %+v", o)
	}
	on, off := true, false
	in = batchInput{Sleep: &on, Restricted: &off}
	in.applyDefaults(n8n)
	if o := in.installOptions(); !o.Sleep.Enabled || !o.SleepExplicit || o.Restricted {
		t.Fatalf("explicit choices lost: %+v", o)
	}
	// Any other template keeps sleep on by default.
	in = batchInput{}
	in.applyDefaults(&templates.Template{ID: "x", Services: []templates.Service{{Name: "site", Image: "nginx", Port: 80}}})
	if in.Sleep != nil {
		t.Fatalf("sleep forced for a plain template: %v", *in.Sleep)
	}
}

func TestBatchEstimateGuaranteed(t *testing.T) {
	tpl := &templates.Template{ID: "x", Services: []templates.Service{{Name: "app", Image: "nginx", Port: 80, Size: "small"}}}
	burst := computeBatchEstimate(tpl, 1, 1, false, 100000, 100000)
	in := batchInput{QoS: model.QoSGuaranteed, Size: "medium"}
	g := computeBatchEstimate(in.sized(tpl), 1, 1, false, 100000, 100000)
	if g.PerCopyRequestMB != g.PerCopyLimitMB || g.PerCopyRequestMB <= burst.PerCopyRequestMB {
		t.Fatalf("guaranteed medium: request %d limit %d (burstable small %d)", g.PerCopyRequestMB, g.PerCopyLimitMB, burst.PerCopyRequestMB)
	}
	if tpl.Services[0].Size != "small" || tpl.Services[0].QoS != "" {
		t.Fatal("sized changed the shared template")
	}
	if validInstallSize("huge", "") == nil || validInstallSize("", "gold") == nil || validInstallSize("medium", model.QoSGuaranteed) != nil {
		t.Fatal("validInstallSize")
	}
}

func TestInstallOptions(t *testing.T) {
	s := newTestServerBus(t)
	ctx := context.Background()
	team, err := s.store.CreateTeam(ctx, "T", "t", "", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.store.CreateProject(ctx, team.ID, "class", "class")
	if err != nil {
		t.Fatal(err)
	}
	tpl := &templates.Template{ID: "aw", Services: []templates.Service{{
		Name: "flow", Image: "nginx", Port: 80, Size: "small", KeepAwake: true, VolumeBackupHours: 12,
		Volumes:    []model.Volume{{Name: "data", Path: "/data"}},
		Env:        map[string]string{"A": "1"},
		Restricted: map[string]string{"LOCK": "on"},
		Inputs:     []templates.EnvInput{{Key: "LOCK"}},
	}}}
	sleep := model.SleepPolicy{Enabled: true, IdleMinutes: 30}
	// Sleep not asked for explicitly: a KeepAwake service stays awake. A learner's input cannot lift the restriction.
	svcs, _, err := s.installTemplateWith(ctx, p, tpl, "", "", map[string]map[string]string{"flow": {"LOCK": "off"}},
		installOptions{Sleep: sleep, Size: "medium", QoS: model.QoSGuaranteed, Restricted: true})
	if err != nil {
		t.Fatal(err)
	}
	spec := svcs[0].Spec
	if spec.Sleep.Enabled {
		t.Fatal("a keep-awake service sleeps")
	}
	if spec.QoS != model.QoSGuaranteed || spec.Resources.MemoryRequestMB != spec.Resources.MemoryLimitMB || spec.Resources.MemoryLimitMB <= 512 {
		t.Fatalf("size/qos not applied: %+v %+v", spec.QoS, spec.Resources)
	}
	if spec.VolumeBackupHours != 12 {
		t.Fatalf("volume backup hours %d", spec.VolumeBackupHours)
	}
	vars, _ := s.store.EnvVars(ctx, svcs[0].ID)
	got := map[string]string{}
	for _, v := range vars {
		got[v.Key], _ = s.box.Decrypt(v.ValueEnc)
	}
	if got["LOCK"] != "on" || got["A"] != "1" {
		t.Fatalf("env %v", got)
	}
	// Asked for explicitly, it sleeps; unrestricted, the restricted env is not added.
	svcs, _, err = s.installTemplateWith(ctx, p, tpl, "", "", nil, installOptions{Sleep: sleep, SleepExplicit: true})
	if err != nil {
		t.Fatal(err)
	}
	if !svcs[0].Spec.Sleep.Enabled || svcs[0].Spec.QoS != model.QoSBurstable {
		t.Fatalf("explicit sleep / default qos: %+v %s", svcs[0].Spec.Sleep, svcs[0].Spec.QoS)
	}
	vars, _ = s.store.EnvVars(ctx, svcs[0].ID)
	for _, v := range vars {
		if v.Key == "LOCK" {
			t.Fatal("restricted env on an unrestricted install")
		}
	}
	// A learner's team is held to its copy.
	s.holdToCopy(ctx, team.ID, 1, 0)
	if err := s.checkQuota(ctx, team.ID, TeamUsage{Services: 1}); err == nil {
		t.Fatal("a held team can still add a service")
	}
}

// Instances mode without a project gives each copy a project of its own.
func TestBatchInstancesOneProjectPerCopy(t *testing.T) {
	oldPoll := batchPoll
	batchPoll = 20 * time.Millisecond
	t.Cleanup(func() { batchPoll = oldPoll })
	s := newTestServerBus(t)
	h := s.routes()
	ctx := context.Background()
	userWithTeam(t, s, "admin@example.com", true)
	tok := mustLogin(t, h, "admin@example.com", "password1")
	tpl := plainTemplate(t)
	w := do(t, h, "POST", "/api/v1/batches", tok, map[string]any{"name": "class", "template": tpl.ID, "mode": "instances",
		"count": 2, "parallel": 2, "sleep": false, "force": true})
	if w.Code != 202 {
		t.Fatalf("POST batches = %d %s", w.Code, w.Body.String())
	}
	var batchID string
	var shared *string
	_ = s.store.Q(ctx).QueryRow(ctx, `SELECT id, project_id FROM batches`).Scan(&batchID, &shared)
	if shared != nil {
		t.Fatal("the batch has a shared project")
	}
	deadline := time.Now().Add(10 * time.Second)
	var n, distinct int
	for time.Now().Before(deadline) {
		_ = s.store.Q(ctx).QueryRow(ctx, `SELECT count(service_id), count(DISTINCT project_id) FROM batch_items WHERE batch_id=$1`, batchID).Scan(&n, &distinct)
		if n == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n != 2 || distinct != 2 {
		t.Fatalf("%d copies in %d projects", n, distinct)
	}
}
