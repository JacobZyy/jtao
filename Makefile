BINARY := bin/jtao

.PHONY: build test test-race clean install plugin-reinstall

build:
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/jtao

test:
	go test ./...

test-race:
	go test -race ./...

install: build
	mkdir -p $${HOME}/.local/bin
	rm -f $${HOME}/.local/bin/jtao
	cp $(BINARY) $${HOME}/.local/bin/jtao

plugin-reinstall: install
	codex plugin remove jtao@jtao || true
	codex plugin add jtao@jtao

clean:
	rm -rf bin/
