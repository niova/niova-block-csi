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
XFS_SC="niova-xfs-fs-storageclass.yaml"

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
for f in "$BLOCK_SC" "$EXT4_SC" "$XFS_SC"; do
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

run_integrity_test() {
    local num=$1 title=$2 focus=$3
    echo
    echo "==================== integrity test $num: $title ===================="
    
    cd "$REPO_DIR"

    # Temporarily disable -e to capture failure without exiting script
    set +e
    env \
        NIOVA_BLOCK_UBLK_UNIFIED=0 \
        NIOVA_LOG_LEVEL=0 \
        NIOVA_GOSSIP_PATH=/etc/niova/gossipNodes \
        NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT=256 \
        NIOVA_GOSSIP_KEY=dummy \
        NIOVA_BLOCK_CP_AUTH_USERNAME=admin \
        NIOVA_BLOCK_CP_AUTH_SECRET=admin \
        LD_LIBRARY_PATH="/usr/local/lib:${LD_LIBRARY_PATH:-}" \
        go run github.com/onsi/ginkgo/v2/ginkgo -v \
            --timeout="$TEST_TIMEOUT" \
            --focus="$focus" \
            ./test/e2e/integrity
    
    local status=$?
    set -e

    (( status == 0 )) && { echo "---- integrity test $num PASSED: $title"; PASSED+=("$num: $title"); return 0; }
    echo "---- integrity test $num FAILED: $title" >&2
    FAILED+=("$num: $title")
}

switch_storageclass() {
    local sc_file=$1
    echo
    echo "== switching StorageClass to $sc_file =="
    (
        cd "$DEPLOY_DIR"
        # Delete existing StorageClasses to bypass immutability restrictions
        kubectl delete -f "$BLOCK_SC" --ignore-not-found
        kubectl delete -f "$EXT4_SC"  --ignore-not-found
        kubectl delete -f "$XFS_SC"   --ignore-not-found
        kubectl apply  -f "$sc_file"
    )
    kubectl get storageclass
}


echo "repo: $REPO_DIR"
echo "KUBECONFIG: ${KUBECONFIG:-<default>}"

# 1) deploy dir: apply the block StorageClass
cd "$DEPLOY_DIR"
echo
echo "== applying $BLOCK_SC =="
switch_storageclass $BLOCK_SC
# 2) test dir: run the tests one by one
cd "$TEST_DIR"

run_test 1 "Invalid multi-attach (RWO)" "Invalid multi-attach"
run_test 2 "Rapid create/delete"        "Rapid create/delete"
run_test 3 "Lifecycle: Block volume mode" "PVC Lifecycle.*Block volume mode"

run_integrity_test 4 "fio checksum verification (Block PVC)" "writes with sha512 checksums and reads back without errors \(block PVC\)"
run_integrity_test 5 "Backend restart during IO"             "Backend restart during IO
"
run_integrity_test 6 "Node: Device naming stability (Block)" "Node-Level.*Device naming stability"
run_integrity_test 7 "Security: Raw block device permissions" "Security.*Raw block device permissions"
run_integrity_test 8 "Security: Device Isolation" "Security.*pod cannot access a different pod's block device path"
run_integrity_test 9 "Security: udev rule file check" "Security.*udev rule file is installed"

echo
echo "== switching StorageClass for test 3 =="
switch_storageclass $EXT4_SC
cd "$TEST_DIR"
run_test 10 "Rapid pod scheduling" "Rapid pod scheduling"
run_test 11 "Filesystem ext4 validation" "Filesystem.*ext4 validation"
run_test 12 "Lifecycle: Filesystem volume mode (ext4)" "PVC Lifecycle.*Filesystem volume mode"
run_integrity_test 13 "fio checksum verification (Filesystem PVC)" "writes with sha512 checksums and reads back without errors \(filesystem PVC\)"
run_integrity_test 14 "Node: CSI plugin restart handling (Filesystem)" "Node-Level.*CSI plugin restart handling"

echo
echo "== switching StorageClass for test 3 =="
switch_storageclass $XFS_SC
cd "$TEST_DIR"
run_test 15 "Filesystem xfs validation" "Filesystem.*xfs validation"
run_test 16 "Lifecycle: Filesystem volume mode (xfs)" "PVC Lifecycle.*Filesystem volume mode"
run_test 17 "Lifecycle: Access mode validation" "PVC Lifecycle.*Access mode validation"
run_test 18 "Lifecycle: Idempotency" "PVC Lifecycle.*Idempotency"

###############################################################################
# Summary
###############################################################################

echo
echo "==================== summary ===================="
for t in "${PASSED[@]}"; do echo "  PASSED  $t"; done
for t in "${FAILED[@]}"; do echo "  FAILED  $t"; done

if (( ${#FAILED[@]} > 0 )); then
    echo "${#FAILED[@]} of 18 tests failed" >&2
    exit 1
fi
echo "all 18 tests passed"
