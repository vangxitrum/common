# aioz-common Makefile.
# Lint targets applied from team-playbook's init-source template:
# /home/tuan/work/team-playbook/templates/golangci-lint/Makefile.snippet
# Uses the same golangci-lint version pin as .gitlab-ci.yml so `make lint`
# locally reproduces the CI check exactly.

GOLANGCI_LINT_VERSION := v2.11.4
GOLANGCI_LINT := $(shell go env GOPATH)/bin/golangci-lint

# Nested modules: each framework adapter is its own module, and so is each
# tool under tools/, so ./... from the root does not reach them.
ADAPTERS := httpapi/echoapi httpapi/ginapi
# A tool is its own module so `go install <path>@<version>` works: the root
# go.mod carries a replace directive (gogo/protobuf, for go-sdk), and go install
# refuses a module that has one. Tested with GOWORK=off, against the released
# aioz-common its go.mod requires, because that is what an install builds.
TOOLS := tools/aiozkey
.PHONY: lint lint-fix lint-modules fmt test test-adapters test-tools test-integration

$(GOLANGCI_LINT):
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
		| sh -s -- -b "$(shell go env GOPATH)/bin" "$(GOLANGCI_LINT_VERSION)"

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...
	$(MAKE) lint-modules

lint-modules: $(GOLANGCI_LINT)
	for m in $(ADAPTERS) $(TOOLS); do (cd $$m && GOWORK=off $(GOLANGCI_LINT) run --config $(CURDIR)/.golangci.yml ./...) || exit 1; done

lint-fix: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --fix ./...

fmt:
	go fmt ./...

test:
	go test ./...

test-adapters:
	for m in $(ADAPTERS); do (cd $$m && go vet ./... && go test -race ./...) || exit 1; done

test-tools:
	for m in $(TOOLS); do (cd $$m && GOWORK=off go vet ./... && GOWORK=off go test -race ./...) || exit 1; done

# The container tests, with skipping turned off so a missing docker daemon is
# a failure rather than a green run that tested nothing.
test-integration:
	AIOZ_REQUIRE_DOCKER=1 go test -race -timeout 20m ./redis/... ./rabbitmq/... ./postgres/... ./ratelimit/...
