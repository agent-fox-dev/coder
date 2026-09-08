.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*##"}; {printf "  %-16s %s\n", $$1, $$2}'

.PHONY: test
test: ## Run all Go tests (root module + difftest and flatline submodules)
	go test ./...
	cd difftest && go test ./...
	@$(MAKE) --no-print-directory test-flatline

# examples/flatline is a nested module that imports the agent-fox spec library
# through a replace to a sibling checkout (see examples/flatline/go.mod). When
# that checkout is absent the module cannot build, so its tests are skipped
# rather than failed.
.PHONY: test-flatline
test-flatline: ## Run the flatline example's tests (needs ../spec/golang)
	@if [ -d ../spec/golang ]; then \
		cd examples/flatline && go test ./...; \
	else \
		echo "examples/flatline: ../spec/golang not found; skipping (see examples/flatline/go.mod)"; \
	fi

.PHONY: build-examples
build-examples: ## Install examples/issued, examples/cleaner and examples/flatline into ./bin
	go install ./examples/issued
	go install ./examples/cleaner
	cd examples/flatline && go install .

.PHONY: fmt
fmt: ## gofmt all source files in place
	gofmt -l -w .

.PHONY: vet
vet: ## go vet the root module and difftest submodule (and flatline when buildable)
	go vet ./...
	cd difftest && go vet ./...
	@if [ -d ../spec/golang ]; then cd examples/flatline && go vet ./...; fi

.PHONY: lint
lint: ## Run golangci-lint if installed, otherwise skip
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./... && (cd difftest && golangci-lint run ./...); \
	else \
		echo "golangci-lint not installed; skipping (see https://golangci-lint.run)"; \
	fi

.PHONY: tidy
tidy: ## go mod tidy the root module and submodules
	go mod tidy
	cd difftest && go mod tidy
	@if [ -d ../spec/golang ]; then cd examples/flatline && go mod tidy; fi

.PHONY: check
check: fmt vet lint test ## Run fmt, vet, lint and test - use before committing

