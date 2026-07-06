BIN := agent-coordinator
PREFIX := $(HOME)/.local

build:
	go build -o $(BIN) ./cmd/agent-coordinator

test:
	go test ./...

install: build
	install -D -m 0755 $(BIN) $(PREFIX)/bin/$(BIN)
	$(PREFIX)/bin/$(BIN) install
	systemctl --user try-restart agent-coordinator.service

uninstall:
	$(PREFIX)/bin/$(BIN) install --uninstall
	rm -f $(PREFIX)/bin/$(BIN)

clean:
	rm -f $(BIN)

.PHONY: build test install uninstall clean
