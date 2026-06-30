BINARY := kubectl-can_schedule
PKG := ./cmd/$(BINARY)
INSTALL_DIR ?= $(shell go env GOPATH)/bin

.PHONY: build test vet tidy install clean

build:
	go build -o bin/$(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# Installs the plugin onto your PATH so `kubectl can-schedule` works.
install:
	go build -o $(INSTALL_DIR)/$(BINARY) $(PKG)
	@echo "installed $(INSTALL_DIR)/$(BINARY) — run: kubectl can-schedule --help"

clean:
	rm -rf bin
