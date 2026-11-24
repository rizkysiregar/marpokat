# Marpokat Android SDK: test, build and publish (GitHub Packages, Maven).
#
#   make test        unit tests of both modules
#   make publish     test, check the version is new, then publish it
#   make local       publish to ~/.m2 for an app built with -Pmarpokat.local
#
# Publishing needs a token with write:packages: GITHUB_PACKAGES_TOKEN in the environment, or the GitHub CLI's token
# (gh auth refresh -s write:packages,read:packages once). The token is never printed. The version is `version` in
# build.gradle.kts: raise it first, a published version cannot be replaced.

OWNER        ?= rizkysiregar
GROUP        := com.rizkysiregar.marpokat
ARTIFACTS    := marpokat-core marpokat-android
VERSION      := $(shell sed -n 's/^ *version = "\(.*\)"/\1/p' build.gradle.kts)
# a JDK 21 (the SDK's toolchain); JAVA_HOME in the environment wins
JAVA_HOME    ?= $(shell /usr/libexec/java_home -v 21 2>/dev/null)
export JAVA_HOME

TOKEN = $(or $(GITHUB_PACKAGES_TOKEN),$(shell gh auth token 2>/dev/null))

.PHONY: help
help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'
	@echo "  version: $(VERSION)"

.PHONY: version
version: ## print the version that would be published
	@echo $(VERSION)

.PHONY: test
test: ## unit tests (core and android)
	./gradlew :core:test :android:testDebugUnitTest

.PHONY: build
build: ## build both artifacts (jar and release aar)
	./gradlew :core:assemble :android:assembleRelease

.PHONY: local
local: ## publish $(VERSION) to the local Maven repository (~/.m2)
	./gradlew publishToMavenLocal
	@echo "published $(GROUP) $(VERSION) to ~/.m2: build the app with -Pmarpokat.local"

.PHONY: check-token
check-token:
	@test -n "$(TOKEN)" || { echo "no token: set GITHUB_PACKAGES_TOKEN, or run: gh auth login && gh auth refresh -s write:packages,read:packages"; exit 1; }

.PHONY: check-clean
check-clean:
	@git diff --quiet HEAD -- . || { echo "uncommitted changes in sdk-android: commit them first, the published version must match a commit"; exit 1; }

.PHONY: check-version
check-version: check-token ## fail if $(VERSION) is already published
	@for a in $(ARTIFACTS); do \
	  if GH_TOKEN="$(TOKEN)" gh api "/users/$(OWNER)/packages/maven/$(GROUP).$$a/versions" --paginate --jq '.[].name' 2>/dev/null | grep -qx "$(VERSION)"; then \
	    echo "$$a $(VERSION) is already published: raise version in build.gradle.kts"; exit 1; \
	  fi; \
	done; echo "$(VERSION) is new"

.PHONY: publish
publish: check-clean check-version test ## test and publish $(VERSION) to GitHub Packages
	@GITHUB_ACTOR="$(OWNER)" GITHUB_TOKEN="$(TOKEN)" ./gradlew publishAllPublicationsToGitHubPackagesRepository
	@echo "published $(GROUP):{$(subst $() ,$(comma),$(ARTIFACTS))}:$(VERSION)"

comma := ,
