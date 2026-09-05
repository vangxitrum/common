# aioz-stats Makefile.
# Lint targets applied from team-playbook's init-source template:
# /home/tuan/work/team-playbook/templates/golangci-lint/Makefile.snippet
# Uses the same golangci-lint version pin as .gitlab-ci.yml so `make lint`
# locally reproduces the CI check exactly.

GOLANGCI_LINT_VERSION := v2.11.4
GOLANGCI_LINT := $(shell go env GOPATH)/bin/golangci-lint

.PHONY: lint lint-fix fmt test

$(GOLANGCI_LINT):
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
		| sh -s -- -b "$(shell go env GOPATH)/bin" "$(GOLANGCI_LINT_VERSION)"

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...

lint-fix: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --fix ./...

fmt:
	go fmt ./...

test:
	go test ./...
