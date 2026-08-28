.PHONY: help test test-race test-coverage fmt fmt-check vet lint vulncheck check tidy

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

check: fmt-check vet lint test vulncheck ## Everything CI runs
	@echo "✅ All checks passed"
