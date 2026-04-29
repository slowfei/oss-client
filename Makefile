.PHONY: test test-no-docker vet fmt tidy add-provider release release-notes

# Modules walked by tidy / multi-module-aware targets. Mirror the set
# tagged by scripts/release.sh (12 modules: root + testkit + 10 providers).
MODULES = . \
	./pkg/testkit/contract \
	./providers/alibaba \
	./providers/aws \
	./providers/azure \
	./providers/gcs \
	./providers/huawei \
	./providers/minio \
	./providers/qiniu \
	./providers/tencent \
	./providers/upyun \
	./providers/volcengine

# test runs the full test suite (root + every module), including Docker-gated
# cases (the docker tag is opt-in per module via go test -tags=docker).
test:
	go test ./... -race

# test-no-docker runs only the tests that do not require a Docker daemon.
test-no-docker:
	go test ./... -race -short

vet:
	go vet ./...

fmt:
	gofmt -w .

# tidy runs `go mod tidy` in every module so go.mod / go.sum stay in sync
# across the workspace (root + testkit + providers/<name>). Mirrors the
# multi-module setup documented in RELEASING.md §1.
tidy:
	@for m in $(MODULES); do \
		echo "==> go mod tidy: $$m"; \
		(cd $$m && go mod tidy) || exit 1; \
	done

# add-provider scaffolds a new provider module under providers/<name> and wires
# it into go.work. Usage: make add-provider NAME=<name>
add-provider:
	@if [ -z "$(NAME)" ]; then echo "usage: make add-provider NAME=<name>"; exit 2; fi
	./scripts/add-provider.sh $(NAME)

# release runs the synchronized-bump release flow: bumps every go.mod
# require line + go.work replace block to VERSION, renames CHANGELOG
# [Unreleased] → [VERSION], commits as a release-prep, creates 12 git
# tags (1 bare root + 1 testkit + 10 providers) at the prep commit.
# Push + GitHub Release creation are maintainer actions printed at the
# end. See RELEASING.md §3 and §6 for the binding policy.
#
# Usage: make release VERSION=v0.2.0
# Optional: SKIP_TESTS=1 make release VERSION=v0.2.0
release:
	@if [ -z "$(VERSION)" ]; then echo "usage: make release VERSION=vX.Y.Z"; exit 2; fi
	./scripts/release.sh $(VERSION)

# release-notes emits the GitHub Release body markdown for VERSION to stdout.
# Pipe into `gh release create --notes-file` (or capture to a file first).
# Usage: make release-notes VERSION=v0.2.0
release-notes:
	@if [ -z "$(VERSION)" ]; then echo "usage: make release-notes VERSION=vX.Y.Z"; exit 2; fi
	@./scripts/release-notes.sh $(VERSION)
