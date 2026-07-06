# Agent Coordinator Go Makefile

# Build variables
BINARY_NAME=agent-coordinator
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0")
BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
GO_VERSION=$(shell go version | cut -d ' ' -f 3)

# Directories
BUILD_DIR=./build
CMD_DIR=./cmd/agent-coordinator

# Go build flags
LDFLAGS=-ldflags "-X main.version=${VERSION} -X main.buildTime=${BUILD_TIME} -X main.goVersion=${GO_VERSION}"
BUILD_FLAGS=-trimpath $(LDFLAGS)

# Default target
.PHONY: all
all: clean build

# Build the application
.PHONY: build
build:
	@echo "Building $(BINARY_NAME) $(VERSION)..."
	@mkdir -p $(BUILD_DIR)
	go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(CMD_DIR)

# Build for multiple platforms
.PHONY: build-all
build-all: clean
	@echo "Building for multiple platforms..."
	@mkdir -p $(BUILD_DIR)
	
	# Linux AMD64
	GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 $(CMD_DIR)
	
	# Linux ARM64
	GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 $(CMD_DIR)
	
	# macOS AMD64
	GOOS=darwin GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 $(CMD_DIR)
	
	# macOS ARM64 (Apple Silicon)
	GOOS=darwin GOARCH=arm64 go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(CMD_DIR)
	
	# Windows AMD64
	GOOS=windows GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe $(CMD_DIR)

# Install dependencies
.PHONY: deps
deps:
	@echo "Installing dependencies..."
	go mod download
	go mod tidy

# Run tests
.PHONY: test
test:
	@echo "Running tests..."
	go test -v -race ./...

# Run tests with coverage
.PHONY: test-coverage
test-coverage:
	@echo "Running tests with coverage..."
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Run benchmarks
.PHONY: benchmark
benchmark:
	@echo "Running benchmarks..."
	go test -bench=. -benchmem ./...

# Lint the code
.PHONY: lint
lint:
	@echo "Linting code..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed. Install it from https://golangci-lint.run/"; \
		go vet ./...; \
	fi

# Format the code
.PHONY: fmt
fmt:
	@echo "Formatting code..."
	go fmt ./...

# Generate documentation
.PHONY: docs
docs:
	@echo "Generating documentation..."
	@mkdir -p docs/api
	go doc -all > docs/api/godoc.md

# Run the application
.PHONY: run
run: build
	@echo "Starting Agent Coordinator..."
	$(BUILD_DIR)/$(BINARY_NAME)

# Run the demo
.PHONY: demo
demo:
	@echo "Running workflow demo..."
	go run ./examples/workflow-demo/main.go

# Clean build artifacts
.PHONY: clean
clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(BUILD_DIR)
	rm -f coverage.out coverage.html

# Install the binary
.PHONY: install
install: build
	@echo "Installing $(BINARY_NAME)..."
	sudo cp $(BUILD_DIR)/$(BINARY_NAME) /usr/local/bin/

# Uninstall the binary
.PHONY: uninstall
uninstall:
	@echo "Uninstalling $(BINARY_NAME)..."
	sudo rm -f /usr/local/bin/$(BINARY_NAME)

# Create a development environment
.PHONY: dev-env
dev-env:
	@echo "Setting up development environment..."
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install golang.org/x/tools/cmd/goimports@latest

# Docker build
.PHONY: docker-build
docker-build:
	@echo "Building Docker image..."
	docker build -t agent-coordinator:$(VERSION) .

# Docker run
.PHONY: docker-run
docker-run: docker-build
	@echo "Running Docker container..."
	docker run --rm -it agent-coordinator:$(VERSION)

# Check for security vulnerabilities
.PHONY: security
security:
	@echo "Checking for security vulnerabilities..."
	@if command -v gosec >/dev/null 2>&1; then \
		gosec ./...; \
	else \
		echo "gosec not installed. Install it with: go install github.com/securecodewarrior/gosec/v2/cmd/gosec@latest"; \
	fi

# Validate go.mod
.PHONY: mod-verify
mod-verify:
	@echo "Verifying go.mod..."
	go mod verify

# Update dependencies
.PHONY: update-deps
update-deps:
	@echo "Updating dependencies..."
	go get -u ./...
	go mod tidy

# Show version
.PHONY: version
version:
	@echo "Version: $(VERSION)"
	@echo "Build Time: $(BUILD_TIME)"
	@echo "Go Version: $(GO_VERSION)"

# Release (tag and build)
.PHONY: release
release: test lint
	@echo "Creating release $(VERSION)..."
	git tag -a $(VERSION) -m "Release $(VERSION)"
	$(MAKE) build-all

# Help target
.PHONY: help
help:
	@echo "Available targets:"
	@echo "  all          - Clean and build the application"
	@echo "  build        - Build the application for current platform"
	@echo "  build-all    - Build for all supported platforms"
	@echo "  deps         - Install dependencies"
	@echo "  test         - Run tests"
	@echo "  test-coverage- Run tests with coverage report"
	@echo "  benchmark    - Run benchmarks"
	@echo "  lint         - Lint the code"
	@echo "  fmt          - Format the code"
	@echo "  docs         - Generate documentation"
	@echo "  run          - Build and run the application"
	@echo "  demo         - Run the workflow demo"
	@echo "  clean        - Clean build artifacts"
	@echo "  install      - Install the binary system-wide"
	@echo "  uninstall    - Uninstall the binary"
	@echo "  dev-env      - Set up development environment"
	@echo "  docker-build - Build Docker image"
	@echo "  docker-run   - Build and run Docker container"
	@echo "  security     - Check for security vulnerabilities"
	@echo "  mod-verify   - Verify go.mod"
	@echo "  update-deps  - Update dependencies"
	@echo "  version      - Show version information"
	@echo "  release      - Create a release"
	@echo "  help         - Show this help message"