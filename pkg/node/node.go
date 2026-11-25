package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/google/uuid"
	"github.com/niova-block-csi/pkg/config"
	"github.com/niova-block-csi/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

type NodeServer struct {
	nodeID       string
	Node         *types.Node
	ublkManager  *UblkManager
	mountManager *MountManager
	mutex        sync.RWMutex
	caps         []*csi.NodeServiceCapability
	config       *config.ConfigManager
}

func NewNodeServer(nodeID string, clientid string, configManager *config.ConfigManager) *NodeServer {
	return &NodeServer{
		nodeID: nodeID,
		Node: &types.Node{
			VolMap: make(map[string]*types.NodeVolume),
		},
		ublkManager:  NewUblkManager(clientid),
		mountManager: NewMountManager(),
		caps: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
		},
		config: configManager,
	}
}

func (ns *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	klog.Infof("NodeStageVolume: called with args %+v", req)

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}

	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Staging target path cannot be empty")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities cannot be empty")
	}
	readOnly := false
	accessMode := req.VolumeCapability.AccessMode.Mode
	if accessMode == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		klog.Infof("Volume is ReadOnlyMany")
		readOnly = true
	}
	recoveryModeStr := req.GetVolumeContext()[types.UblkRecovery]
	recoveryMode, err := strconv.ParseBool(recoveryModeStr)
	if err != nil {
    		recoveryMode = false // default value
	}
	cap := req.GetVolumeCapability()
	isBlock := cap.GetBlock() != nil
	mode := types.MOUNT_MODE
	if isBlock {
		mode = types.BLOCK_MODE
	}

	volumeID := req.GetVolumeId()
	stagingPath := req.GetStagingTargetPath()
	publishContext := req.GetPublishContext()
	volumeSizeStr := publishContext["volumeSize"]
	_, err = strconv.ParseInt(volumeSizeStr, 10, 64)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Invalid volume size: %v", err))
	}

	// If this volume is already staged with a live ublk device, return
	// success. Guards against ConfigManager.UpdateNodeVolumeMap having
	// failed to confirm a live backend for this volume ID (e.g. the udev
	// symlink check raced with niova-ublk still coming up) and left a bare
	// VolID-only entry behind — that's not actually staged locally, so it
	// must not short-circuit here.
	// Only a read lock is needed here since we are not modifying the map.
	ns.mutex.RLock()
	existing, alreadyStaged := ns.Node.VolMap[volumeID]
	alreadyStaged = alreadyStaged && existing.UblkPath != ""
	ns.mutex.RUnlock()
	if alreadyStaged {
		klog.Infof("NodeStageVolume: volume %s is already staged, returning success", volumeID)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	ublkDevicePath, ublkpid, err := ns.ublkManager.CreateUblkDevice(volumeID, volumeSizeStr, recoveryMode, readOnly)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to create ublk device: %v", err))
	}

	if mode == types.MOUNT_MODE {
		// Determine filesystem type from volume capability
		fsType := "ext4" // default
		if req.GetVolumeCapability().GetMount() != nil {
			if req.GetVolumeCapability().GetMount().GetFsType() != "" {
				fsType = req.GetVolumeCapability().GetMount().GetFsType()
			}
		}
		// Format and mount the ublk device to staging path (runs mkfs + mount —
		// can take seconds; no shared state, no lock needed).
		if err := ns.mountManager.FormatAndMountDevice(ublkDevicePath, stagingPath, fsType); err != nil {
			// Cleanup ublk device on failure
			ns.ublkManager.DeleteUblkDevice(volumeID, ublkDevicePath)
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to mount device: %v", err))
		}
	}

	// Parse volume ID to UUID
	volUUID, err := uuid.Parse(volumeID)
	if err != nil {
		klog.Errorf("Failed to parse volume ID %s: %v", volumeID, err)
		ns.ublkManager.DeleteUblkDevice(volumeID, ublkDevicePath)
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("Invalid volume ID format: %v", err))
	}

	nodeVolume := &types.NodeVolume{
		VolID:       volUUID,
		UblkPath:    ublkDevicePath,
		UblkPid:     ublkpid,
		VolumeMode:  mode,
		StagingPath: stagingPath,
	}

	// Write lock: only the map insertion needs exclusive access.
	// All the expensive work above runs concurrently for different volumes.
	ns.mutex.Lock()
	ns.Node.VolMap[volumeID] = nodeVolume
	ns.mutex.Unlock()

	klog.Infof("Successfully staged volume %s with ublk device %s at %s", volumeID, ublkDevicePath, stagingPath)
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.Infof("NodeUnstageVolume: called with args %+v", req)

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}

	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Staging target path cannot be empty")
	}

	volumeID := req.GetVolumeId()
	stagingPath := req.GetStagingTargetPath()

	ns.mutex.RLock()
	nodeVol, exists := ns.Node.VolMap[volumeID]
	ns.mutex.RUnlock()

	if !exists {
		klog.Warningf("Volume %s not found in node map, considering it already unstaged", volumeID)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	// Unmount from staging path. Called unconditionally rather than gated
	// on nodeVol.VolumeMode: VolMap can hold a bare control-plane stub (see
	// ConfigManager.UpdateNodeVolumeMap) with no VolumeMode recorded, and
	// Unmount is already a safe no-op when nothing is mounted there.
	if err := ns.mountManager.Unmount(stagingPath); err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to unmount staging path: %v", err))
	}

	// Delete the ublk device. Fall back to the deterministic udev symlink
	// path when UblkPath wasn't recorded (same stub scenario as above), so
	// cleanup still runs instead of leaking the host-side systemd unit.
	ublkPath := nodeVol.UblkPath
	if ublkPath == "" {
		ublkPath = types.UblkByUUIDPath(volumeID)
	}
	if err := ns.ublkManager.DeleteUblkDevice(volumeID, ublkPath); err != nil {
		klog.Errorf("Failed to delete ublk device %s: %v", ublkPath, err)
	}

	// Clean up staging path
	if err := ns.mountManager.CleanupMountPoint(stagingPath); err != nil {
		klog.Warningf("Failed to cleanup staging path %s: %v", stagingPath, err)
	}

	// Write lock: remove the entry from the shared map.
	ns.mutex.Lock()
	delete(ns.Node.VolMap, volumeID)
	ns.mutex.Unlock()

	klog.Infof("Successfully unstaged volume %s", volumeID)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.Infof("NodePublishVolume: called with args %+v", req)

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}

	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path cannot be empty")
	}

	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Staging target path cannot be empty")
	}

	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	stagingPath := req.GetStagingTargetPath()

	ns.mutex.RLock()
	nodeVol, exists := ns.Node.VolMap[volumeID]
	ns.mutex.RUnlock()

	if !exists {
		return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("Volume %s not staged", volumeID))
	}

	if req.GetVolumeCapability().GetBlock() != nil {
		// Ensure parent dir exists
		if err := os.MkdirAll(filepath.Dir(targetPath), 0750); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}

		// Create target file
		f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		f.Close()
		// Block volume - bind mount the ublk device directly
		if err := ns.mountManager.BindRawBlock(nodeVol.UblkPath, targetPath); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to bind mount block device: %v", err))
		}
	} else {
		// Filesystem volume - bind mount from staging path
		if err := ns.mountManager.BindMount(stagingPath, targetPath); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to bind mount filesystem: %v", err))
		}
	}

	// Write lock: update the TargetPath field on the shared NodeVolume struct.
	ns.mutex.Lock()
	nodeVol.TargetPath = targetPath
	ns.mutex.Unlock()
	klog.Infof("Successfully published volume %s to %s", volumeID, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.Infof("NodeUnpublishVolume: called with args %+v", req)

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}

	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path cannot be empty")
	}

	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	ns.mutex.RLock()
	nodeVol, exists := ns.Node.VolMap[volumeID]
	ns.mutex.RUnlock()

	if !exists {
		klog.Warningf("Volume %s not found in node map, considering it already unpublished", volumeID)
	}

	// --- Filesystem / unmount operations: no lock held ---
	if err := ns.mountManager.Unmount(targetPath); err != nil {
		if os.IsNotExist(err) {
			klog.Infof("Target path %s already removed", targetPath)
		} else {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to unmount target path: %v", err))
		}
	}

	if exists {
		// Determine block vs. filesystem mode from the on-disk shape of
		// targetPath rather than the stored VolumeMode: VolMap can hold a
		// bare control-plane stub (see ConfigManager.UpdateNodeVolumeMap)
		// with no VolumeMode recorded. NodePublishVolume always creates a
		// plain file for block mode (bind-mounting the ublk device onto
		// it) and a directory for filesystem mode, so stat is authoritative
		// regardless of what VolMap knows.
		isDir := false
		if fi, err := os.Stat(targetPath); err == nil {
			isDir = fi.IsDir()
		} else if !os.IsNotExist(err) {
			klog.Warningf("Failed to stat target path %s to determine volume mode: %v", targetPath, err)
		}
		if !isDir {
			_ = os.Remove(targetPath)
		} else {
			if err := ns.mountManager.CleanupMountPoint(targetPath); err != nil {
				klog.Warningf("Failed to cleanup target path %s: %v", targetPath, err)
			}
		}
		// Write lock: clear the TargetPath field on the shared NodeVolume struct.
		ns.mutex.Lock()
		nodeVol.TargetPath = ""
		ns.mutex.Unlock()
	}

	klog.Infof("Successfully unpublished volume %s from %s", volumeID, targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	klog.Infof("NodeGetVolumeStats: called with args %+v", req)

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}

	if req.GetVolumePath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume path cannot be empty")
	}

	ns.mutex.RLock()
	nodeVol, ok := ns.Node.VolMap[req.GetVolumeId()]
	ns.mutex.RUnlock()

	if ok && nodeVol.VolumeMode == types.BLOCK_MODE {
		return nil, status.Error(codes.Unimplemented, "Block volume stats not supported")
	}

	volumePath := req.GetVolumePath()

	// Check if path exists
	if _, err := os.Stat(volumePath); err != nil {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume path %s not found", volumePath))
	}

	// Get filesystem stats
	// For now, return basic stats - can be enhanced with actual filesystem usage
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:  csi.VolumeUsage_BYTES,
				Total: 0, // TODO: Get actual filesystem stats
				Used:  0,
			},
		},
	}, nil
}

func (ns *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeExpandVolume is not implemented")
}

func (ns *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: ns.caps,
	}, nil
}

func (ns *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: ns.nodeID,
	}, nil
}
