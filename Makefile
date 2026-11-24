BINARY := antigravity-zectrix
PKG := ./cmd/antigravity-zectrix

.PHONY: build test preview clean linux

build:
	go build -trimpath -ldflags="-s -w" -o bin/$(BINARY) $(PKG)

linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/$(BINARY)-linux-amd64 $(PKG)

test:
	go test -v ./...

preview:
	go run $(PKG) --preview preview.png

clean:
	rm -rf bin/ preview.png
