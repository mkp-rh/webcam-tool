.PHONY: help all build run test fmt tidy clean deps-fedora deps-ubuntu

APP := webcamctl
PKG := ./cmd/webcamctl
BIN := bin/$(APP)

help:
	@echo "Targets:"
	@echo "  make build        Build $(BIN)"
	@echo "  make run          Run the GUI (go run)"
	@echo "  make test         Compile/test non-GUI packages"
	@echo "  make fmt          gofmt all Go files"
	@echo "  make tidy         go mod tidy"
	@echo "  make clean        Remove build artifacts"
	@echo "  make deps-fedora  Install build deps (Fedora)"
	@echo "  make deps-ubuntu  Install build deps (Ubuntu/Debian)"

all: build

build:
	@mkdir -p bin
	go build -o "$(BIN)" "$(PKG)"

run:
	go run "$(PKG)"

# In CI/headless environments you may not have GUI build deps installed.
test:
	go test ./internal/...

fmt:
	gofmt -w $$(go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .TestGoFiles}}{{$$.Dir}}/{{.}} {{end}}{{range .XTestGoFiles}}{{$$.Dir}}/{{.}} {{end}}' ./...)

tidy:
	go mod tidy

clean:
	rm -rf bin

deps-fedora:
	sudo dnf install -y gcc pkgconf-pkg-config mesa-libGL-devel libX11-devel libXrandr-devel libXcursor-devel libXi-devel libXxf86vm-devel v4l-utils

deps-ubuntu:
	sudo apt-get update
	sudo apt-get install -y build-essential pkg-config libgl1-mesa-dev xorg-dev v4l-utils

