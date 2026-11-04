# Makefile for niova-block-csi driver

# Variables
DRIVER_NAME = niova-block-csi
VERSION = v1.0.0
REGISTRY ?= docker.io/niova
IMAGE_TAG ?= $(VERSION)

# Architecture Detection
# Go uses 'arm64' for the architecture that uname calls 'aarch64'
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_M),aarch64)
    ARCH = arm64
else ifeq ($(UNAME_M),x86_64)
    ARCH = amd64
else
    ARCH = $(shell go env GOARCH)
endif

# Go related variables
GOOS ?= linux
GOARCH ?= $(ARCH)
GOPATH ?= $(shell go env GOPATH)

# Explicitly set the C compiler to ensure CGO doesn't default to x86 toolchains
CC = gcc

# Binary names
CONTROLLER_BINARY = niova-block-csi-controller
NODE_BINARY = niova-block-csi-node

# Build directories
BUILD_DIR ?= /opt/niova
BIN_DIR = $(BUILD_DIR)

# Test configuration
E2E_KUBECONFIG      ?= $(HOME)/.kube/config
E2E_STORAGE_CLASS   ?= niova-csi-sc
E2E_NAMESPACE       ?= niova-csi-test
E2E_FIO_IMAGE       ?= ljishen/fio:latest
E2E_NODE_NAME       ?=
E2E_TIMEOUT         ?= 30m
PERF_THRESHOLDS_FILE ?=
PERF_RESULTS_FILE   ?=

.PHONY: all build controller node clean help version \
        test test-unit test-e2e test-lifecycle test-node test-integrity \
        test-filesystem test-concurrency test-security test-performance \
        test-e2e-all storageclass

# Default target
all: build

# Build both controller and node binaries
build: controller node

# Build controller binary
controller:
	@echo "Building controller binary for $(GOOS)/$(GOARCH)..."
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 \
	GOOS=$(GOOS) \
	GOARCH=$(GOARCH) \
	CC=$(CC) \
	go build -v -o $(BIN_DIR)/$(CONTROLLER_BINARY) ./cmd/controller

# Build node binary
node:
	@echo "Building node binary for $(GOOS)/$(GOARCH)..."
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=1 \
	GOOS=$(GOOS) \
	GOARCH=$(GOARCH) \
	CC=$(CC) \
	go build -v -o $(BIN_DIR)/$(NODE_BINARY) ./cmd/node

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(BUILD_DIR)

# Show help
help:
	@echo "Available targets:"
	@echo "  build              - Build both binaries"
	@echo "  controller         - Build controller binary"
	@echo "  node               - Build node binary"
	@echo "  clean              - Remove build artifacts"
	@echo "  version            - Show detected arch and version"
	@echo ""
	@echo "Test targets (require E2E_KUBECONFIG and a running cluster for e2e):"
	@echo "  test               - Run unit tests only"
	@echo "  test-unit          - Run unit tests only"
	@echo "  test-e2e-all       - Run all e2e suites"
	@echo "  test-lifecycle     - PVC lifecycle tests"
	@echo "  test-node          - Node-level / udev symlink tests"
	@echo "  test-integrity     - Data integrity (fio verify) tests"
	@echo "  test-filesystem    - Filesystem (ext4/xfs) tests"
	@echo "  test-concurrency   - Multi-attach / concurrency tests"
	@echo "  test-security      - Device permissions / isolation tests"
	@echo "  test-performance   - fio benchmark tests"
	@echo ""
	@echo "Key variables:"
	@echo "  E2E_KUBECONFIG     (default: ~/.kube/config)"
	@echo "  E2E_STORAGE_CLASS  (default: niova-csi-sc)"
	@echo "  E2E_NAMESPACE      (default: niova-csi-test)"
	@echo "  E2E_NODE_NAME      (default: empty = any node)"
	@echo "  E2E_FIO_IMAGE      (default: ljishen/fio:latest)"
	@echo "  E2E_TIMEOUT        (default: 30m)"

# Apply StorageClass to the cluster
storageclass:
	kubectl apply -f test/manifests/storageclass.yaml

# Run unit tests (no cluster needed)
test-unit:
	go test -v -count=1 ./pkg/...

