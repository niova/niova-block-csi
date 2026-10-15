package node

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/niova-block-csi/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

var (
	QUEUEDEPTH    = "128"
	MAXBUFSIZE    = "1048576"
	ldLibraryPath = "/usr/local/lib"
	workingDir    = "/var/niova"
)


type UblkManager struct {
	ublkBinary string
	ClientID   string
	ldLib      string
}

func NewUblkManager(clientid string) *UblkManager {
	return &UblkManager{
		ublkBinary: "niova-ublk", // Assuming niova-ublk is in PATH
		ClientID:   clientid,
		ldLib:      ldLibraryPath,
	}
}

func (um *UblkManager) getBinaryPath() {
	if path := os.Getenv("NIOVA_BIN_PATH"); path != "" {
		um.ublkBinary = fmt.Sprintf("%s/bin/niova-ublk", path)
		um.ldLib = fmt.Sprintf("%s/lib", path)
	}
}

// CreateUblkDevice starts niova-ublk for the given volume as a transient
// systemd unit on the host (see systemd.go) rather than as a child of this
// container, so it survives node-plugin pod restarts. Returns
// (ublkDevicePath, pid, error) where ublkDevicePath is the stable
// /dev/disk/by-uuid/<volumeID> symlink created by 61-niova-ublk.rules, and
// pid is the host-side niova-ublk process, kept only for observability
// (stopping it again goes through stopUblkUnit(volumeID), not the pid).
func (um *UblkManager) CreateUblkDevice(volumeID, volumesize string, recovery, readOnly bool) (string, int, error) {
	klog.Infof("Creating ublk device for volume %s", volumeID)
	// get niova binary path
	um.getBinaryPath()
	args := []string{
		"-t", "cp",
		"-v", volumeID,
		"-u", um.ClientID,
		"-q", QUEUEDEPTH,
		"-b", MAXBUFSIZE,
		"-T",
	}
	if recovery {
		args = append(args, "-r")
	}
	if readOnly {
		args = append(args, "-R")
	}
	env := []string{
		fmt.Sprintf("LD_LIBRARY_PATH=%s", um.ldLib),
		fmt.Sprintf("NIOVA_GOSSIP_PATH=%s", os.Getenv(types.NiovaGossipPath)),
		fmt.Sprintf("NIOVA_GOSSIP_KEY=%s", os.Getenv(types.NiovaGossipKey)),
		fmt.Sprintf("NIOVA_BLOCK_CP_AUTH_USERNAME=%s", os.Getenv(types.NiovaUserName)),
		fmt.Sprintf("NIOVA_BLOCK_CP_AUTH_SECRET=%s", os.Getenv(types.NiovaUserSecret)),
		fmt.Sprintf("NIOVA_BLOCK_UBLK_UNIFIED=%s", os.Getenv(types.NiovaUblkUnified)),
		fmt.Sprintf("NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT=%s", os.Getenv(types.NiovaMdsvcChunkLimit)),
	}

	klog.Infof("ENV variables %s: %s and %s: %s, %s: %s and %s: %s, %s: %s, %s: %s ", types.NiovaGossipPath, os.Getenv(types.NiovaGossipPath), types.NiovaGossipKey, os.Getenv(types.NiovaGossipKey), types.NiovaUserName, os.Getenv(types.NiovaUserName), types.NiovaUserSecret, os.Getenv(types.NiovaUserSecret), types.NiovaUblkUnified, os.Getenv(types.NiovaUblkUnified), types.NiovaMdsvcChunkLimit, os.Getenv(types.NiovaMdsvcChunkLimit))

	pid, err := startUblkUnit(volumeID, um.ublkBinary, args, env, workingDir)
	if err != nil {
		return "", -1, status.Errorf(codes.Internal, "failed to start ublk: %v", err)
	}

	ublkDevicePath, err := waitForByUUIDLink(volumeID)
	if err != nil {
		if stopErr := stopUblkUnit(volumeID); stopErr != nil {
			klog.Warningf("failed to stop ublk unit %s after device wait failure: %v", ublkUnitName(volumeID), stopErr)
		}
		return "", -1, err
	}

	klog.Infof("Successfully created ublk device %s for volume %s", ublkDevicePath, volumeID)
	return ublkDevicePath, pid, nil
}

