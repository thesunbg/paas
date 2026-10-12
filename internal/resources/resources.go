// Package resources implements the density features: instance sizes,
// request/limit defaults, QoS classes and runtime tuning per language.
package resources

import (
	"fmt"
	"strconv"

	"github.com/thesunbg/paas/internal/cron"
	"github.com/thesunbg/paas/internal/model"
)

// Size is a named instance size.
type Size struct {
	Name             string `json:"name"`
	MemoryLimitMB    int    `json:"memory_limit_mb"`
	CPULimitMillis   int    `json:"cpu_limit_millis"`
	MemoryRequestMB  int    `json:"memory_request_mb"`  // burstable request
	CPURequestMillis int    `json:"cpu_request_millis"` // burstable request
}

var Sizes = []Size{
	{"nano", 128, 250, 48, 25},
	{"micro", 256, 500, 96, 50},
	{"small", 512, 1000, 160, 100},
	{"medium", 1024, 2000, 384, 250},
	{"large", 2048, 4000, 768, 500},
	{"xlarge", 4096, 8000, 1536, 1000},
}

func SizeByName(name string) (Size, bool) {
	for _, s := range Sizes {
		if s.Name == name {
			return s, true
		}
	}
	return Size{}, false
}

// Apply fills the service spec's resources from a size and QoS class.
func Apply(spec *model.ServiceSpec, size Size, qos string) {
	spec.Resources.MemoryLimitMB = size.MemoryLimitMB
	spec.Resources.CPULimitMillis = size.CPULimitMillis
	switch qos {
	case model.QoSGuaranteed:
		spec.Resources.MemoryRequestMB = size.MemoryLimitMB
		spec.Resources.CPURequestMillis = size.CPULimitMillis / 2
	case model.QoSBestEffort:
		spec.Resources.MemoryRequestMB = 0
		spec.Resources.CPURequestMillis = 0
	default:
		qos = model.QoSBurstable
		spec.Resources.MemoryRequestMB = size.MemoryRequestMB
		spec.Resources.CPURequestMillis = size.CPURequestMillis
	}
	spec.QoS = qos
}

// Normalize validates and fills defaults on a spec for the given service type.
func Normalize(svcType string, spec *model.ServiceSpec) error {
	if spec.Replicas <= 0 && svcType != model.TypeStatic && svcType != model.TypeCron {
		spec.Replicas = 1
	}
	if spec.Replicas > 50 {
		return fmt.Errorf("replicas must be <= 50")
	}
	r := &spec.Resources
	if r.MemoryLimitMB == 0 {
		size, _ := SizeByName("small")
		if svcType == model.TypeDatabase {
			size, _ = SizeByName("micro")
		}
		qos := spec.QoS
		if qos == "" {
			qos = model.QoSBurstable
			if svcType == model.TypeDatabase {
				qos = model.QoSGuaranteed
			}
		}
		Apply(spec, size, qos)
	}
	if r.MemoryLimitMB < 32 {
		return fmt.Errorf("memory limit must be at least 32 MB")
	}
	if r.MemoryRequestMB > r.MemoryLimitMB {
		r.MemoryRequestMB = r.MemoryLimitMB
	}
	if r.CPULimitMillis > 0 && r.CPURequestMillis > r.CPULimitMillis {
		r.CPURequestMillis = r.CPULimitMillis
	}
	spec.QoS = ClassOf(*r)
	if spec.RuntimeClass == "" {
		spec.RuntimeClass = model.RuntimeStandard
	}
	if spec.RuntimeClass != model.RuntimeStandard && spec.RuntimeClass != model.RuntimeSandboxed {
		return fmt.Errorf("unknown runtime class %q", spec.RuntimeClass)
	}
	if spec.Sleep.Enabled && spec.Sleep.IdleMinutes <= 0 {
		spec.Sleep.IdleMinutes = 15
	}
	if svcType == model.TypeWeb && spec.Port == 0 {
		spec.Port = 8080
	}
	if svcType == model.TypeCron {
		spec.Replicas, spec.Port = 0, 0
		spec.Sleep = model.SleepPolicy{}
		spec.Autoscale = model.Autoscale{}
		if _, err := cron.Parse(spec.Schedule); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
	}
	if a := &spec.Autoscale; a.Enabled {
		if a.Min <= 0 {
			a.Min = 1
		}
		if a.Max < a.Min {
			a.Max = max(a.Min, 3)
		}
		if a.Max > 50 {
			return fmt.Errorf("autoscale max must be <= 50")
		}
		if a.TargetCPUPercent == 0 && a.TargetRPS == 0 {
			a.TargetCPUPercent = 70
		}
		if len(spec.Volumes) > 0 {
			return fmt.Errorf("services with volumes cannot autoscale")
		}
		spec.Replicas = min(max(spec.Replicas, a.Min), a.Max)
	}
	return nil
}

