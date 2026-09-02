# Boring make targets. The build is `go build`; this file exists so that the
# flags a release needs are written down once rather than remembered.

VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet fmt check image release-dry clean

build: ## build ./je for this machine
	go build -trimpath -ldflags "$(LDFLAGS)" -o je ./cmd/je

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# What CI runs, and what to run before asking for a review.
check: fmt vet test

image:
	docker build --build-arg VERSION=$(VERSION) -t job-engine:$(VERSION) .

# Build every release artifact locally, exactly as the workflow does.
#
# Worth having as a target rather than only in CI: a cross-compilation failure
# found at tag time is found at the worst possible moment, and this is the
# thirty seconds that prevents it.
release-dry:
	rm -rf dist && mkdir -p dist
	@for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do \
		goos=$${target%/*}; goarch=$${target#*/}; \
		echo "building $$goos/$$goarch"; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -trimpath \
			-ldflags "$(LDFLAGS)" -o dist/je ./cmd/je; \
		tar -czf "dist/je_$(VERSION:v%=%)_$${goos}_$${goarch}.tar.gz" -C dist je; \
		rm dist/je; \
	done
	cd dist && shasum -a 256 *.tar.gz > checksums.txt
	@echo && ls -lh dist

clean:
	rm -rf je dist
	go clean -testcache