// DeleteUblkDevice stops the host-side systemd unit for volumeID, if still
// running, and detaches the device from the ublk kernel driver.
func (um *UblkManager) DeleteUblkDevice(volumeID, ublkDevicePath string) error {
	klog.Infof("Deleting ublk device %s for volume %s", ublkDevicePath, volumeID)

	// Resolve the by-uuid symlink to its ublkbN id before touching the
	// unit: once niova-ublk exits (next step), a udev "remove" event can
	// take the symlink down with it, so resolving afterwards would race.
	// `ublk del` itself (UBLK_CMD_STOP_DEV + UBLK_CMD_DEL_DEV) doesn't
	// care whether the daemon is still alive, already exited, or sitting
	// quiesced mid-recovery, so it's safe to run after stopUblkUnit.
	var id string
	if ublkDevicePath != "" {
		var err error
		id, err = resolveUblkID(ublkDevicePath)
		if err != nil {
			return fmt.Errorf("failed to resolve ublk id for %s: %v", ublkDevicePath, err)
		}
	}

	if err := stopUblkUnit(volumeID); err != nil {
		return fmt.Errorf("failed to delete ublk device: %v", err)
	}

	if id != "" {
		klog.Infof("Deleting the ublk %s", ublkDevicePath)
		// Run on the host via systemd, not exec.Command in this
		// container: the ublk CLI's runtime (libublksrv/liburing) isn't
		// reliably available/resolvable inside the node-plugin image,
		// while the host environment niova-ublk itself runs in is.
		if err := runHostCommand(volumeID, "ublk", []string{"del", "-n", id}); err != nil {
			return fmt.Errorf("failed to delete ublk %s: %v", ublkDevicePath, err)
		}
	}

	klog.Infof("Successfully deleted ublk device %s for volume %s", ublkDevicePath, volumeID)
	return nil
}

func prepareTargetPath(nisdUUID, nisdIPAddr string, nisdPort int) string {
	// nisduuid := tcp:<nisd_uuid>:<nisd_ip>:<nisd_port>
	var tPath string
	if nisdIPAddr != "" {
		tPath = fmt.Sprintf("tcp:%s:%s:%d", nisdUUID, nisdIPAddr, nisdPort+1)
	} else {
		tPath = fmt.Sprintf("tcp:%s:127.0.0.1:%d", nisdUUID, nisdPort+1)
	}
	return tPath
}

func (um *UblkManager) extractUblkID(ublkDevicePath string) string {
	// Extract ublk ID from device path like /dev/ublk123 -> 123
	base := filepath.Base(ublkDevicePath)
	if strings.HasPrefix(base, "ublk") {
		return strings.TrimPrefix(base, "ublkb")
	}
	return ""
}

// waitForByUUIDLink polls for the /dev/disk/by-uuid/<volumeID> symlink that
// 61-niova-ublk.rules creates in response to niova-ublk's synthetic uevent.
// The symlink appearing is an implicit readiness signal: niova-ublk only fires
// the uevent after niova_ublk_start() completes successfully.
func waitForByUUIDLink(volumeID string) (string, error) {
	path := types.UblkByUUIDPath(volumeID)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); err == nil {
			return path, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", status.Errorf(codes.DeadlineExceeded,
		"timed out waiting for udev symlink %s (is 61-niova-ublk.rules installed on the node?)", path)
}

func (um *UblkManager) GetUblkDeviceInfo(ublkDevicePath string) (map[string]string, error) {
	ublkID := um.extractUblkID(ublkDevicePath)
	if ublkID == "" {
		return nil, fmt.Errorf("invalid ublk device path: %s", ublkDevicePath)
	}

	// Command to get ublk device info
	// Format: niova-ublk -i <ublk_id> --info
	cmd := exec.Command(um.ublkBinary,
		"-i", ublkID,
		"--info")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to get ublk device info: %v, output: %s", err, string(output))
	}

	// Parse output into key-value pairs
	info := make(map[string]string)
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				info[key] = value
			}
		}
	}

	return info, nil
}

// resolveUblkID follows the /dev/disk/by-uuid/<volumeID> symlink down to the
// real ublkbN device node and returns its numeric minor, as required by
// `ublk del -n <id>`. filepath.Base of the symlink itself is the volume
// UUID, not an ublkbN name, so it must be resolved first.
func resolveUblkID(ublkDevicePath string) (string, error) {
	resolved, err := filepath.EvalSymlinks(ublkDevicePath)
	if err != nil {
		return "", err
	}
	base := filepath.Base(resolved)
	if !strings.HasPrefix(base, "ublkb") {
		return "", fmt.Errorf("unexpected ublk device name %q resolved from %q", base, ublkDevicePath)
	}
	return strings.TrimPrefix(base, "ublkb"), nil
}
