package templates

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/thesunbg/paas/internal/model"
)

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string    `yaml:"image"`
	Build       yaml.Node `yaml:"build"`
	Ports       []string  `yaml:"ports"`
	Expose      []any     `yaml:"expose"`
	Environment yaml.Node `yaml:"environment"`
	Volumes     []string  `yaml:"volumes"`
	Command     yaml.Node `yaml:"command"`
	Deploy      struct {
		Replicas int `yaml:"replicas"`
	} `yaml:"deploy"`
}

// ParseCompose converts a docker-compose.yml into template services.
// Only image-based services are supported; named volumes become persistent volumes.
// Services can reach each other by their compose name (DNS search domain of the project).
func ParseCompose(data []byte) ([]Service, []string, error) {
	var f composeFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, nil, fmt.Errorf("invalid compose file: %w", err)
	}
	if len(f.Services) == 0 {
		return nil, nil, fmt.Errorf("compose file has no services")
	}
	names := make([]string, 0, len(f.Services))
	for n := range f.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Service
	var warnings []string
	for _, name := range names {
		cs := f.Services[name]
		if cs.Image == "" {
			warnings = append(warnings, fmt.Sprintf("%s: skipped (build contexts are not supported; push an image or create a git service)", name))
			continue
		}
		svc := Service{Name: name, Image: cs.Image, Type: "worker", Size: "small", Env: map[string]string{}}
		for _, p := range cs.Ports {
			if port := containerPort(p); port > 0 {
				svc.Port, svc.Type = port, "web"
				break
			}
		}
		if svc.Port == 0 {
			for _, e := range cs.Expose {
				if port, err := strconv.Atoi(strings.Split(fmt.Sprint(e), "/")[0]); err == nil {
					svc.Port, svc.Type = port, "web"
					break
				}
			}
		}
		switch cs.Environment.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(cs.Environment.Content); i += 2 {
				svc.Env[cs.Environment.Content[i].Value] = cs.Environment.Content[i+1].Value
			}
		case yaml.SequenceNode:
			for _, n := range cs.Environment.Content {
				k, v, _ := strings.Cut(n.Value, "=")
				svc.Env[k] = v
			}
		}
		for _, v := range cs.Volumes {
			parts := strings.Split(v, ":")
			if len(parts) < 2 {
				continue
			}
			src, dst := parts[0], parts[1]
			if strings.HasPrefix(src, ".") || strings.HasPrefix(src, "/") || strings.HasPrefix(src, "~") {
				warnings = append(warnings, fmt.Sprintf("%s: bind mount %s ignored (use a named volume)", name, v))
				continue
			}
			svc.Volumes = append(svc.Volumes, model.Volume{Name: sanitize(src), Path: dst})
		}
		switch cs.Command.Kind {
		case yaml.ScalarNode:
			if cs.Command.Value != "" {
				svc.Command = []string{cs.Command.Value}
			}
		case yaml.SequenceNode:
			for _, n := range cs.Command.Content {
				svc.Command = append(svc.Command, n.Value)
			}
		}
		out = append(out, svc)
	}
	return out, warnings, nil
}

// containerPort extracts the container side of "8080:80", "127.0.0.1:8080:80/tcp" or "80".
func containerPort(p string) int {
	p = strings.Split(p, "/")[0]
	parts := strings.Split(p, ":")
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0
	}
	return n
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "data"
	}
	return b.String()
}
