BIN := agent-coordinator

build:
	go build -o $(BIN) ./cmd/agent-coordinator

test:
	go test ./...

install: build
	./$(BIN) install

clean:
	rm -f $(BIN)

.PHONY: build test install clean
