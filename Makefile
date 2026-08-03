BINARY  := walletspace
# git describe so a locally built binary reports something traceable; the release
# build gets the tag from GoReleaser instead. Both are recursively expanded, so
# `git describe` runs only for the targets that actually stamp a binary and not
# on every `make clean`.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -ldflags "-s -w -X main.version=$(VERSION)"

.PHONY: start build install test lint fmt clean snapshot release

## Run from source, with the same version stamp `make build` produces
start:
	go run $(LDFLAGS) ./cmd/walletspace

## Build ./bin/walletspace with the version stamped in
build:
	go build $(LDFLAGS) -o bin/$(BINARY) ./cmd/walletspace

## Install walletspace into $(GOBIN)
install:
	go install $(LDFLAGS) ./cmd/walletspace

test:
	go test ./... -count=1 -race

lint:
	go vet ./...

fmt:
	go fix ./...
	gofumpt -l -w .

clean:
	rm -rf bin/ dist/

## Build every release artifact and the cask into ./dist without publishing
# Useful for looking at what a tag would produce, including Casks/walletspace.rb,
# before the tag exists. Needs goreleaser: brew install goreleaser.
snapshot:
	goreleaser release --snapshot --clean

# Release
release:
	@if [ -z "$(TAG)" ]; then echo "Usage: make release TAG=v1.2.3"; exit 1; fi
	@# GoReleaser refuses a tag it cannot read as semver, and by then the tag is
	@# already pushed, so reject it here instead. The pattern is semver's own
	@# grammar: a hyphen is legal inside a pre-release identifier and build
	@# metadata is legal after a plus, and GoReleaser accepts both.
	@printf '%s' "$(TAG)" | \
		grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$$' || \
		{ echo "TAG must be semver: v1.2.3, v1.2.3-rc.1, v1.2.3+build.7"; exit 1; }
	@# The workflow builds the tagged commit and publishes a wallet binary from
	@# it, so what is tagged has to be reviewable afterwards: no uncommitted work
	@# in the build, and nothing that only exists in this clone.
	@git diff-index --quiet HEAD -- || \
		{ echo "the working tree is dirty; commit or stash before releasing"; exit 1; }
	@git fetch --quiet origin master
	@git merge-base --is-ancestor HEAD FETCH_HEAD || \
		{ echo "HEAD is not on origin/master, which is the only branch CI gates"; exit 1; }
	git tag -a $(TAG) -m "Release $(TAG)"
	git push origin $(TAG)
