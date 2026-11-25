package types

import (
	cpClient "github.com/00pauln00/niova-mdsvc/controlplane/ctlplanefuncs/client"
	userClient "github.com/00pauln00/niova-mdsvc/controlplane/user/client"
	"github.com/google/uuid"
)

type VolumeStatus string

const (
	SrcCP                             = "control-plane"
	BLOCK_MODE                        = "block"
	MOUNT_MODE                        = "mount"
	VolumeStatusCreated  VolumeStatus = "created"
	VolumeStatusAttached VolumeStatus = "attached"
	VolumeStatusDetached VolumeStatus = "detached"
	VolumeStatusDeleted  VolumeStatus = "deleted"

	NiovaUserName        = "NIOVA_BLOCK_CP_AUTH_USERNAME"
	NiovaUserSecret      = "NIOVA_BLOCK_CP_AUTH_SECRET"
	NiovaGossipKey       = "NIOVA_GOSSIP_KEY"
	NiovaGossipPath      = "NIOVA_GOSSIP_PATH"
	NiovaUblkUnified     = "NIOVA_BLOCK_UBLK_UNIFIED"
	NiovaMdsvcChunkLimit = "NIOVA_BLOCK_MDSVC_GET_CHUNKS_LIMIT"
	NiovaTenantUuid      = "NIOVA_TENANT_UUID"

	FailureDomain = "failuredomain"
	EntityID      = "entityID"
	PfsID         = "pfsId"
	UblkRecovery  = "ublkrecovery"
	MAX_RETRY     = 2
)

type Controller struct {
	Cpclient   *cpClient.CliCFuncs
	UserClient *userClient.Client
	Usertoken  string
}

type NodeVolume struct {
	VolID       uuid.UUID `yaml:"volumeID" json:"volumeID"`
	UblkPath    string    `yaml:"ublkPath" json:"ublkPath"`
	UblkPid     int       `yaml:"ublkPid" json:"ublkPid"`
	VolumeMode  string    `yaml:"volumeMode" json:"volumeMode"`
	StagingPath string    `yaml:"stagingPath" json:"stagingPath"`
	TargetPath  string    `yaml:"targetPath" json:"targetPath"`
}

type Node struct {
	VolMap map[string]*NodeVolume `yaml:"volMap" json:"volMap"`
}

// UblkByUUIDPath returns the deterministic device path that
// 61-niova-ublk.rules creates as /dev/disk/by-uuid/<volumeID> once the
// niova-ublk backend for that volume is ready. It's used both to wait for a
// freshly-started backend to come up and, at node-plugin startup, as local
// ground truth for which control-plane-known volumes actually have a live
// backend running on this node.
func UblkByUUIDPath(volumeID string) string {
	return "/dev/disk/by-uuid/" + volumeID
}
