/*
Copyright 2018 The Ceph-CSI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cephfs

import (
	"context"
	"fmt"
	"os"

	csicommon "github.com/ceph/ceph-csi/pkg/csi-common"
	"github.com/ceph/ceph-csi/pkg/util"
	csipbv1 "github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/volume"
)

// NodeServer struct of ceph CSI driver with supported methods of CSI
// node server spec.
type NodeServer struct {
	*csicommon.DefaultNodeServer
}

var (
	nodeVolumeIDLocker = util.NewIDLocker()
)

func getCredentialsForVolume(volOptions *volumeOptions, req *csi.NodeStageVolumeRequest) (*util.Credentials, error) {
	var (
		cr      *util.Credentials
		secrets = req.GetSecrets()
	)

	if volOptions.ProvisionVolume {
		// The volume is provisioned dynamically, get the credentials directly from Ceph

		// First, get admin credentials - those are needed for retrieving the user credentials

		adminCr, err := util.GetAdminCredentials(secrets)
		if err != nil {
			return nil, fmt.Errorf("failed to get admin credentials from node stage secrets: %v", err)
		}

		cr = adminCr
	} else {
		// The volume is pre-made, credentials are in node stage secrets

		userCr, err := util.GetUserCredentials(req.GetSecrets())
		if err != nil {
			return nil, fmt.Errorf("failed to get user credentials from node stage secrets: %v", err)
		}

		cr = userCr
	}

	return cr, nil
}

// NodeStageVolume mounts the volume to a staging path on the node.
func (ns *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	var (
		volOptions *volumeOptions
	)
	if err := util.ValidateNodeStageVolumeRequest(req); err != nil {
		return nil, err
	}

	// Configuration

	stagingTargetPath := req.GetStagingTargetPath()
	volID := volumeID(req.GetVolumeId())

	volOptions, _, err := newVolumeOptionsFromVolID(string(volID), req.GetVolumeContext(), req.GetSecrets())
	if err != nil {
		if _, ok := err.(ErrInvalidVolID); !ok {
			return nil, status.Error(codes.Internal, err.Error())
		}

		// check for pre-provisioned volumes (plugin versions > 1.0.0)
		volOptions, _, err = newVolumeOptionsFromStaticVolume(string(volID), req.GetVolumeContext())
		if err != nil {
			if _, ok := err.(ErrNonStaticVolume); !ok {
				return nil, status.Error(codes.Internal, err.Error())
			}

			// check for volumes from plugin versions <= 1.0.0
			volOptions, _, err = newVolumeOptionsFromVersion1Context(string(volID), req.GetVolumeContext(),
				req.GetSecrets())
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		}
	}

	if err = util.CreateMountPoint(stagingTargetPath); err != nil {
		klog.Errorf("failed to create staging mount point at %s for volume %s: %v", stagingTargetPath, volID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	idLk := nodeVolumeIDLocker.Lock(string(volID))
	defer nodeVolumeIDLocker.Unlock(idLk, string(volID))

	// Check if the volume is already mounted

	isMnt, err := util.IsMountPoint(stagingTargetPath)

	if err != nil {
		klog.Errorf("stat failed: %v", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	if isMnt {
		klog.Infof("cephfs: volume %s is already mounted to %s, skipping", volID, stagingTargetPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// It's not, mount now
	if err = ns.mount(volOptions, req); err != nil {
		return nil, err
	}

	klog.Infof("cephfs: successfully mounted volume %s to %s", volID, stagingTargetPath)

	return &csi.NodeStageVolumeResponse{}, nil
}

func (*NodeServer) mount(volOptions *volumeOptions, req *csi.NodeStageVolumeRequest) error {
	stagingTargetPath := req.GetStagingTargetPath()
	volID := volumeID(req.GetVolumeId())

	cr, err := getCredentialsForVolume(volOptions, req)
	if err != nil {
		klog.Errorf("failed to get ceph credentials for volume %s: %v", volID, err)
		return status.Error(codes.Internal, err.Error())
	}

	m, err := newMounter(volOptions)
	if err != nil {
		klog.Errorf("failed to create mounter for volume %s: %v", volID, err)
		return status.Error(codes.Internal, err.Error())
	}

	klog.V(4).Infof("cephfs: mounting volume %s with %s", volID, m.name())

	if err = m.mount(stagingTargetPath, cr, volOptions); err != nil {
		klog.Errorf("failed to mount volume %s: %v", volID, err)
		return status.Error(codes.Internal, err.Error())
	}
	if err := volumeMountCache.nodeStageVolume(req.GetVolumeId(), stagingTargetPath, volOptions.Mounter, req.GetSecrets()); err != nil {
		klog.Warningf("mount-cache: failed to stage volume %s %s: %v", volID, stagingTargetPath, err)
	}
	return nil
}

// NodePublishVolume mounts the volume mounted to the staging path to the target
// path
func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {

	mountOptions := []string{"bind"}
	if err := util.ValidateNodePublishVolumeRequest(req); err != nil {
		return nil, err
	}

	// Configuration

	targetPath := req.GetTargetPath()
	volID := req.GetVolumeId()

	if err := util.CreateMountPoint(targetPath); err != nil {
		klog.Errorf("failed to create mount point at %s: %v", targetPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	volCap := req.GetVolumeCapability()

	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	if m := volCap.GetMount(); m != nil {
		hasOption := func(options []string, opt string) bool {
			for _, o := range options {
				if o == opt {
					return true
				}
			}
			return false
		}
		for _, f := range m.MountFlags {
			if !hasOption(mountOptions, f) {
				mountOptions = append(mountOptions, f)
			}
		}
	}

	// Check if the volume is already mounted

	isMnt, err := util.IsMountPoint(targetPath)

	if err != nil {
		klog.Errorf("stat failed: %v", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	if isMnt {
		klog.Infof("cephfs: volume %s is already bind-mounted to %s", volID, targetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// It's not, mount now

	if err = bindMount(req.GetStagingTargetPath(), req.GetTargetPath(), req.GetReadonly(), mountOptions); err != nil {
		klog.Errorf("failed to bind-mount volume %s: %v", volID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err = volumeMountCache.nodePublishVolume(volID, targetPath, req.GetReadonly()); err != nil {
		klog.Warningf("mount-cache: failed to publish volume %s %s: %v", volID, targetPath, err)
	}

	klog.Infof("cephfs: successfully bind-mounted volume %s to %s", volID, targetPath)

	err = os.Chmod(targetPath, 0777)
	if err != nil {
		klog.Errorf("failed to change targetpath permission for volume %s: %v", volID, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts the volume from the target path
func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	var err error
	if err = util.ValidateNodeUnpublishVolumeRequest(req); err != nil {
		return nil, err
	}

	targetPath := req.GetTargetPath()

	volID := req.GetVolumeId()
	if err = volumeMountCache.nodeUnPublishVolume(volID, targetPath); err != nil {
		klog.Warningf("mount-cache: failed to unpublish volume %s %s: %v", volID, targetPath, err)
	}

	// Unmount the bind-mount
	if err = unmountVolume(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err = os.Remove(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.Infof("cephfs: successfully unbinded volume %s from %s", req.GetVolumeId(), targetPath)

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeUnstageVolume unstages the volume from the staging path
func (ns *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	var err error
	if err = util.ValidateNodeUnstageVolumeRequest(req); err != nil {
		return nil, err
	}

	stagingTargetPath := req.GetStagingTargetPath()

	volID := req.GetVolumeId()
	if err = volumeMountCache.nodeUnStageVolume(volID); err != nil {
		klog.Warningf("mount-cache: failed to unstage volume %s %s: %v", volID, stagingTargetPath, err)
	}

	// Unmount the volume
	if err = unmountVolume(stagingTargetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err = os.Remove(stagingTargetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	klog.Infof("cephfs: successfully unmounted volume %s from %s", req.GetVolumeId(), stagingTargetPath)

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *NodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {

	var err error
	targetPath := req.GetVolumePath()
	if targetPath == "" {
		err = fmt.Errorf("targetpath %v is empty", targetPath)
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	/*
		 volID := req.GetVolumeId()

		 TODO: Map the volumeID to the targetpath.

		we need secret to connect to the ceph cluster to get the volumeID from volume
		Name, however `secret` field/option is not available  in NodeGetVolumeStats spec,
		Below issue covers this request and once its available, we can do the validation
		as per the spec.
		https://github.com/container-storage-interface/spec/issues/371

	*/

	isMnt, err := util.IsMountPoint(targetPath)

	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.InvalidArgument, "targetpath %s doesnot exist", targetPath)
		}
		return nil, err
	}
	if !isMnt {
		return nil, status.Errorf(codes.InvalidArgument, "targetpath %s is not mounted", targetPath)
	}

	cephfsProvider := volume.NewMetricsStatFS(targetPath)
	volMetrics, volMetErr := cephfsProvider.GetMetrics()
	if volMetErr != nil {
		return nil, status.Error(codes.Internal, volMetErr.Error())
	}

	available, ok := (*(volMetrics.Available)).AsInt64()
	if !ok {
		klog.Errorf("failed to fetch available bytes")
	}
	capacity, ok := (*(volMetrics.Capacity)).AsInt64()
	if !ok {
		klog.Errorf("failed to fetch capacity bytes")
		return nil, status.Error(codes.Unknown, "failed to fetch capacity bytes")
	}
	used, ok := (*(volMetrics.Used)).AsInt64()
	if !ok {
		klog.Errorf("failed to fetch used bytes")
	}
	inodes, ok := (*(volMetrics.Inodes)).AsInt64()
	if !ok {
		klog.Errorf("failed to fetch available inodes")
		return nil, status.Error(codes.Unknown, "failed to fetch available inodes")

	}
	inodesFree, ok := (*(volMetrics.InodesFree)).AsInt64()
	if !ok {
		klog.Errorf("failed to fetch free inodes")
	}

	inodesUsed, ok := (*(volMetrics.InodesUsed)).AsInt64()
	if !ok {
		klog.Errorf("failed to fetch used inodes")
	}
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: available,
				Total:     capacity,
				Used:      used,
				Unit:      csipbv1.VolumeUsage_BYTES,
			},
			{
				Available: inodesFree,
				Total:     inodes,
				Used:      inodesUsed,
				Unit:      csipbv1.VolumeUsage_INODES,
			},
		},
	}, nil

}

// NodeGetCapabilities returns the supported capabilities of the node server
func (ns *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
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
	}, nil
}
