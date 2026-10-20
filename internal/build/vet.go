package build

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// vetDockerfile checks a user Dockerfile and rewrites it in place so that every
// `RUN --mount=type=cache` uses a cache id under scope (the project id). Without
// that, a user who knows another project's cache id could read or poison it.
//
// There is no BuildKit parser in go.mod, so this scans by hand: join `\`
// continuations, skip comments and heredoc bodies, read the --mount flags that
// precede the RUN command. Anything it cannot read with certainty is rejected
// (fail-closed) rather than guessed at.
func vetDockerfile(path, scope string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimPrefix(string(data), "\xef\xbb\xbf"), "\n")
	if err := vetDirectives(lines); err != nil {
		return err
	}
	if scope == "" {
		// paasd older than this change sends no project_id; drop this branch once all control planes do.
		return nil
	}
	if !scopeRe.MatchString(scope) {
		return fmt.Errorf("invalid cache scope %q", scope)
	}
	out, changed, err := scopeCacheMounts(lines, scope)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), st.Mode().Perm())
}

var (
	scopeRe     = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	directiveRe = regexp.MustCompile(`^#\s*([A-Za-z][A-Za-z0-9]*)\s*=\s*(.*?)\s*$`)
	syntaxRe    = regexp.MustCompile(`^docker/dockerfile(:|@)\S+$`)
	contRe      = regexp.MustCompile(`\\[ \t]*$`)
	heredocRe   = regexp.MustCompile(`^\d*<<(-?)([^<]*)$`)
)

