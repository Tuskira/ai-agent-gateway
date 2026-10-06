MODULE  := github.com/Tuskira/tusk-ai-secured-gateway
BINARY  := gateway
VERSION ?= $(shell git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-go-only run test lint vuln ci snapshot docker-build compose-up compose-down tidy notices ui-install ui-dev ui-build examples-smoke examples-k8s

# build produces the full binary with the React admin console embedded:
# ui-build populates internal/api/ui/dist before the Go compiler runs.
build: ui-build build-go-only

# build-go-only skips the UI build -- fast iteration on the Go side using
# whatever (possibly empty, possibly stale) internal/api/ui/dist already
# has on disk. ui.Handler() serves 503 if dist/ was never built.
build-go-only:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/gateway

run: build
	./bin/$(BINARY)

test:
	go test -race ./...

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed, skipping (see https://golangci-lint.run/welcome/install/)"; \
	fi

vuln:
	@if command -v govulncheck >/dev/null 2>&1; then \
		govulncheck ./...; \
	else \
		echo "govulncheck not installed, skipping (go install golang.org/x/vuln/cmd/govulncheck@latest)"; \
	fi

# ci runs everything the CI "go" and "web" jobs run, in one shot, against
# whatever's already on disk -- no containers spun up. It does NOT run
# the "integration" job's Postgres/ClickHouse/e2e suites (those need
# live services; see .github/workflows/ci.yml's integration job, or the
# `go test` invocations documented atop test/e2e/mcp_test.go and
# pkg/sink/clickhouse/integration_test.go, for how to run them locally).
ci:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "$$out"; echo "gofmt: files not formatted, run 'gofmt -w .'" >&2; exit 1; fi
	go vet ./...
	go vet -tags e2e ./test/...
	go build ./...
	$(MAKE) notices
	go test -race -count=1 ./...
	$(MAKE) lint
	$(MAKE) vuln
	npm ci --prefix web
	npm run typecheck --prefix web
	npm run lint --prefix web
	npm run test --prefix web
	npm run build --prefix web

# snapshot builds the current commit's binary the same way a real
# release would (see .goreleaser.yaml), without tagging or publishing
# anything. ui-build runs first because the binary is built by
# goreleaser and internal/api/ui/dist is go:embed'd in at that point.
snapshot: ui-build
	@if command -v goreleaser >/dev/null 2>&1; then \
		goreleaser build --snapshot --clean --single-target; \
	else \
		echo "goreleaser not installed (go install github.com/goreleaser/goreleaser/v2@latest)"; \
		exit 1; \
	fi

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) .

compose-up:
	docker compose -f deploy/docker-compose.yml up --build

compose-down:
	docker compose -f deploy/docker-compose.yml down -v

# examples-smoke runs run.sh for every example that needs no external
# credential (today: 01-quickstart, 05-profiles, 12-skills-and-commands,
# 13-claude-desktop (gateway side only), and 04-python-agent -- see examples/README.md). Each run.sh brings up what it needs itself (the
# compose stack, a local MCP server) and is safe to re-run. 04-python-agent
# runs in its --mcp-only mode here, since no ANTHROPIC_API_KEY is set in CI.
examples-smoke:
	./examples/01-quickstart/run.sh
	./examples/05-profiles/run.sh
	./examples/12-skills-and-commands/run.sh
	./examples/04-python-agent/run.sh
	./examples/02-claude-code/check.sh
	./examples/03-cursor-and-vscode/check.sh
	./examples/06-credentials-and-headers/run.sh
	./examples/07-llm-passthrough/run.sh
	./examples/08-observability/run.sh
	./examples/11-server-requests/run.sh
	./examples/13-claude-desktop/check.sh
	./examples/13-claude-desktop/run.sh
	./examples/09-roles-keys-and-rate-limits/run.sh

# examples-k8s runs the Kubernetes example on a local kind cluster
# (builds the image, creates the cluster if missing). Slow; CI runs it
# only when deploy/ or examples/10-kubernetes/ change.
examples-k8s:
	./examples/10-kubernetes/run.sh

tidy:
	go mod tidy

# notices classifies the license of every third-party Go module in the
# released binary's dependency closure (./cmd/gateway), fails if any of
# them isn't on the strict allow list in tools/notices/main.go, and writes
# THIRD_PARTY_NOTICES (full license + NOTICE text per module, not
# committed -- see .gitignore). Run before a release (.goreleaser.yaml's
# before.hooks does this) and in CI (the "go" job), but also safe to run
# any time to check a new dependency's license by hand.
notices:
	go run -C tools/notices . -repo-root ../.. -pkg ./cmd/gateway -out THIRD_PARTY_NOTICES

ui-install:
	cd web && npm install

ui-dev:
	cd web && npm run dev

ui-build:
	npm ci --prefix web
	npm run build --prefix web
