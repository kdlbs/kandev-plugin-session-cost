.PHONY: build test fmt vet package package-host clean

BIN := bin/kandev-session-cost
VERSION := 0.1.0
STAGE := .build/stage
PKG_OUT := kandev-session-cost-$(VERSION).tar.gz

## Build the plugin binary for the host platform (development use; the
## installed-plugin path always goes through `make package`/`package-host`).
build:
	mkdir -p bin
	go build -o $(BIN) ./server

test:
	go test ./server/...

fmt:
	gofmt -l .

vet:
	go vet ./server/...

## Cross-compile server/plugin-<goos>-<goarch>[.exe] for every platform
## declared in manifest.yaml's runtime.executables, stage manifest.yaml + ui/
## alongside them, and pack the tree with the kandev repo's plugin-pack
## (resolved via this repo's local `replace` directive). Install via
## Settings > Plugins (upload) or
## `curl -F package=@$(PKG_OUT) http://localhost:<port>/api/plugins/install`.
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
	go run github.com/kandev/kandev/cmd/plugin-pack -dir $(STAGE) -out $(PKG_OUT)
	rm -rf $(STAGE)
	@echo "Wrote $(PKG_OUT)"

## Package for the host platform only — faster local iteration than the full
## 5-platform `make package`.
package-host:
	rm -rf $(STAGE)
	mkdir -p $(STAGE)/server
	cp manifest.yaml $(STAGE)/manifest.yaml
	cp -r ui $(STAGE)/ui
	go build -o $(STAGE)/server/plugin-$$(go env GOOS)-$$(go env GOARCH)$$(go env GOEXE) ./server
	go run github.com/kandev/kandev/cmd/plugin-pack -dir $(STAGE) -out $(PKG_OUT) -platform-only
	rm -rf $(STAGE)
	@echo "Wrote $(PKG_OUT)"

clean:
	rm -rf bin $(STAGE) kandev-session-cost-*.tar.gz
