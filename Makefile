BINARY  := chalk
PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := build

.PHONY: build install uninstall service check vet fmt test clean

build:
	go build -ldflags "$(LDFLAGS)" -o "$(BINARY)" .

install: build
	mkdir -p -- "$(BINDIR)"
	install -m 0755 "$(BINARY)" "$(BINDIR)/$(BINARY)"
	@echo
	@echo "installed $(BINDIR)/$(BINARY)"
	@command -v "$(BINARY)" >/dev/null 2>&1 || echo "warning: $(BINDIR) is not on your PATH"
	@echo "run it now:            chalk"
	@echo "or run it in the background:  chalk service install && chalk service start"

# Install the binary, then register and start it as a per-user background service.
service: install
	"$(BINDIR)/$(BINARY)" service install
	"$(BINDIR)/$(BINARY)" service start
	"$(BINDIR)/$(BINARY)" service status

uninstall:
	@if "$(BINDIR)/$(BINARY)" service status >/dev/null 2>&1; then \
		echo "stopping and removing the background service"; \
		"$(BINDIR)/$(BINARY)" service stop || echo "warning: stop failed, removing anyway"; \
		"$(BINDIR)/$(BINARY)" service uninstall; \
	fi
	rm -f -- "$(BINDIR)/$(BINARY)"
	@echo "removed $(BINDIR)/$(BINARY)"
	@echo "the board database was left in place"

check: fmt vet test

fmt:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -f -- "$(BINARY)"
