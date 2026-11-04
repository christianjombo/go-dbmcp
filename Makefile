# mcp-proxy — build and release automation
#
# The deployment target is a bare DigitalOcean droplet running the binary
# under systemd, so the default release target cross-compiles a fully static
# linux/amd64 binary with no runtime dependencies.
#
#   make verify    run formatting, vet and the test suite
#   make release   clean, cross-compile for the droplet, print verify checks
#   make build     build for the local machine (development)
#   make run       run locally against .env
#   make clean     remove build artefacts

BINARY      := mcp-proxy-binary
PKG         := ./cmd/mcp-proxy
BUILD_DIR   := bin
ARTIFACT    := $(BUILD_DIR)/$(BINARY)

# Target platform for the droplet. Override for arm droplets:
#   make release GOARCH=arm64
GOOS        ?= linux
GOARCH      ?= amd64

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

HOST_OS     := $(shell go env GOOS)
HOST_ARCH   := $(shell go env GOARCH)

# -s -w strip the symbol table and DWARF data; -trimpath removes local
# filesystem paths from the binary so builds are reproducible.
LDFLAGS     := -s -w -X main.version=$(VERSION)
GOFLAGS     := -trimpath

.DEFAULT_GOAL := help
.PHONY: help all clean fmt fmt-check vet test test-race verify build release verify-binary tidy run install-unit

help:
	@echo "mcp-proxy $(VERSION)"
	@echo
	@echo "  make verify    gofmt check, go vet, race-enabled tests"
	@echo "  make release   clean + static $(GOOS)/$(GOARCH) build + verify checks"
	@echo "  make build     build for this machine ($(HOST_OS)/$(HOST_ARCH))"
	@echo "  make run       run locally, reading .env if present"
	@echo "  make clean     remove $(BUILD_DIR)/"

all: verify release

clean:
	@echo "==> cleaning"
	@rm -rf $(BUILD_DIR)
	@go clean -testcache

tidy:
	@go mod tidy

fmt:
	@gofmt -w .

fmt-check:
	@echo "==> gofmt"
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "    FAIL: these files are not gofmt-clean:"; \
		echo "$$unformatted" | sed 's/^/      /'; \
		exit 1; \
	fi
	@echo "    ok"

vet:
	@echo "==> go vet"
	@go vet ./...
	@echo "    ok"

test:
	@echo "==> go test"
	@go test ./...

test-race:
	@echo "==> go test -race"
	@go test -race ./...

verify: fmt-check vet test-race
	@echo "==> verify passed"

build:
	@mkdir -p $(BUILD_DIR)
	@go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(ARTIFACT) $(PKG)
	@echo "built $(ARTIFACT) for $(HOST_OS)/$(HOST_ARCH)"

# Production artefact: one static binary, nothing else to ship.
release: clean
	@echo "==> building $(GOOS)/$(GOARCH) release binary"
	@mkdir -p $(BUILD_DIR)
	@CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(ARTIFACT) $(PKG)
	@$(MAKE) --no-print-directory verify-binary

verify-binary:
	@echo
	@echo "==================== BUILD VERIFICATION ===================="
	@printf "  %-18s %s\n" "artifact:" "$(ARTIFACT)"
	@printf "  %-18s %s\n" "version:" "$(VERSION)"
	@printf "  %-18s %s\n" "commit:" "$(COMMIT)"
	@printf "  %-18s %s\n" "built (UTC):" "$(BUILD_TIME)"
	@printf "  %-18s %s\n" "target:" "$(GOOS)/$(GOARCH)"
	@test -f $(ARTIFACT) || { echo "  FAIL: artifact missing"; exit 1; }
	@printf "  %-18s %s\n" "size:" "$$(ls -lh $(ARTIFACT) | awk '{print $$5}')"
	@printf "  %-18s %s\n" "sha256:" "$$( (sha256sum $(ARTIFACT) 2>/dev/null || shasum -a 256 $(ARTIFACT)) | awk '{print $$1}')"
	@printf "  %-18s %s\n" "file:" "$$(file -b $(ARTIFACT) 2>/dev/null || echo 'file(1) unavailable')"
	@echo "  ---------------------------------------------------------"
	@# CGO_ENABLED=0 must yield a binary with no dynamic dependencies, or it
	@# will not start on a minimal droplet image.
	@if file -b $(ARTIFACT) 2>/dev/null | grep -qi "statically linked\|static-pie"; then \
		printf "  %-18s %s\n" "linkage:" "static (no libc dependency)"; \
	elif file -b $(ARTIFACT) 2>/dev/null | grep -qi "dynamically linked"; then \
		printf "  %-18s %s\n" "linkage:" "WARNING: dynamically linked — check CGO_ENABLED=0"; \
	else \
		printf "  %-18s %s\n" "linkage:" "not determined"; \
	fi
	@# Only executable here when we built for this machine.
	@if [ "$(GOOS)" = "$(HOST_OS)" ] && [ "$(GOARCH)" = "$(HOST_ARCH)" ]; then \
		printf "  %-18s %s\n" "self-report:" "$$(./$(ARTIFACT) -version)"; \
	else \
		printf "  %-18s %s\n" "self-report:" "skipped (cross-compiled for $(GOOS)/$(GOARCH))"; \
	fi
	@echo "============================================================"
	@echo
	@echo "Next: copy to the droplet and restart the service"
	@echo "  scp $(ARTIFACT) root@<droplet>:/opt/mcp-proxy/$(BINARY)"
	@echo "  ssh root@<droplet> 'systemctl restart mcp-proxy && systemctl status mcp-proxy --no-pager'"

run: build
	@set -a; [ -f .env ] && . ./.env; set +a; ./$(ARTIFACT)

# Convenience for a first install on the droplet (run as root, on the host).
install-unit:
	@install -D -m 0644 deploy/mcp-proxy.service /etc/systemd/system/mcp-proxy.service
	@install -d -m 0750 /etc/mcp-proxy
	@test -f /etc/mcp-proxy/mcp-proxy.env || install -m 0640 deploy/mcp-proxy.env.example /etc/mcp-proxy/mcp-proxy.env
	@systemctl daemon-reload
	@echo "installed. edit /etc/mcp-proxy/mcp-proxy.env then: systemctl enable --now mcp-proxy"
