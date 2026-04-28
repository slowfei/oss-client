.PHONY: test test-no-docker vet fmt add-provider

# test runs the full test suite, including Docker-gated cases.
test:
	go test ./... -race

# test-no-docker runs only the tests that do not require a Docker daemon.
test-no-docker:
	go test ./... -race -short

vet:
	go vet ./...

fmt:
	gofmt -w .

# add-provider scaffolds a new provider module under providers/<name> and wires
# it into go.work. Usage: make add-provider NAME=<name>
add-provider:
	@if [ -z "$(NAME)" ]; then echo "usage: make add-provider NAME=<name>"; exit 2; fi
	./scripts/add-provider.sh $(NAME)
