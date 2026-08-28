.PHONY: help test test-race test-coverage fmt fmt-check vet lint vulncheck check tidy tidy-check

help:
	@grep -E '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

test: ## Run tests with race detection and coverage
	go test -race -coverprofile=coverage.out ./...
	@echo "✅ Tests complete"

test-coverage: test ## Show the coverage report
	go tool cover -func=coverage.out | tail -1

fmt: ## Format
	gofmt -w .

fmt-check: ## Fail if unformatted
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "❌ run make fmt" && exit 1)
	@echo "✅ gofmt clean"

vet: ## go vet
	go vet ./...
	@echo "✅ vet clean"

lint: ## golangci-lint
	golangci-lint run
	@echo "✅ Lint complete"

vulncheck: ## govulncheck
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	@echo "✅ No known vulnerabilities"

tidy: ## go mod tidy
	go mod tidy

# Compared against a snapshot rather than against git: `git diff` cannot tell an
# untidy go.mod from a tidy one the developer has legitimately edited and not yet
# committed, so it failed the gate on every in-progress dependency change. The
# snapshot answers the actual question — would `go mod tidy` change anything —
# and the working tree is put back either way.
tidy-check: ## Fail if `go mod tidy` would change go.mod or go.sum
	@tmp=$$(mktemp -d); \
	cp go.mod go.sum "$$tmp/"; \
	go mod tidy; \
	rc=0; \
	{ cmp -s go.mod "$$tmp/go.mod" && cmp -s go.sum "$$tmp/go.sum"; } || rc=1; \
	cp "$$tmp/go.mod" "$$tmp/go.sum" .; \
	rm -rf "$$tmp"; \
	if [ $$rc -ne 0 ]; then \
		echo "❌ go mod tidy would change go.mod/go.sum — run make tidy and commit the result"; \
		exit 1; \
	fi; \
	echo "✅ go.mod tidy"

check: fmt-check tidy-check vet lint test vulncheck ## Everything CI runs
	@echo "✅ All checks passed"
