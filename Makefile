# Boring make targets. The build is `go build`; this file exists so that the
# flags a release needs are written down once rather than remembered.

VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet fmt check image clean

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

clean:
	rm -f je
	go clean -testcache
