BINARY     := p2p-ci-proxy
BUILD_DIR  := ./bin
CMD        := ./cmd/proxy
GOFLAGS    := -trimpath -ldflags="-s -w"

.PHONY: all build test lint run clean install setup

all: build

build:
	@mkdir -p $(BUILD_DIR)
	go build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY) $(CMD)
	@echo "Built $(BUILD_DIR)/$(BINARY)"

test:
	go test ./... -race -count=1

lint:
	@which golangci-lint > /dev/null || (echo "Install golangci-lint: https://golangci-lint.run/usage/install/" && exit 1)
	golangci-lint run ./...

# Run with default config (stdout logging)
run:
	go run $(CMD) 

# Run with a config file
run-config:
	go run $(CMD) --config config.example.yaml

install: build
	cp $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)
	@echo "Installed to /usr/local/bin/$(BINARY)"

# Print the npm/pip/cargo configuration snippets
setup:
	@$(BUILD_DIR)/$(BINARY) --help 2>/dev/null || go run $(CMD) &
	@sleep 1
	@curl -s http://127.0.0.1:7878/_p2pci/health | python3 -m json.tool

clean:
	rm -rf $(BUILD_DIR)
	go clean ./...

tidy:
	go mod tidy
	go mod verify

# Quick smoke test: start proxy and test cache round-trip with a known npm package
smoke:
	@echo "Starting proxy in background..."
	@go run $(CMD) &
	@sleep 1
	@echo "Fetching via proxy (cold)..."
	@curl -s -o /dev/null -w "Status: %{http_code}  Time: %{time_total}s\n" \
		"http://127.0.0.1:7878/https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz"
	@echo "Fetching via proxy (should be cache hit)..."
	@curl -s -o /dev/null -w "Status: %{http_code}  Time: %{time_total}s  Cache: %{header_x-p2pci-cache}\n" \
		"http://127.0.0.1:7878/https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz"
	@pkill -f "p2p-ci-proxy" || true
