PKG := github.com/tadapin/pg-dumpling
BUILD_TS := $(shell date -u '+%Y-%m-%d %H:%M:%S')
GIT_HASH := $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
GIT_BRANCH := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")
GO_VERSION := $(shell go version | awk '{print $$3}')

LDFLAGS := -X "$(PKG)/cli.ReleaseVersion=dev" \
           -X "$(PKG)/cli.BuildTimestamp=$(BUILD_TS)" \
           -X "$(PKG)/cli.GitHash=$(GIT_HASH)" \
           -X "$(PKG)/cli.GitBranch=$(GIT_BRANCH)" \
           -X "$(PKG)/cli.GoVersion=$(GO_VERSION)"

GOBUILD := go build -ldflags '$(LDFLAGS)'
GOTEST := go test -v -timeout 5m

.PHONY: all build test test-unit clean tidy vet help

all: build

build:
	$(GOBUILD) -o bin/dumpling ./cmd/dumpling

test: test-unit

test-unit:
	$(GOTEST) -short -count=1 ./log/... ./export/...

# Tests that exercise failpoints (e.g. TestWriteTableMeta) require source
# rewriting via failpoint-ctl. Install with:
#   go install github.com/pingcap/failpoint/failpoint-ctl@latest
test-unit-failpoint:
	failpoint-ctl enable
	-$(GOTEST) -short -count=1 ./log/... ./export/...
	failpoint-ctl disable

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/

help:
	@echo "Targets:"
	@echo "  build      build bin/dumpling"
	@echo "  test       alias of test-unit"
	@echo "  test-unit  short unit tests for log/ and export/"
	@echo "  vet        go vet ./..."
	@echo "  tidy       go mod tidy"
	@echo "  clean      remove bin/"
