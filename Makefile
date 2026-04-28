.PHONY: test test-no-docker vet fmt tidy add-provider

# Modules walked by tidy / multi-module-aware targets. Update when a new
# provider module lands (scripts/add-provider.sh registers it in go.work
# automatically; add it here too so multi-module ergonomics stay clean).
MODULES = . ./pkg/testkit/contract ./providers/aws ./providers/minio

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
