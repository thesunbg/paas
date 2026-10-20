package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func vet(t *testing.T, in, scope string) (string, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Dockerfile")
	if err := os.WriteFile(p, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	err := vetDockerfile(p, scope)
	b, _ := os.ReadFile(p)
	return string(b), err
}

func TestVetDockerfile(t *testing.T) {
	cases := []struct {
		name, in, want, errSub string
	}{
		{name: "no id", in: "FROM x\nRUN --mount=type=cache,target=/root/.cache go build\n",
			want: "FROM x\nRUN --mount=type=cache,target=/root/.cache,id=P/root/.cache go build\n"},
		{name: "dst alias", in: "RUN --mount=type=cache,dst=/c make\n",
			want: "RUN --mount=type=cache,dst=/c,id=P/c make\n"},
		{name: "with id keeps options", in: "RUN --mount=type=cache,id=gomod,target=/go/pkg,sharing=locked,mode=0755 go mod download\n",
			want: "RUN --mount=type=cache,id=P/gomod,target=/go/pkg,sharing=locked,mode=0755 go mod download\n"},
		{name: "upper case keys", in: "run --mount=Type=Cache,ID=a,target=/a x\n",
			want: "run --mount=Type=Cache,ID=P/a,target=/a x\n"},
		{name: "two mounts one line", in: "RUN --mount=type=cache,target=/a --mount=type=bind,source=.,target=/s --mount=type=cache,id=b,target=/b x\n",
			want: "RUN --mount=type=cache,target=/a,id=P/a --mount=type=bind,source=.,target=/s --mount=type=cache,id=P/b,target=/b x\n"},
		{name: "multi line keeps numbering", in: "RUN --mount=type=cache,target=/a \\\n    --mount=type=cache,id=b,target=/b \\\n    make\nENV A=1\n",
			want: "RUN --mount=type=cache,target=/a,id=P/a     --mount=type=cache,id=P/b,target=/b     make\n\n\nENV A=1\n"},
		{name: "comment inside continuation", in: "RUN --mount=type=cache,target=/a \\\n# note\n    make\n",
			want: "RUN --mount=type=cache,target=/a,id=P/a     make\n\n\n"},
		{name: "comment line ignored", in: "# RUN --mount=type=cache,target=/a x\nFROM y\n", want: "# RUN --mount=type=cache,target=/a x\nFROM y\n"},
		{name: "heredoc body ignored", in: "RUN <<EOF\nmount --type=cache\nRUN --mount=type=cache,target=/z x\nEOF\nRUN --mount=type=cache,target=/a x\n",
			want: "RUN <<EOF\nmount --type=cache\nRUN --mount=type=cache,target=/z x\nEOF\nRUN --mount=type=cache,target=/a,id=P/a x\n"},
		{name: "heredoc with flag", in: "RUN --mount=type=cache,target=/a <<-EOF\n\techo hi\n\tEOF\n",
			want: "RUN --mount=type=cache,target=/a,id=P/a <<-EOF\n\techo hi\n\tEOF\n"},
		{name: "non cache mounts untouched", in: "RUN --mount=type=secret,id=x --mount=type=bind,target=/b cmd\n",
			want: "RUN --mount=type=secret,id=x --mount=type=bind,target=/b cmd\n"},
		{name: "crlf", in: "RUN --mount=type=cache,target=/a x\r\nENV A=1\r\n",
			want: "RUN --mount=type=cache,target=/a,id=P/a x\nENV A=1\r\n"},
		{name: "syntax ok", in: "# syntax=docker/dockerfile:1\nFROM x\n", want: "# syntax=docker/dockerfile:1\nFROM x\n"},
		{name: "syntax digest ok", in: "# syntax = docker/dockerfile@sha256:abc\nFROM x\n", want: "# syntax = docker/dockerfile@sha256:abc\nFROM x\n"},
		{name: "syntax evil", in: "# syntax=evil/frontend\nFROM x\n", errSub: "line 1: # syntax=evil/frontend"},
		{name: "syntax evil after other directive", in: "# check=skip=all\n# syntax=docker.io/evil/x\nFROM x\n", errSub: "line 2"},
		{name: "syntax prefix trick", in: "# syntax=docker/dockerfilex\nFROM x\n", errSub: "not allowed"},
		{name: "syntax shebang", in: "#!/bin/evil\nFROM x\n", errSub: "shebang"},
		{name: "syntax later is a comment", in: "FROM x\n# syntax=evil\n", want: "FROM x\n# syntax=evil\n"},
		{name: "escape backtick", in: "# escape=`\nFROM x\n", errSub: "line 1: # escape="},
		{name: "escape default ok", in: "# escape=\\\nFROM x\n", want: "# escape=\\\nFROM x\n"},
		{name: "type variable", in: "ARG T=cache\nRUN --mount=type=$T,target=/a x\n", errSub: "line 2: --mount type"},
		{name: "type braces", in: "RUN --mount=type=${T},target=/a x\n", errSub: "not a literal"},
		{name: "quoted mount", in: "RUN --mount=\"type=cache,target=/a\" x\n", errSub: "quoted"},
		{name: "space separated flag", in: "RUN --mount type=cache,target=/a x\n", errSub: "line 1"},
		{name: "onbuild", in: "ONBUILD RUN --mount=type=cache,target=/a x\n", errSub: "outside a RUN"},
		{name: "cache text in command", in: "RUN echo --mount=type=cache,target=/a\n", errSub: "found 1 type=cache but could only rewrite 0"},
		{name: "duplicate id", in: "RUN --mount=type=cache,id=a,id=b,target=/a x\n", errSub: "more than one id"},
		{name: "no target no id", in: "RUN --mount=type=cache x\n", errSub: "no id and no target"},
		{name: "unterminated heredoc", in: "RUN <<EOF\necho\n", errSub: "never terminated"},
		{name: "go golden", in: `# syntax=docker/dockerfile:1
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /app ./cmd/app
FROM gcr.io/distroless/static
COPY --from=build /app /app
`, want: `# syntax=docker/dockerfile:1
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,id=P/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod,id=P/go/pkg/mod     --mount=type=cache,target=/root/.cache/go-build,id=P/root/.cache/go-build     CGO_ENABLED=0 go build -o /app ./cmd/app


FROM gcr.io/distroless/static
COPY --from=build /app /app
`},
		{name: "npm golden", in: "FROM node:22\nWORKDIR /app\nCOPY package*.json ./\nRUN --mount=type=cache,id=npm,target=/root/.npm npm ci\nCOPY . .\nRUN npm run build\n",
			want: "FROM node:22\nWORKDIR /app\nCOPY package*.json ./\nRUN --mount=type=cache,id=P/npm,target=/root/.npm npm ci\nCOPY . .\nRUN npm run build\n"},
		{name: "pip golden", in: "FROM python:3.12\nCOPY requirements.txt .\nRUN --mount=type=cache,target=/root/.cache/pip pip install -r requirements.txt\nCOPY . .\n",
			want: "FROM python:3.12\nCOPY requirements.txt .\nRUN --mount=type=cache,target=/root/.cache/pip,id=P/root/.cache/pip pip install -r requirements.txt\nCOPY . .\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := vet(t, c.in, "P")
			if c.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), c.errSub) {
					t.Fatalf("want error containing %q, got %v", c.errSub, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, c.want)
			}
		})
	}
}

func TestVetDockerfileNoScope(t *testing.T) {
	in := "RUN --mount=type=cache,target=/a x\n"
	got, err := vet(t, in, "")
	if err != nil || got != in {
		t.Fatalf("no scope must leave the file alone: %q %v", got, err)
	}
	if _, err := vet(t, "# syntax=evil\n"+in, ""); err == nil {
		t.Fatal("syntax must still be vetted without scope")
	}
}

func TestVetDockerfileBadScope(t *testing.T) {
	if _, err := vet(t, "FROM x\n", "a,b"); err == nil {
		t.Fatal("want error for scope with comma")
	}
}
