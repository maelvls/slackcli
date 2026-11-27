SKILLS_DIR ?= $(HOME)/.claude/skills
SKILL_DEST := $(SKILLS_DIR)/slackcli

.PHONY: install install-bin install-skill test check fmt-check vet test-race cross-build vulncheck

# Installs the binary into $(go env GOBIN) or ~/go/bin, and the agent skill
# into $(SKILLS_DIR).
install: install-bin install-skill

install-bin:
	go install ./cmd/slackcli

# Replaces the installed copy so that files removed from skills/ go away too.
# A symlink (see the README) already follows the checkout and is left alone.
install-skill:
	@if [ -L "$(SKILL_DEST)" ]; then \
		echo "$(SKILL_DEST) is a symlink, leaving it alone"; \
	else \
		mkdir -p "$(SKILLS_DIR)" && \
		rm -rf "$(SKILL_DEST)" && \
		cp -R skills/slackcli "$(SKILL_DEST)" && \
		echo "skill installed in $(SKILL_DEST)"; \
	fi

test:
	go test ./...

# What CI runs (.github/workflows/ci.yml). The cross-builds cover the per-OS
# files (keychain on macOS, DPAPI on Windows, flock on Unix).
check: fmt-check vet test-race cross-build

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "not gofmt-ed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test-race:
	go test -race -count=1 ./...

CROSS_TARGETS := linux/amd64 darwin/arm64 windows/amd64

# Builds and vets every package for each target, test files included.
cross-build:
	@set -e; for t in $(CROSS_TARGETS); do \
		echo "GOOS=$${t%/*} GOARCH=$${t#*/}"; \
		CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go build ./...; \
		CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go vet ./...; \
	done

GOVULNCHECK_VERSION := v1.10.0

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
