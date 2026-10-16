VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w
GOFLAGS := -trimpath

.PHONY: all build web paasd paas test test-integration vet fmt clean release

all: build

build: paasd paas

paasd:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/paasd ./cmd/paasd

paas:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/paas ./cmd/paas

# Rebuild the dashboard into internal/webui/dist (embedded into paasd).
web:
	cd web && npm ci && npm run build

# DB-backed tests skip unless PAAS_TEST_DATABASE_URL is set. Locally:
#   podman run -d -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:16
#   export PAAS_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
test:
	go test ./...

# Needs root, containerd, buildkitd (containerd worker) and CNI plugins.
test-integration:
	go test -tags integration ./internal/runtime/ ./internal/build/

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

release:
	@mkdir -p dist
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/paasd-linux-$$arch ./cmd/paasd; \
		for os in linux darwin; do \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o dist/paas-$$os-$$arch ./cmd/paas; \
		done; \
	done
	cp internal/installer/install.sh dist/install.sh

clean:
	rm -rf bin dist