// ClassOf derives the QoS class from requests and limits.
func ClassOf(r model.Resources) string {
	switch {
	case r.MemoryRequestMB == 0 && r.CPURequestMillis == 0:
		return model.QoSBestEffort
	case r.MemoryRequestMB >= r.MemoryLimitMB && r.MemoryLimitMB > 0:
		return model.QoSGuaranteed
	default:
		return model.QoSBurstable
	}
}

// TuningEnv returns environment variables that size language runtimes to the
// container's memory/CPU limits. User-provided variables always win.
func TuningEnv(language string, r model.Resources) map[string]string {
	env := map[string]string{}
	mem := r.MemoryLimitMB
	cpus := 1
	if r.CPULimitMillis > 0 {
		cpus = (r.CPULimitMillis + 999) / 1000
	}
	if mem <= 0 {
		return env
	}
	switch language {
	case "node":
		env["NODE_OPTIONS"] = "--max-old-space-size=" + strconv.Itoa(mem*75/100)
	case "go":
		env["GOMEMLIMIT"] = strconv.Itoa(mem*90/100) + "MiB"
		env["GOMAXPROCS"] = strconv.Itoa(cpus)
	case "java":
		opts := "-XX:MaxRAMPercentage=75 -XX:+UseSerialGC -Xss512k -XX:+ExitOnOutOfMemoryError"
		if mem <= 512 {
			opts += " -XX:TieredStopAtLevel=1 -XX:ReservedCodeCacheSize=32m"
		}
		env["JAVA_TOOL_OPTIONS"] = opts
	case "python", "ruby":
		// Roughly one worker per ~120 MB, bounded by 2*CPU+1.
		workers := mem / 120
		if workers < 1 {
			workers = 1
		}
		if limit := 2*cpus + 1; workers > limit {
			workers = limit
		}
		env["WEB_CONCURRENCY"] = strconv.Itoa(workers)
		if language == "ruby" {
			env["RAILS_MAX_THREADS"] = "5"
		}
	case "php":
		env["PHP_MEMORY_LIMIT"] = strconv.Itoa(mem*80/100) + "M"
	}
	return env
}

// Recommendation is a right-sizing suggestion.
type Recommendation struct {
	CurrentLimitMB     int    `json:"current_limit_mb"`
	PeakMB             int    `json:"peak_mb"`
	SuggestedLimitMB   int    `json:"suggested_limit_mb"`
	SuggestedRequestMB int    `json:"suggested_request_mb"`
	SavingsMB          int    `json:"savings_mb"`
	Message            string `json:"message"`
}

// Recommend suggests memory settings from observed peak usage.
func Recommend(r model.Resources, peakMB int) *Recommendation {
	if peakMB <= 0 || r.MemoryLimitMB == 0 {
		return nil
	}
	suggested := roundUp(peakMB*16/10, 32) // 60% headroom
	if suggested < 64 {
		suggested = 64
	}
	rec := &Recommendation{CurrentLimitMB: r.MemoryLimitMB, PeakMB: peakMB, SuggestedLimitMB: suggested, SuggestedRequestMB: roundUp(peakMB*11/10, 16)}
	switch {
	case peakMB > r.MemoryLimitMB*90/100:
		rec.Message = fmt.Sprintf("Peak usage %d MB is close to the %d MB limit; consider raising it to %d MB.", peakMB, r.MemoryLimitMB, roundUp(r.MemoryLimitMB*3/2, 64))
		rec.SuggestedLimitMB = roundUp(r.MemoryLimitMB*3/2, 64)
	case suggested < r.MemoryLimitMB*7/10:
		rec.SavingsMB = r.MemoryLimitMB - suggested
		rec.Message = fmt.Sprintf("Uses at most %d MB but the limit is %d MB; lowering it to %d MB frees %d MB.", peakMB, r.MemoryLimitMB, suggested, rec.SavingsMB)
	default:
		return nil
	}
	return rec
}

func roundUp(v, step int) int { return (v + step - 1) / step * step }