// vetDirectives checks the parser directives at the top of the file. A custom
// `# syntax=` frontend would bypass every later check; a custom escape char
// would change how this scanner joins lines.
func vetDirectives(lines []string) error {
	if len(lines) > 0 && strings.HasPrefix(lines[0], "#!") {
		return fmt.Errorf("Dockerfile line 1: shebang syntax selection is not supported")
	}
	for i, l := range lines {
		m := directiveRe.FindStringSubmatch(strings.TrimRight(l, "\r"))
		if m == nil {
			return nil
		}
		switch strings.ToLower(m[1]) {
		case "syntax":
			if !syntaxRe.MatchString(m[2]) {
				return fmt.Errorf("Dockerfile line %d: # syntax=%s is not allowed (only docker/dockerfile:<tag> or @<digest>)", i+1, m[2])
			}
		case "escape":
			if m[2] != `\` {
				return fmt.Errorf("Dockerfile line %d: # escape=%s is not supported (only the default \\)", i+1, m[2])
			}
		}
	}
	return nil
}

func isBlankOrComment(l string) bool {
	t := strings.TrimSpace(l)
	return t == "" || t[0] == '#'
}

// scopeCacheMounts walks the instructions and returns the lines with every
// cache mount rewritten. A rewritten instruction becomes one joined line
// followed by blank lines, so later line numbers do not move.
func scopeCacheMounts(lines []string, scope string) ([]string, bool, error) {
	out := append([]string(nil), lines...)
	changed := false
	for i := 0; i < len(lines); {
		if isBlankOrComment(lines[i]) {
			i++
			continue
		}
		start := i
		var sb strings.Builder
		for i < len(lines) {
			s := strings.TrimRight(lines[i], "\r")
			i++
			if !contRe.MatchString(s) {
				sb.WriteString(s)
				break
			}
			sb.WriteString(contRe.ReplaceAllString(s, ""))
			// Comment and blank lines inside a continued instruction are dropped by the Dockerfile parser.
			for i < len(lines) && isBlankOrComment(lines[i]) {
				i++
			}
		}
		end := i
		text := sb.String()
		kw := strings.ToLower(firstField(text))

		if kw == "run" || kw == "copy" || kw == "add" {
			var err error
			if i, err = skipHeredocs(lines, i, text, start+1); err != nil {
				return nil, false, err
			}
		}

		want := strings.Count(strings.ToLower(text), "type=cache")
		if want == 0 && kw != "run" {
			continue
		}
		if kw != "run" {
			return nil, false, fmt.Errorf("Dockerfile line %d: type=cache outside a RUN --mount flag is not supported", start+1)
		}
		rewritten, n, err := rewriteRun(text, scope)
		if err != nil {
			return nil, false, fmt.Errorf("Dockerfile line %d: %w", start+1, err)
		}
		if n != want {
			return nil, false, fmt.Errorf("Dockerfile line %d: found %d type=cache but could only rewrite %d; write each as a plain --mount=type=cache,... flag before the command", start+1, want, n)
		}
		if n == 0 {
			continue
		}
		out[start] = rewritten
		for j := start + 1; j < end; j++ {
			out[j] = ""
		}
		changed = true
	}
	return out, changed, nil
}

// skipHeredocs returns the index after the bodies of any heredocs opened on
// the instruction text, which starts at 1-based line.
func skipHeredocs(lines []string, i int, text string, line int) (int, error) {
	for _, f := range strings.Fields(text) {
		m := heredocRe.FindStringSubmatch(f)
		if m == nil {
			continue
		}
		name := strings.Trim(m[2], `"'`)
		if name == "" {
			continue
		}
		for {
			if i >= len(lines) {
				return 0, fmt.Errorf("Dockerfile line %d: heredoc %q is never terminated", line, name)
			}
			l := strings.TrimRight(lines[i], "\r")
			i++
			if m[1] == "-" {
				l = strings.TrimLeft(l, "\t")
			}
			if l == name {
				break
			}
		}
	}
	return i, nil
}

func firstField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// rewriteRun rewrites the --mount flags of one joined RUN instruction and
// reports how many cache mounts it scoped.
func rewriteRun(text, scope string) (string, int, error) {
	var out strings.Builder
	rest := text
	// keyword, with its leading whitespace
	lead := len(rest) - len(strings.TrimLeft(rest, " \t"))
	kwEnd := lead + len(firstField(rest))
	out.WriteString(rest[:kwEnd])
	rest = rest[kwEnd:]
	n := 0
	for {
		ws := len(rest) - len(strings.TrimLeft(rest, " \t"))
		out.WriteString(rest[:ws])
		rest = rest[ws:]
		if !strings.HasPrefix(rest, "--") {
			break
		}
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		tok := rest[:end]
		rest = rest[end:]
		if strings.HasPrefix(tok, "--mount") {
			val, ok := strings.CutPrefix(tok, "--mount=")
			if !ok {
				return "", 0, fmt.Errorf("unsupported flag form %q; use --mount=<options>", tok)
			}
			nv, cache, err := scopeMount(val, scope)
			if err != nil {
				return "", 0, err
			}
			if cache {
				n++
			}
			tok = "--mount=" + nv
		}
		out.WriteString(tok)
	}
	out.WriteString(rest)
	return out.String(), n, nil
}

// scopeMount rewrites the options of one --mount flag; cache reports whether it was a cache mount.
func scopeMount(val, scope string) (string, bool, error) {
	if strings.ContainsAny(val, `"'\`) {
		return "", false, fmt.Errorf("quoted or escaped --mount value %q is not supported", val)
	}
	fields := strings.Split(val, ",")
	typ, id, target := -1, -1, -1
	for i, f := range fields {
		k, _, _ := strings.Cut(f, "=")
		switch strings.ToLower(k) {
		case "type":
			if typ >= 0 {
				return "", false, fmt.Errorf("--mount has more than one type")
			}
			typ = i
		case "id":
			if id >= 0 {
				return "", false, fmt.Errorf("--mount has more than one id")
			}
			id = i
		case "target", "dst", "destination":
			if target >= 0 {
				return "", false, fmt.Errorf("--mount has more than one target")
			}
			target = i
		}
	}
	if typ < 0 {
		return val, false, nil // bind mount, the default
	}
	_, tv, _ := strings.Cut(fields[typ], "=")
	if strings.Contains(tv, "$") {
		return "", false, fmt.Errorf("--mount type %q is not a literal", tv)
	}
	if !strings.EqualFold(tv, "cache") {
		return val, false, nil
	}
	if id >= 0 {
		k, v, ok := strings.Cut(fields[id], "=")
		if !ok {
			return "", false, fmt.Errorf("--mount id has no value")
		}
		fields[id] = k + "=" + scope + "/" + v
	} else {
		if target < 0 {
			return "", false, fmt.Errorf("cache mount has no id and no target")
		}
		_, v, ok := strings.Cut(fields[target], "=")
		if !ok || v == "" {
			return "", false, fmt.Errorf("cache mount target has no value")
		}
		fields = append(fields, "id="+scope+v)
	}
	return strings.Join(fields, ","), true, nil
}