# Run all e2e test suites (requires cluster + self-hosted runner env)
test-e2e-all: storageclass
	E2E_KUBECONFIG=$(E2E_KUBECONFIG) \
	E2E_STORAGE_CLASS=$(E2E_STORAGE_CLASS) \
	E2E_NAMESPACE=$(E2E_NAMESPACE) \
	E2E_FIO_IMAGE=$(E2E_FIO_IMAGE) \
	E2E_NODE_NAME=$(E2E_NODE_NAME) \
	go test -v -count=1 -timeout $(E2E_TIMEOUT) \
		./test/e2e/lifecycle/... \
		./test/e2e/node/... \
		./test/e2e/integrity/... \
		./test/e2e/filesystem/... \
		./test/e2e/concurrency/... \
		./test/e2e/security/...

# Individual suite targets
test-lifecycle: storageclass
	E2E_KUBECONFIG=$(E2E_KUBECONFIG) E2E_STORAGE_CLASS=$(E2E_STORAGE_CLASS) \
	E2E_NAMESPACE=$(E2E_NAMESPACE)-lifecycle E2E_FIO_IMAGE=$(E2E_FIO_IMAGE) \
	E2E_NODE_NAME=$(E2E_NODE_NAME) \
	go test -v -count=1 -timeout $(E2E_TIMEOUT) ./test/e2e/lifecycle/...

test-node: storageclass
	E2E_KUBECONFIG=$(E2E_KUBECONFIG) E2E_STORAGE_CLASS=$(E2E_STORAGE_CLASS) \
	E2E_NAMESPACE=$(E2E_NAMESPACE)-node E2E_FIO_IMAGE=$(E2E_FIO_IMAGE) \
	E2E_NODE_NAME=$(E2E_NODE_NAME) \
	go test -v -count=1 -timeout $(E2E_TIMEOUT) ./test/e2e/node/...

test-integrity: storageclass
	E2E_KUBECONFIG=$(E2E_KUBECONFIG) E2E_STORAGE_CLASS=$(E2E_STORAGE_CLASS) \
	E2E_NAMESPACE=$(E2E_NAMESPACE)-integrity E2E_FIO_IMAGE=$(E2E_FIO_IMAGE) \
	E2E_NODE_NAME=$(E2E_NODE_NAME) \
	go test -v -count=1 -timeout $(E2E_TIMEOUT) ./test/e2e/integrity/...

test-filesystem: storageclass
	E2E_KUBECONFIG=$(E2E_KUBECONFIG) E2E_STORAGE_CLASS=$(E2E_STORAGE_CLASS) \
	E2E_NAMESPACE=$(E2E_NAMESPACE)-filesystem E2E_FIO_IMAGE=$(E2E_FIO_IMAGE) \
	E2E_NODE_NAME=$(E2E_NODE_NAME) \
	go test -v -count=1 -timeout $(E2E_TIMEOUT) ./test/e2e/filesystem/...

test-concurrency: storageclass
	E2E_KUBECONFIG=$(E2E_KUBECONFIG) E2E_STORAGE_CLASS=$(E2E_STORAGE_CLASS) \
	E2E_NAMESPACE=$(E2E_NAMESPACE)-concurrency E2E_FIO_IMAGE=$(E2E_FIO_IMAGE) \
	E2E_NODE_NAME=$(E2E_NODE_NAME) \
	go test -v -count=1 -timeout $(E2E_TIMEOUT) ./test/e2e/concurrency/...

test-security: storageclass
	E2E_KUBECONFIG=$(E2E_KUBECONFIG) E2E_STORAGE_CLASS=$(E2E_STORAGE_CLASS) \
	E2E_NAMESPACE=$(E2E_NAMESPACE)-security E2E_FIO_IMAGE=$(E2E_FIO_IMAGE) \
	E2E_NODE_NAME=$(E2E_NODE_NAME) \
	go test -v -count=1 -timeout $(E2E_TIMEOUT) ./test/e2e/security/...

test-performance: storageclass
	E2E_KUBECONFIG=$(E2E_KUBECONFIG) E2E_STORAGE_CLASS=$(E2E_STORAGE_CLASS) \
	E2E_NAMESPACE=$(E2E_NAMESPACE)-perf E2E_FIO_IMAGE=$(E2E_FIO_IMAGE) \
	E2E_NODE_NAME=$(E2E_NODE_NAME) \
	PERF_THRESHOLDS_FILE=$(PERF_THRESHOLDS_FILE) PERF_RESULTS_FILE=$(PERF_RESULTS_FILE) \
	go test -v -count=1 -timeout 120m ./test/performance/...

test: test-unit

# Version information
version:
	@echo "Driver:     $(DRIVER_NAME)"
	@echo "Version:    $(VERSION)"
	@echo "Host Arch:  $(UNAME_M)"
	@echo "Go Arch:    $(GOARCH)"
	@echo "Compiler:   $(CC)"
