#!/usr/bin/env bash
#
# unit_test.sh - run the "Multi-Attach & Concurrency" tests of niova-block-csi.
#
# Usage:
#   ./unit_test.sh /path/to/niova-block-csi
#
# What it does:
#   1. cd <repo>/deploy and apply niova-block-storageclass.yaml
#   2. cd <repo>/test and run the tests one by one:
#        1) Invalid multi-attach (RWO)
#        2) Rapid create/delete
#        3) Rapid pod scheduling  (first: delete niova-block-storageclass.yaml
#                                  and apply niova-ext4-fs-storageclass.yaml)
#   3. print a summary; exit 1 if any test failed
#
# Optional environment variables:
#   KUBECONFIG      kubeconfig to use (default: /etc/rancher/k3s/k3s.yaml if it exists)
#   TEST_PKG        go package to test inside <repo>/test   (default: .)
#   TEST_TIMEOUT    go test timeout per test                (default: 45m)

set -Eeuo pipefail

if [[ $# -ne 1 ]]; then
    echo "Usage: $0 <path-to-niova-block-csi-repo>" >&2
    exit 2
fi

REPO_DIR="$(cd "$1" 2>/dev/null && pwd)" || { echo "ERROR: repo directory not found: $1" >&2; exit 1; }
DEPLOY_DIR="$REPO_DIR/deploy"
TEST_DIR="$REPO_DIR/test"
TEST_PKG="${TEST_PKG:-.}"
TEST_TIMEOUT="${TEST_TIMEOUT:-45m}"

BLOCK_SC="niova-block-storageclass.yaml"
EXT4_SC="niova-ext4-fs-storageclass.yaml"

# kubectl and the Go test framework both read KUBECONFIG
if [[ -z "${KUBECONFIG:-}" && -r /etc/rancher/k3s/k3s.yaml ]]; then
    export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
fi

###############################################################################
# Checks
###############################################################################

for cmd in kubectl go; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "ERROR: $cmd is not installed or not in PATH" >&2; exit 1; }
done
for d in "$DEPLOY_DIR" "$TEST_DIR"; do
    [[ -d "$d" ]] || { echo "ERROR: directory not found: $d" >&2; exit 1; }
done
for f in "$BLOCK_SC" "$EXT4_SC"; do
    [[ -f "$DEPLOY_DIR/$f" ]] || { echo "ERROR: $DEPLOY_DIR/$f not found" >&2; exit 1; }
done

###############################################################################
# Helpers
###############################################################################

PASSED=()
FAILED=()

# run_test <number> <title> <ginkgo focus regex>
run_test() {
    local num=$1 title=$2 focus=$3
    echo
    echo "==================== test $num: $title ===================="
    if go test "$TEST_PKG" -v -count=1 -timeout "$TEST_TIMEOUT" \
            -ginkgo.v -ginkgo.focus="$focus"; then
        echo "---- test $num PASSED: $title"
        PASSED+=("$num: $title")
    else
        echo "---- test $num FAILED: $title" >&2
        FAILED+=("$num: $title")
    fi
}

###############################################################################
# test1 concurrency test
###############################################################################

echo "test1 concurrency test"
echo "repo: $REPO_DIR"
echo "KUBECONFIG: ${KUBECONFIG:-<default>}"

# 1) deploy dir: apply the block StorageClass
cd "$DEPLOY_DIR"
echo
echo "== applying $BLOCK_SC =="
kubectl apply -f "$BLOCK_SC"

# 2) test dir: run the tests one by one
cd "$TEST_DIR"

run_test 1 "Invalid multi-attach (RWO)" "Invalid multi-attach"
run_test 2 "Rapid create/delete"        "Rapid create/delete"

# test 3 uses a filesystem PVC: switch from the block StorageClass to the ext4 one
echo
echo "== switching StorageClass for test 3 =="
(
    cd "$DEPLOY_DIR"
    kubectl delete -f "$BLOCK_SC" --ignore-not-found
    kubectl apply  -f "$EXT4_SC"
)
kubectl get storageclass

run_test 3 "Rapid pod scheduling" "Rapid pod scheduling"

###############################################################################
# Summary
###############################################################################

echo
echo "==================== summary ===================="
for t in "${PASSED[@]}"; do echo "  PASSED  $t"; done
for t in "${FAILED[@]}"; do echo "  FAILED  $t"; done

if (( ${#FAILED[@]} > 0 )); then
    echo "${#FAILED[@]} of 3 tests failed" >&2
    exit 1
fi
echo "all 3 tests passed"
