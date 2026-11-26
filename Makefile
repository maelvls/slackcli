SKILLS_DIR ?= $(HOME)/.claude/skills
SKILL_DEST := $(SKILLS_DIR)/slackcli

.PHONY: install install-bin install-skill test

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
