# Marpokat JS SDK: test, build and publish (GitHub Packages, npm).
#
#   make test        typecheck and unit tests
#   make publish     test, check the version is new, then publish core and react
#
# Publishing needs a token with write:packages: GITHUB_PACKAGES_TOKEN in the environment, or the GitHub CLI's token
# (gh auth refresh -s write:packages,read:packages once). The token goes into a temporary npm config, never into the
# repository, and is never printed. The versions are in packages/*/package.json: raise them first.

OWNER     ?= rizkysiregar
PACKAGES  := core react
VERSION   := $(shell node -p "require('./packages/core/package.json').version")
TOKEN = $(or $(GITHUB_PACKAGES_TOKEN),$(shell gh auth token 2>/dev/null))

.PHONY: help
help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'
	@echo "  version: $(VERSION)"

.PHONY: install
install: ## npm ci
	npm ci

.PHONY: test
test: ## typecheck and unit tests
	npm run typecheck && npm test

.PHONY: build
build: ## build both packages
	npm run build

.PHONY: check-token
check-token:
	@test -n "$(TOKEN)" || { echo "no token: set GITHUB_PACKAGES_TOKEN, or run: gh auth login && gh auth refresh -s write:packages,read:packages"; exit 1; }

.PHONY: check-clean
check-clean:
	@git diff --quiet HEAD -- . || { echo "uncommitted changes in sdk-js: commit them first"; exit 1; }

.PHONY: check-version
check-version: check-token ## fail if a package's version is already published
	@for p in $(PACKAGES); do \
	  v=$$(node -p "require('./packages/$$p/package.json').version"); \
	  if GH_TOKEN="$(TOKEN)" gh api "/users/$(OWNER)/packages/npm/marpokat-$$p/versions" --paginate --jq '.[].name' 2>/dev/null | grep -qx "$$v"; then \
	    echo "@$(OWNER)/marpokat-$$p $$v is already published: raise its version"; exit 1; \
	  fi; \
	done; echo "versions are new"

.PHONY: publish
publish: check-clean check-version test build ## test, build and publish both packages to GitHub Packages
	@cfg=$$(mktemp); trap 'rm -f $$cfg' EXIT; \
	printf '@$(OWNER):registry=https://npm.pkg.github.com\n//npm.pkg.github.com/:_authToken=%s\n' "$(TOKEN)" > $$cfg; \
	for p in $(PACKAGES); do npm publish -w @$(OWNER)/marpokat-$$p --userconfig $$cfg || exit 1; done
	@echo "published @$(OWNER)/marpokat-{core,react}"
