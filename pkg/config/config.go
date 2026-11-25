package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	cpClient "github.com/00pauln00/niova-mdsvc/controlplane/ctlplanefuncs/client"
	ctlplfl "github.com/00pauln00/niova-mdsvc/controlplane/ctlplanefuncs/lib"
	userClient "github.com/00pauln00/niova-mdsvc/controlplane/user/client"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/niova-block-csi/pkg/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

type ConfigManager struct {
	CpConfigPath string
	Controller   *types.Controller
	Mutex        sync.RWMutex
	K8sClient    *kubernetes.Clientset
}

func NewConfigManager(cpConfigPath string) *ConfigManager {
	return &ConfigManager{
		CpConfigPath: cpConfigPath,
		Controller:   &types.Controller{},
	}
}

func StartAuthClient(raftuuid, raftconfig string) (*userClient.Client, func()) {
	cfg := userClient.Config{
		AppUUID:          uuid.New().String(),
		RaftUUID:         raftuuid,
		GossipConfigPath: raftconfig,
	}

	c, tearDown := userClient.New(cfg)
	return c, tearDown
}

func NewK8sController() (*kubernetes.Clientset, error) {
	// Use in-cluster config — works automatically when running inside Kubernetes
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load in-cluster config: %v", err)
	}

	// Create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %v", err)
	}

	return clientset, nil
}

