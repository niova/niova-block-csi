#!/usr/bin/env bash
#
# unit_test.sh - run the "Multi-Attach & Concurrency" tests of niova-block-csi.
#
# Usage:
#   ./unit_test.sh /path/to/niova-block-csi
#
# What it does:
#   1. cd <repo>/deploy and apply niova-block-storageclass.yaml
#   2. cd <repo>/test and run the tests target subpackage subdirectories
#   3. print a summary; exit 1 if any test failed
#
# Optional environment variables:
#   KUBECONFIG     kubeconfig to use (default: /etc/rancher/k3s/k3s.yaml if it exists)
#   TEST_TIMEOUT   go test timeout per test                (default: 45m)

set -Eeuo pipefail

# Preserve Go path if executing via sudo/restricted shells
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin

if [[ $# -ne 1 ]]; then
    echo "Usage: $0 <path-to-niova-block-csi-repo>" >&2
    exit 2
fi

REPO_DIR="$(cd "$1" 2>/dev/null && pwd)" || { echo "ERROR: repo directory not found: $1" >&2; exit 1; }
DEPLOY_DIR="$REPO_DIR/deploy"
TEST_DIR="$REPO_DIR/test"
TEST_TIMEOUT="${TEST_TIMEOUT:-45m}"

BLOCK_SC="niova-block-storageclass.yaml"
EXT4_SC="niova-ext4-fs-storageclass.yaml"
XFS_SC="niova-xfs-fs-storageclass.yaml"

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
for f in "$BLOCK_SC" "$EXT4_SC" "$XFS_SC" ; do
    [[ -f "$DEPLOY_DIR/$f" ]] || { echo "ERROR: $DEPLOY_DIR/$f not found" >&2; exit 1; }
done

###############################################################################
# Helpers
###############################################################################

PASSED=()
FAILED=()

# run_test <number> <title> <subpackage_dir> <ginkgo focus regex>
run_test() {
    local num=$1 title=$2 subpkg=$3 focus=$4
    echo
    echo "==================== test $num: $title ===================="

    cd "$TEST_DIR"
    if go test "./e2e/$subpkg" -v -count=1 -timeout "$TEST_TIMEOUT" \
            -ginkgo.v --ginkgo.focus="$focus"; then
        echo "---- test $num PASSED: $title"
        PASSED+=("$num: $title")
    else
        echo "---- test $num FAILED: $title" >&2
        FAILED+=("$num: $title")
    fi
}

run_integrity_test() {
    local num=$1 title=$2 subpkg=$3 focus=$4
    echo
    echo "==================== integrity test $num: $title ===================="

    cd "$REPO_DIR"

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
            "./test/e2e/$subpkg"

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
switch_storageclass "$BLOCK_SC"

# 2) Run tests with direct subpackage paths
run_test 1 "Invalid multi-attach (RWO)"           "concurrency" "Invalid multi-attach"
run_test 2 "Rapid create/delete"                   "concurrency" "Rapid create/delete"
run_test 3 "Lifecycle:Block volume mode"          "lifecycle"   "Block volume mode"
run_test 4 "Lifecycle: Access mode validation"     "lifecycle"   "Access mode validation"

run_integrity_test 5 "fio checksum verification" "integrity" "writes with sha512 checksums and reads back without errors \(block PVC\)"
run_integrity_test 6 "Backend restart during IO"          "integrity"   "Backend restart"
run_integrity_test 7 "Node: Device naming stability"      "node"  "Device naming stability"
run_integrity_test 8 "Security: Raw block device permissions" "security" "Raw block device permissions"

echo
echo "== switching StorageClass for ext4 tests =="
switch_storageclass "$EXT4_SC"

run_test 9 "Rapid pod scheduling"                        "concurrency" "Rapid pod scheduling"
run_test 10 "Filesystem ext4 validation"                  "filesystem"  "ext4 validation"
run_test 11 "Lifecycle: Filesystem volume mode"     "lifecycle"   "creates, binds, mounts and deletes a ext4 filesystem PVC"
run_integrity_test 12 "fio checksum verification" "integrity" "writes with sha512 checksums and reads back without errors \(filesystem PVC\)"
run_integrity_test 13 "Node: CSI plugin restart handling"  "node"  "CSI plugin restart handling"

echo
echo "== switching StorageClass for xfs tests =="
switch_storageclass "$XFS_SC"

run_test 14 "Filesystem xfs validation"                   "filesystem"  "xfs validation"
run_test 15 "Lifecycle: Filesystem volume mode"      "lifecycle"   "creates, binds, mounts and deletes a xfs filesystem PVC"
run_test 16 "Lifecycle: Idempotency"                     "lifecycle"   "Idempotency"
###############################################################################
# Summary
###############################################################################

echo
echo "==================== summary ===================="
for t in "${PASSED[@]}"; do echo "  PASSED  $t"; done
for t in "${FAILED[@]}"; do echo "  FAILED  $t"; done

if (( ${#FAILED[@]} > 0 )); then
    echo "${#FAILED[@]} of 16 tests failed" >&2
    exit 1
fi
echo "all 16 tests passed"
