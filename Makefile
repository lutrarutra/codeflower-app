.PHONY: launcher-build launcher-build-linux launcher-build-all launcher-test launcher-install

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LAUNCHER_LDFLAGS := -X codeflower-launcher/internal/cli.Version=$(VERSION)

# The launcher is a static Go binary: build it for this machine, or for the
# cluster with launcher-build-linux and copy it there.
launcher-build:
	CGO_ENABLED=0 go build -ldflags "$(LAUNCHER_LDFLAGS)" -o build/codeflower ./cmd/codeflower

launcher-build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LAUNCHER_LDFLAGS)" \
		-o build/codeflower-linux-amd64 ./cmd/codeflower

# Launchers for every desktop system, e.g. to hand to users who start
# sandboxes with "codeflower gui": build/codeflower-<os>-<arch>[.exe].
launcher-build-all:
	for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LAUNCHER_LDFLAGS)" \
			-o build/codeflower-$$os-$$arch$$ext ./cmd/codeflower || exit 1; \
	done

launcher-test:
	go vet ./... && go test ./...

# Installs codeflower into $(shell go env GOPATH 2>/dev/null || echo ~/go)/bin.
launcher-install:
	CGO_ENABLED=0 go install -ldflags "$(LAUNCHER_LDFLAGS)" ./cmd/codeflower