func (cm *ConfigManager) NodeExists(nodeID string) (bool, error) {
	if cm.K8sClient == nil {
		return false, fmt.Errorf("k8s client is nil")
	}
	_, err := cm.K8sClient.CoreV1().Nodes().Get(context.Background(), nodeID, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (cm *ConfigManager) InitNiovaClient(c *cpClient.CliCFuncs, u *userClient.Client) error {
	if cm == nil {
		return fmt.Errorf("ConfigManager is nil")
	}
	if c == nil {
		return fmt.Errorf("CP client Cannot be Nil")
	}
	if u == nil {
		return fmt.Errorf("User client Cannot be Nil")
	}
	if cm.Controller == nil {
		cm.Controller = &types.Controller{}
	}
	cm.Controller.Cpclient = c
	cm.Controller.UserClient = u
	return nil
}

func (cm *ConfigManager) UserLogin() error {
	klog.Infof("Loging in the user with env-var %s  value %s is applied from environment", types.NiovaUserName, os.Getenv(types.NiovaUserName))
	klog.Infof("Loging in the user with env-var %s value %s is applied from environment", types.NiovaUserSecret, os.Getenv(types.NiovaUserSecret))
	klog.Infof("Loging in the user with env-var %s value %s is applied from environment", types.NiovaMdsvcChunkLimit, os.Getenv(types.NiovaMdsvcChunkLimit))
	klog.Infof("Logging in the use wih env-var %s value %s is  applied from enviornment", types.NiovaTenantUuid, os.Getenv(types.NiovaTenantUuid))
	token, err := cm.Controller.UserClient.LoginWithTenant(os.Getenv(types.NiovaUserName), os.Getenv(types.NiovaUserSecret), os.Getenv(types.NiovaTenantUuid))
	if err != nil {
		klog.Errorf("Failed to Login admin user", err)
		return err
	}
	cm.Controller.Usertoken = token.AccessToken
	cm.Controller.Cpclient.SetToken(token.AccessToken)
	return nil
}

func (cm *ConfigManager) VerifyTokenExpiryAndReLogin(exp error) error {
	if errors.Is(exp, jwt.ErrTokenExpired) || strings.Contains(exp.Error(), "expired") || strings.Contains(exp.Error(), "Invalid Token") {
		err := cm.UserLogin()
		if err != nil {
			return err
		}
	} else {
		klog.Errorf("Token Verification failed with different error: %v", exp)
		return exp
	}
	return nil
}

func (cm *ConfigManager) GetController() *types.Controller {
	cm.Mutex.Lock()
	defer cm.Mutex.Unlock()
	return cm.Controller
}

func parseFDType(s string) ctlplfl.FD {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pdu":
		return ctlplfl.FD_PDU
	case "rack":
		return ctlplfl.FD_RACK
	case "hv", "hypervisor":
		return ctlplfl.FD_HV
	case "device":
		return ctlplfl.FD_DEVICE
	case "partition":
		return ctlplfl.FD_PARTITION
	default:
		return ctlplfl.FD_ANY
	}
}

func (cm *ConfigManager) RetryAuth(fn func() error) error {
	for i := 0; i < types.MAX_RETRY; i++ {
		err := fn()
		if err == nil {
			return nil
		}

		if exp := cm.VerifyTokenExpiryAndReLogin(err); exp != nil {
			return fmt.Errorf("failed to relogin: %w", err)
		}
	}

	return fmt.Errorf("operation failed after %d retries", types.MAX_RETRY)
}

func (cm *ConfigManager) AllocVdev(name string, requiredSize int64, filter, entityID, pfsId string) (string, error) {
	cm.Mutex.RLock()
	defer cm.Mutex.RUnlock()
	klog.Infof("Allocate vdev with failure domain: %s", entityID)
	// TODO: NumReplica should be passed from PVC file.
	Vdev := &ctlplfl.VdevReq{
		Vdev: &ctlplfl.VdevConfig{
			Name:       name,
			Size:       requiredSize,
			DataBlkCnt: 1,
			PFSID:      pfsId,
		},
		Filter: ctlplfl.Filter{
			ID:   entityID,
			Type: parseFDType(filter),
		},
	}
	klog.Infof("Create vdev of size", Vdev.Vdev.Size)
	var resp *ctlplfl.ResponseXML
	err := cm.RetryAuth(func() error {
		var err error
		resp, err = cm.Controller.Cpclient.CreateVdev(Vdev)
		return err
	})

	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (cm *ConfigManager) RemoveVolume(volumeID string) (string, error) {
	Req := &ctlplfl.DeleteVdevReq{
		ID: volumeID,
	}
	klog.Infof("Delete Vdev ID:", volumeID)
	// check if token expired
	var resp *ctlplfl.ResponseXML
	err := cm.RetryAuth(func() error {
		var err error
		resp, err = cm.Controller.Cpclient.DeleteVdev(Req)
		return err
	})

	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (cm *ConfigManager) GetVolume(volumeID string) (ctlplfl.VdevConfig, error) {
	vdevreq := &ctlplfl.GetReq{
		ID: volumeID,
	}
	var vdevcfg ctlplfl.VdevConfig
	err := cm.RetryAuth(func() error {
		var err error
		vdevcfg, err = cm.Controller.Cpclient.GetVdevConfig(vdevreq)
		return err
	})
	if err != nil {
		return ctlplfl.VdevConfig{}, err
	}
	return vdevcfg, nil
}

// GetVolumeByName looks up a vdev by its CSI volume name instead of its ID.
// The control plane's GetReq.ID accepts either: a UUID is looked up
// directly, anything else is resolved as a name via a server-side reverse
// index. Returns an error whose message is exactly "vdev not found" when
// no vdev with that name exists.
func (cm *ConfigManager) GetVolumeByName(name string) (ctlplfl.VdevConfig, error) {
	return cm.GetVolume(name)
}

func (cm *ConfigManager) ListVolumes() ([]ctlplfl.VdevConfig, error) {
	Req := &ctlplfl.GetReq{
		GetAll: true,
	}
	var vdevcfgs []ctlplfl.VdevConfig
	err := cm.RetryAuth(func() error {
		var err error
		vdevcfgs, err = cm.Controller.Cpclient.GetVdevConfigs(Req)
		return err
	})
	if err != nil {
		return []ctlplfl.VdevConfig{}, err
	}
	return vdevcfgs, nil
}

// UpdateNodeVolumeMap seeds node.VolMap at node-plugin startup from the
// control plane's vdev list. ListVolumes is cluster-wide — the control
// plane doesn't (yet) track which node a vdev is attached to — so most of
// what it returns has nothing to do with this node. The deterministic udev
// symlink niova-ublk creates (types.UblkByUUIDPath) is used as local ground
// truth instead: only a vdev whose ublk backend is actually running on this
// host resolves that path, so that's what tells us it belongs here. Vdevs
// that don't resolve are skipped rather than added as bare stubs.
func (cm *ConfigManager) UpdateNodeVolumeMap(node *types.Node) error {
	vdevcfgs, err := cm.ListVolumes()
	if err != nil {
		return err
	}

	if node.VolMap == nil {
		node.VolMap = make(map[string]*types.NodeVolume)
	}

	for _, v := range vdevcfgs {
		volID := v.ID

		ublkPath := types.UblkByUUIDPath(volID)
		if _, err := os.Stat(ublkPath); err != nil {
			continue
		}

		nv, exists := node.VolMap[volID]
		if !exists {
			nv = &types.NodeVolume{}
			node.VolMap[volID] = nv
		}

		// Update fields from controller config
		nv.VolID, err = uuid.Parse(v.ID)
		if err != nil {
			klog.Errorf("Failed to parse volume ID %s: %v", v.ID, err)
			return err
		}
		nv.UblkPath = ublkPath
	}

	return nil
}
