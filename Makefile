BINARY_NAME := taskpilot
BIN_DIR := bin
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: build install uninstall verify-install test

build:
	mkdir -p "$(BIN_DIR)"
	go build -o $(BIN_DIR)/$(BINARY_NAME) ./cmd/taskpilot

install: build
	mkdir -p "$(INSTALL_DIR)"
	ln -sf "$(CURDIR)/$(BIN_DIR)/$(BINARY_NAME)" "$(INSTALL_DIR)/$(BINARY_NAME)"
	@echo "Installed $(BINARY_NAME) -> $(INSTALL_DIR)/$(BINARY_NAME)"
	@echo "Make sure $(INSTALL_DIR) is on PATH before other TaskPilot installs:"
	@echo '  export PATH="$(INSTALL_DIR):$$PATH"'

uninstall:
	rm -f "$(INSTALL_DIR)/$(BINARY_NAME)"

verify-install:
	@command -v $(BINARY_NAME)
	@ls -l "$$(command -v $(BINARY_NAME))"
	@$(BINARY_NAME) config show || true

test:
	go test ./...
