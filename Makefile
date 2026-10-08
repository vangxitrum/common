# aioz-common Makefile.
# Lint targets applied from team-playbook's init-source template:
# /home/tuan/work/team-playbook/templates/golangci-lint/Makefile.snippet
# Uses the same golangci-lint version pin as .gitlab-ci.yml so `make lint`
# locally reproduces the CI check exactly.

GOLANGCI_LINT_VERSION := v2.11.4
GOLANGCI_LINT := $(shell go env GOPATH)/bin/golangci-lint

# Nested modules: each framework adapter is its own module, so ./... from the
# root does not reach it.
ADAPTERS := httpapi/echoapi httpapi/ginapi
.PHONY: lint lint-fix fmt test test-adapters test-integration tools

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

# The internal tools under cmd/, cross-compiled for every platform people run
# them on, with a SHA256SUMS to check a download against. Pure Go and
# CGO_ENABLED=0, so each binary is static and runs on any distribution.
# -trimpath keeps the builder's paths out of the binary; the version comes from
# the VCS stamp Go embeds, read by aioz-common/version.
TOOLS := aiozkey
TOOLS_PLATFORMS := linux/amd64 linux/arm64 darwin/arm64
TOOLS_DIR := dist

tools:
	rm -rf $(TOOLS_DIR)
	set -e; for tool in $(TOOLS); do \
		for platform in $(TOOLS_PLATFORMS); do \
			os=$${platform%/*}; arch=$${platform#*/}; \
			echo "build $$tool $$os/$$arch"; \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
				-o $(TOOLS_DIR)/$$tool/$$os-$$arch/$$tool ./cmd/$$tool; \
		done; \
		(cd $(TOOLS_DIR)/$$tool && find . -type f -name $$tool | sort | sed 's|^\./||' \
			| xargs sha256sum > SHA256SUMS); \
	done
