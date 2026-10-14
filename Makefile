CURRENT_DIR=$(shell pwd)
HOST_OS?=$(shell go env GOOS)
HOST_ARCH?=$(shell go env GOARCH)
DIST_DIR=${CURRENT_DIR}/dist

LOCALBIN ?= ${CURRENT_DIR}/bin
GOLANGCI_LINT_VERSION ?= v2.1.6
HELMDOCS_VERSION ?= v1.14.2
GITCHGLOG_VERSION ?= v0.15.4
BUF_VERSION ?= v1.47.2
PROTOC_GEN_GO_VERSION ?= v1.36.6
PROTOC_GEN_GO_GRPC_VERSION ?= v1.5.1

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: build
build: ## build the daemon binary
	CGO_ENABLED=0 GOOS=${HOST_OS} GOARCH=${HOST_ARCH} go build -v -ldflags="-s -w" -o ${DIST_DIR}/krci-env-daemon-${HOST_ARCH} ./cmd/krci-env-daemon

.PHONY: generate
generate: buf protoc-gen-go protoc-gen-go-grpc ## regenerate internal/externalscaler from externalscaler.proto
	PATH=${LOCALBIN}:$$PATH ${LOCALBIN}/buf generate --template internal/externalscaler/buf.gen.yaml internal/externalscaler

.PHONY: buf
buf: $(LOCALBIN)
	@test -x $(LOCALBIN)/buf || \
		GOBIN=$(LOCALBIN) go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

.PHONY: protoc-gen-go
protoc-gen-go: $(LOCALBIN)
	@test -x $(LOCALBIN)/protoc-gen-go || \
		GOBIN=$(LOCALBIN) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

.PHONY: protoc-gen-go-grpc
protoc-gen-go-grpc: $(LOCALBIN)
	@test -x $(LOCALBIN)/protoc-gen-go-grpc || \
		GOBIN=$(LOCALBIN) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

.PHONY: test
test: lint ## run lint, then the tests (the chart render tests skip when helm is absent)
	go test ./... -coverprofile=coverage.out

.PHONY: lint
lint: golangci-lint ## run go lint
	${LOCALBIN}/golangci-lint run -v -c .golangci.yaml ./...

.PHONY: lint-fix
lint-fix: golangci-lint ## run go lint with --fix
	${LOCALBIN}/golangci-lint run -v -c .golangci.yaml ./... --fix

# use https://github.com/git-chglog/git-chglog/
.PHONY: changelog
changelog: git-chglog ## generate CHANGELOG.md (pass NEXT_RELEASE_TAG=X.Y.Z at release time)
ifneq (${NEXT_RELEASE_TAG},)
	${LOCALBIN}/git-chglog --next-tag v${NEXT_RELEASE_TAG} -o CHANGELOG.md
else
	${LOCALBIN}/git-chglog -o CHANGELOG.md
endif

.PHONY: git-chglog
git-chglog: $(LOCALBIN)
	@test -x $(LOCALBIN)/git-chglog || \
		GOBIN=$(LOCALBIN) go install github.com/git-chglog/git-chglog/cmd/git-chglog@$(GITCHGLOG_VERSION)

.PHONY: helm-lint
helm-lint: ## lint the Helm chart
	helm lint deploy-templates

.PHONY: helm-template
helm-template: ## render the Helm chart with defaults
	helm template krci-env-daemon deploy-templates --namespace krci-env-daemon

.PHONY: helm-docs
helm-docs: helmdocs ## generate deploy-templates/README.md from values
	${LOCALBIN}/helm-docs

.PHONY: helmdocs
helmdocs: $(LOCALBIN)
	@test -x $(LOCALBIN)/helm-docs || \
		GOBIN=$(LOCALBIN) go install github.com/norwoodj/helm-docs/cmd/helm-docs@$(HELMDOCS_VERSION)

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN)
	@test -x $(LOCALBIN)/golangci-lint || \
		GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: help
help: ## show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'
