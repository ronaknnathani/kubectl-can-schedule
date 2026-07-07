.PHONY: build clean test test-coverage install install-plugin deps run lint snapshot check ci help

BINARY := kubectl-can_schedule
PKG := ./cmd/$(BINARY)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
GOLANGCI_LINT_VERSION ?= v2.12.2
TOOLS_DIR := $(CURDIR)/.tools
GOLANGCI_LINT := $(TOOLS_DIR)/golangci-lint/$(GOLANGCI_LINT_VERSION)/golangci-lint

# Build the plugin after dependency, test, and lint checks
build: deps test lint
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

# Clean build artifacts
clean:
	rm -f $(BINARY)
	rm -f coverage.out
	rm -rf bin/ dist/

# Run tests
test:
	go test ./...

# Run tests with coverage
test-coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

# Run linter
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run ./...

# Run all checks (test + lint)
check: test lint

# Run the same checks as GitHub Actions
ci: $(GOLANGCI_LINT)
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run ./...
	go build -o $(BINARY) $(PKG)
	./$(BINARY) --version

$(GOLANGCI_LINT):
	mkdir -p $(dir $@)
	GOBIN=$(dir $@) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Local release test (no publish)
snapshot:
	goreleaser release --snapshot --clean

# Install the plugin onto your PATH so `kubectl can-schedule` works
install: build
	mkdir -p ~/.local/bin
	cp $(BINARY) ~/.local/bin/
	chmod +x ~/.local/bin/$(BINARY)

# Install as a kubectl plugin
install-plugin: build
	mkdir -p ~/.kube/plugins/can-schedule
	cp $(BINARY) ~/.kube/plugins/can-schedule/
	chmod +x ~/.kube/plugins/can-schedule/$(BINARY)

# Download dependencies
deps:
	go mod tidy

# Run against the current Kubernetes context
run:
	go run $(PKG) --resource cpu=1 --replicas 1

# Help
help:
	@echo "Available targets:"
	@echo "  build          - Build the plugin"
	@echo "  clean          - Clean build artifacts"
	@echo "  test           - Run tests"
	@echo "  test-coverage  - Run tests with coverage report"
	@echo "  lint           - Run golangci-lint"
	@echo "  check          - Run tests and linter"
	@echo "  ci             - Run the same checks as GitHub Actions"
	@echo "  snapshot       - Build snapshot release (goreleaser)"
	@echo "  install        - Install to ~/.local/bin"
	@echo "  install-plugin - Install as kubectl plugin"
	@echo "  deps           - Download dependencies"
	@echo "  run            - Run against the current Kubernetes context"
	@echo "  help           - Show this help"
