.PHONY: build test fmt check-format vet package package-host package-file verify-package verify-package-host clean

BIN := bin/kandev-session-cost
VERSION := 0.3.1
STAGE := .build/stage
PKG_OUT := kandev-session-cost-$(VERSION).tar.gz
KANDEV_SDK := ../kandev/apps/backend

## Build the plugin binary for this machine during development.
build:
	mkdir -p bin
	go build -o $(BIN) ./server

## Run Go, browser-bundle, package-verifier, and release-version checks.
test:
	go test ./server/...
	node --test test/bundle.test.mjs
	node --check ui/bundle.js
	sh scripts/test-verify-package.sh
	sh scripts/test-verify-release-version.sh

fmt:
	gofmt -l .

check-format:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

vet:
	go vet ./server/...

## Build every platform declared by manifest.yaml and package the plugin with
## the pinned sibling Kandev checkout's plugin-pack command.
package:
	rm -rf $(STAGE)
	mkdir -p $(STAGE)/server
	cp manifest.yaml $(STAGE)/manifest.yaml
	cp -r ui $(STAGE)/ui
	GOOS=linux   GOARCH=amd64 go build -o $(STAGE)/server/plugin-linux-amd64       ./server
	GOOS=linux   GOARCH=arm64 go build -o $(STAGE)/server/plugin-linux-arm64       ./server
	GOOS=darwin  GOARCH=amd64 go build -o $(STAGE)/server/plugin-darwin-amd64      ./server
	GOOS=darwin  GOARCH=arm64 go build -o $(STAGE)/server/plugin-darwin-arm64      ./server
	GOOS=windows GOARCH=amd64 go build -o $(STAGE)/server/plugin-windows-amd64.exe ./server
	cd $(KANDEV_SDK) && go run ./cmd/plugin-pack -dir $(CURDIR)/$(STAGE) -out $(CURDIR)/$(PKG_OUT)
	rm -rf $(STAGE)
	@echo "Wrote $(PKG_OUT)"

## Package only the current platform for faster local iteration.
package-host:
	rm -rf $(STAGE)
	mkdir -p $(STAGE)/server
	cp manifest.yaml $(STAGE)/manifest.yaml
	cp -r ui $(STAGE)/ui
	go build -o $(STAGE)/server/plugin-$$(go env GOOS)-$$(go env GOARCH)$$(go env GOEXE) ./server
	cd $(KANDEV_SDK) && go run ./cmd/plugin-pack -dir $(CURDIR)/$(STAGE) -out $(CURDIR)/$(PKG_OUT) -platform-only
	rm -rf $(STAGE)
	@echo "Wrote $(PKG_OUT)"

package-file:
	@printf '%s\n' "$(PKG_OUT)"

## Package for this machine and check its manifest, exact file list, and hashes.
verify-package-host: package-host
	@set -eu; \
		tmp="$$(mktemp -d)"; \
		trap 'rm -rf "$$tmp"' EXIT; \
		tar -xzf "$(PKG_OUT)" -C "$$tmp"; \
		sh scripts/verify-package.sh "$$tmp" host "$$(go env GOOS)-$$(go env GOARCH)"

## Build every declared platform and check its manifest, exact file list, and hashes.
verify-package: package
	@set -eu; \
		tmp="$$(mktemp -d)"; \
		trap 'rm -rf "$$tmp"' EXIT; \
		tar -xzf "$(PKG_OUT)" -C "$$tmp"; \
		sh scripts/verify-package.sh "$$tmp" full

clean:
	rm -rf bin $(STAGE) kandev-session-cost-*.tar.gz
