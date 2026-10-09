# aioz-common Makefile.
# Lint targets applied from team-playbook's init-source template:
# /home/tuan/work/team-playbook/templates/golangci-lint/Makefile.snippet
# Uses the same golangci-lint version pin as .gitlab-ci.yml so `make lint`
# locally reproduces the CI check exactly.

GOLANGCI_LINT_VERSION := v2.11.4
GOLANGCI_LINT := $(shell go env GOPATH)/bin/golangci-lint

.PHONY: lint lint-fix fmt test test-adapters test-integration

# Nested modules: each framework adapter is its own module, so ./... from the
# root does not reach it.
ADAPTERS := httpapi/echoapi httpapi/ginapi

$(GOLANGCI_LINT):
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
		| sh -s -- -b "$(shell go env GOPATH)/bin" "$(GOLANGCI_LINT_VERSION)"

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...
	for m in $(ADAPTERS); do (cd $$m && $(GOLANGCI_LINT) run --config $(CURDIR)/.golangci.yml ./...) || exit 1; done

lint-fix: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --fix ./...

fmt:
	go fmt ./...

test:
	go test ./...

test-adapters:
	for m in $(ADAPTERS); do (cd $$m && go vet ./... && go test -race ./...) || exit 1; done

# The container tests, with skipping turned off so a missing docker daemon is
# a failure rather than a green run that tested nothing.
test-integration:
	AIOZ_REQUIRE_DOCKER=1 go test -race -timeout 20m ./redis/... ./rabbitmq/... ./postgres/... ./ratelimit/...
