BINARY := jindo-mcp
CMD    := ./cmd/jindo-mcp

.PHONY: build test install smoke clean

## build: compile the jindo-mcp binary into ./jindo-mcp
build:
	go build -o $(BINARY) $(CMD)

## test: run the full test suite
test:
	go test ./...

## install: build + register with detected MCP hosts (Claude Code / Codex / agy)
install: build
	./install.sh

## smoke: MCP handshake check against the built binary
smoke: build
	@printf '%s\n%s\n' \
	  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
	  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | ./$(BINARY) | grep -q '"jindo-mcp"' \
	  && echo "smoke OK: jindo-mcp responds" || (echo "smoke FAIL" && exit 1)

## clean: remove the built binary
clean:
	rm -f $(BINARY)
