# Boring make targets. The build is `go build`; this file exists so that the
# flags a release needs are written down once rather than remembered.

VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test vet fmt check image release-dry clean web web-build

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

# The web client (D23). A second client of the same API, so it needs a control
# plane running and nothing else. JE_ADDR points it somewhere other than the
# default.
# The Vite dev server, for iterating on the client with hot reload. It proxies
# /v1 to a control plane; `je web run` is the shipped path and needs no npm.
web:
	cd web && npm install && JE_ADDR=$${JE_ADDR:-http://127.0.0.1:7620} npm run dev

# Rebuild the assets the binary embeds. The output lands in internal/webui/dist
# and is committed, so this is only needed after changing anything under web/.
web-build:
	cd web && npm install && npm run build

image:
	docker build --build-arg VERSION=$(VERSION) -t job-engine:$(VERSION) .

# Build and install onto this machine, restarting what runs here.
#
# The loop for trying a change on your own deployment. Going through a tag, a
# workflow and a download to find out whether something worked costs ten
# minutes, and a ten minute loop is not a loop -- people stop testing on their
# own deployment, which is the worst outcome available.
#
# It is `je upgrade` underneath, so the install is the same one a release does:
# the same refusal to write over a package manager's file, the same atomic
# replace, and the same restart of the control plane and worker here. The
# installed `je` does the upgrading, not the one just built -- a binary cannot
# replace itself, and the one on PATH is the one being replaced.
#
# Two cases need the copy instead. Nothing is installed yet, so there is nothing
# to upgrade -- and the installed one predates --from, which is the bootstrap
# every self-installing tool has exactly once and which nobody thinks about
# until they hit it: the first `make install` after adding the flag is run by a
# binary that does not have it. The new one then restarts the deployment
# itself, which is the half that mattered.
JE_INSTALL_DIR ?= $(HOME)/.local/bin
install: build
	@if command -v je >/dev/null 2>&1 && je upgrade --help 2>&1 | grep -q -- '-from'; then \
		je upgrade --from ./je; \
	else \
		target="$$(command -v je || echo '$(JE_INSTALL_DIR)/je')"; \
		install -d "$$(dirname "$$target")" && install -m 0755 ./je "$$target" && \
		echo "installed $$target by copying: the je already there is older than --from"; \
		"$$target" upgrade --restart --yes || true; \
	fi

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
