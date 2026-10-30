.PHONY: app-build app-build-linux app-build-all app-dist app-macos app-test app-install

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
APP_LDFLAGS := -X codeflower-app/internal/cli.Version=$(VERSION)

# The app is a static Go binary: build it for this machine, or for the
# cluster with app-build-linux and copy it there.
app-build:
	CGO_ENABLED=0 go build -ldflags "$(APP_LDFLAGS)" -o build/codeflower ./cmd/codeflower

app-build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(APP_LDFLAGS)" \
		-o build/codeflower-linux-amd64 ./cmd/codeflower

# Binaries for every desktop system, e.g. to hand to users who start
# sandboxes with "codeflower client": build/codeflower-<os>-<arch>[.exe].
app-build-all:
	for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(APP_LDFLAGS)" \
			-o build/codeflower-$$os-$$arch$$ext ./cmd/codeflower || exit 1; \
	done

# The release archives of app-build-all, with checksums: build/dist/codeflower-<os>-<arch>.tar.gz
# (.zip for Windows), each holding the binary as codeflower[.exe].
app-dist: app-build-all
	rm -rf build/dist build/stage && mkdir -p build/dist
	for binary in build/codeflower-*-*; do \
		name=$$(basename $$binary .exe); ext=; case $$binary in *.exe) ext=.exe;; esac; \
		mkdir -p build/stage/$$name && cp $$binary build/stage/$$name/codeflower$$ext && cp README.md build/stage/$$name/; \
		if [ -n "$$ext" ]; then (cd build/stage/$$name && zip -q ../../dist/$$name.zip codeflower$$ext README.md); \
		else tar -C build/stage/$$name -czf build/dist/$$name.tar.gz codeflower README.md; fi; \
	done
	rm -rf build/stage
	cd build/dist && shasum -a 256 * > SHA256SUMS

# The macOS app (double-click opens the window), as build/dist/codeflower-macos.dmg; needs macOS.
app-macos: app-build-all
	scripts/macos-app.sh build/codeflower-darwin-arm64 build/codeflower-darwin-amd64 $(VERSION) build/dist

app-test:
	go vet ./... && go test ./...

# Installs codeflower into $(shell go env GOPATH 2>/dev/null || echo ~/go)/bin.
app-install:
	CGO_ENABLED=0 go install -ldflags "$(APP_LDFLAGS)" ./cmd/codeflower
