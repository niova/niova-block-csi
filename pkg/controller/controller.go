package controller

import (
	"context"
	"fmt"
	"strconv"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/niova-block-csi/pkg/config"
	"github.com/niova-block-csi/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

type ControllerServer struct {
	config *config.ConfigManager
	caps   []*csi.ControllerServiceCapability
}

func NewControllerServer(configManager *config.ConfigManager) *ControllerServer {
	implementedRPCs := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
	}
	var caps []*csi.ControllerServiceCapability
	for _, rpcType := range implementedRPCs {
		caps = append(caps, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: rpcType,
				},
			},
		})
	}

	return &ControllerServer{
		config: configManager,
		caps:   caps,
	}
}

func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	klog.Infof("CreateVolume: called with args %+v", req)

	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume name cannot be empty")
	}

	for _, cap := range req.GetVolumeCapabilities() {
		if cap == nil || cap.GetAccessMode() == nil {
			continue
		}

		switch cap.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			return nil, status.Error(
				codes.Unsupported,
				"ReadWriteMany (MULTI_NODE_MULTI_WRITER) is not supported",
			)
		}
	}

	if req.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "Capacity range cannot be empty")
	}

	volumeSize := req.GetCapacityRange().GetRequiredBytes()
	if volumeSize == 0 {
		volumeSize = 1024 * 1024 * 1024 // 1GB default
	}
	caps := req.GetVolumeCapabilities()
	if len(caps) == 0 {
		return nil, status.Error(codes.InvalidArgument, "VolumeCapabilities missing")
	}
	cap := caps[0]
	if cap.GetMount() == nil && cap.GetBlock() == nil {
		return nil, status.Error(codes.InvalidArgument, "Unsupported volume capability")
	}

	volumeName := req.GetName()

	// Idempotency: a vdev with this name may already exist from a
	// previous, possibly retried, CreateVolume call. Return it as-is
	// instead of allocating a duplicate. Any error from the lookup
	// (including "not found") is treated as "no existing volume" and
	// falls through to AllocVdev below.
	if existing, err := cs.config.GetVolumeByName(volumeName); err == nil {
		if existing.Size != volumeSize {
			return nil, status.Error(codes.AlreadyExists,
				fmt.Sprintf("volume %s already exists with a different capacity", volumeName))
		}
		klog.Infof("Volume %s already exists as %s, returning existing volume", volumeName, existing.ID)
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      existing.ID,
				CapacityBytes: existing.Size,
			},
		}, nil
	}

	p := req.GetParameters()
	fd := p[types.FailureDomain]
	entityId := p[types.EntityID]
	pfsId := p[types.PfsID]
	klog.Infof("failuredomain provided is %s  and entityId provided is %s and  pfsId provided is %s", fd, entityId, pfsId)

	// Allocate Vdev of required size
	volumeID, err := cs.config.AllocVdev(volumeName, volumeSize, fd, entityId, pfsId)
	if err != nil {
		klog.Errorf("Failed to Allocate Vdev with error : %v", err)
		return nil, status.Error(codes.ResourceExhausted, err.Error())
	}

	klog.Infof("Created volume %s of size %d bytes on NISD %s", volumeID, volumeSize)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: volumeSize,
		},
	}, nil
}

func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	cs.config.Mutex.Lock()
	defer cs.config.Mutex.Unlock()
	klog.Infof("DeleteVolume: called with args %+v", req)

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}

	volumeID := req.GetVolumeId()

	// Get volume info
	Vol, err := cs.config.GetVolume(volumeID)
	if err != nil {
		klog.Warningf("Volume %s not found, considering it already deleted", volumeID)
		return &csi.DeleteVolumeResponse{}, nil
	}

	// Remove volume from config
	vid, err := cs.config.RemoveVolume(volumeID)
	if err != nil {
		klog.Errorf("Failed to remove volume from cp: %v", err)
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to delete volume: %v", err))
	}

	klog.Infof("Deleted the volume %s with size", vid, Vol.Size)

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	klog.Infof("ControllerPublishVolume: called with args %+v", req)

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}

	if req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Node ID cannot be empty")
	}

	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability cannot be empty")
	}
	nodeID := req.GetNodeId()
	volumeID := req.GetVolumeId()
	exists, err := cs.config.NodeExists(nodeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check node %s: %v", nodeID, err)
	}
	if !exists {
		return nil, status.Errorf(codes.NotFound, "node %s not found", nodeID)
	}

	// Get volume info
	cs.config.Mutex.Lock()
	Vol, err := cs.config.GetVolume(volumeID)
	if err != nil {
		klog.Errorf("Volume %s not found: %v", volumeID, err)
		cs.config.Mutex.Unlock()
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume %s not found", volumeID))
	}

	cs.config.Mutex.Unlock()
	klog.Infof("Published volume %s to node %s", volumeID, nodeID)

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{
			"volumeID":   volumeID,
			"volumeSize": fmt.Sprintf("%d", Vol.Size),
		},
	}, nil
}

func (cs *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	klog.Infof("ControllerUnpublishVolume: called with args %+v", req)

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}

	volumeID := req.GetVolumeId()
	cs.config.Mutex.Lock()
	// Check if volume exists
	Vol, err := cs.config.GetVolume(volumeID)
	if err != nil {
		klog.Warningf("Volume %s not found, considering it already detached", volumeID)
		cs.config.Mutex.Unlock()
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	cs.config.Mutex.Unlock()

	klog.Infof("Unpublished volume %s with size", volumeID, Vol.Size)

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (cs *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	klog.Infof("ValidateVolumeCapabilities: called with args %+v", req)

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID cannot be empty")
	}

	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities cannot be empty")
	}

	// Check if volume exists
	cs.config.Mutex.Lock()
	_, err := cs.config.GetVolume(req.GetVolumeId())
	if err != nil {
		cs.config.Mutex.Unlock()
		return nil, status.Error(codes.NotFound, fmt.Sprintf("Volume %s not found", req.GetVolumeId()))
	}
	cs.config.Mutex.Unlock()
	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetBlock() != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{}, nil
		}
		if cap.GetMount() != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{}, nil
		}
	}
	// For now, we support all requested capabilities
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}

func (cs *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	klog.Infof("ListVolumes: called with args %+v", req)

	var entries []*csi.ListVolumesResponse_Entry
	// The starting_token is a pagination parameter in the CSI ListVolumes API.
	// It's used to resume listing volumes from where a previous request left
	// off when there are many volumes.
	// Validate starting_token if provided
	// TODO: Replace the current starting token implementation
	// with proper pagination handling
	startingToken := 0
	if req.GetStartingToken() != "" {
		token := req.GetStartingToken()

		// Parse the token - must be a valid non-negative integer
		parsed, err := strconv.Atoi(token)
		if err != nil || parsed < 0 {
			return nil, status.Error(codes.Aborted, fmt.Sprintf("invalid starting_token: %s", token))
		}
		startingToken = parsed
	}

	klog.Infof("Using starting_token: %d", startingToken)
	vols, err := cs.config.ListVolumes()
	if err != nil {
		klog.Errorf("Failed to get volumes list from cp: %v", err)
		return nil, status.Error(codes.Internal, fmt.Sprintf("Failed to list volumes: %v", err))
	}
	for _, v := range vols {
		entry := &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:      v.ID,
				CapacityBytes: int64(v.Size),
			},
		}
		entries = append(entries, entry)
	}
	return &csi.ListVolumesResponse{
		Entries: entries,
	}, nil
}

func (cs *ControllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	klog.Infof("GetCapacity: called with args %+v", req)

	var totalCapacity int64
	return &csi.GetCapacityResponse{
		AvailableCapacity: totalCapacity,
	}, nil
}

func (cs *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.caps,
	}, nil
}

func (cs *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "CreateSnapshot is not implemented")
}

func (cs *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "DeleteSnapshot is not implemented")
}

func (cs *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListSnapshots is not implemented")
}

func (cs *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerExpandVolume is not implemented")
}

func (cs *ControllerServer) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerGetVolume is not implemented")
}
