// +build windows

package hcsoci

import (
	"fmt"
	"path"
	"path/filepath"

	"github.com/Microsoft/hcsshim/internal/ospath"
	"github.com/Microsoft/hcsshim/internal/schema2"
	"github.com/Microsoft/hcsshim/internal/uvm"
	"github.com/Microsoft/hcsshim/internal/wclayer"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type vpMemEntry struct {
	hostPath string
	uvmPath  string
}

const scratchPath = "scratch"

// mountContainerLayers is a helper for clients to hide all the complexity of layer mounting
// Layer folder are in order: base, [rolayer1..rolayern,] scratch
//
// v1/v2: Argon WCOW: Returns the mount path on the host as a volume GUID.
// v1:    Xenon WCOW: Done internally in HCS, so no point calling doing anything here.
// v2:    Xenon WCOW: Returns a CombinedLayersV2 structure where ContainerRootPath is a folder
//                    inside the utility VM which is a GUID mapping of the scratch folder. Each
//                    of the layers are the VSMB locations where the read-only layers are mounted.
//
func mountContainerLayers(layerFolders []string, guestRoot string, uvm *uvm.UtilityVM) (interface{}, error) {
	logrus.Debugln("hcsshim::mountContainerLayers", layerFolders)

	if uvm == nil {
		if len(layerFolders) < 2 {
			return nil, fmt.Errorf("need at least two layers - base and scratch")
		}
		path := layerFolders[len(layerFolders)-1]
		rest := layerFolders[:len(layerFolders)-1]
		logrus.Debugln("hcsshim::mountContainerLayers ActivateLayer", path)
		if err := wclayer.ActivateLayer(path); err != nil {
			return nil, err
		}
		logrus.Debugln("hcsshim::mountContainerLayers Preparelayer", path, rest)
		if err := wclayer.PrepareLayer(path, rest); err != nil {
			if err2 := wclayer.DeactivateLayer(path); err2 != nil {
				logrus.Warnf("Failed to Deactivate %s: %s", path, err)
			}
			return nil, err
		}

		mountPath, err := wclayer.GetLayerMountPath(path)
		if err != nil {
			if err := wclayer.UnprepareLayer(path); err != nil {
				logrus.Warnf("Failed to Unprepare %s: %s", path, err)
			}
			if err2 := wclayer.DeactivateLayer(path); err2 != nil {
				logrus.Warnf("Failed to Deactivate %s: %s", path, err)
			}
			return nil, err
		}
		return mountPath, nil
	}

	// V2 UVM
	logrus.Debugf("hcsshim::mountContainerLayers Is a %s V2 UVM", uvm.OS())

	// 	Add each read-only layers. For Windows, this is a VSMB share with the ResourceUri ending in
	// a GUID based on the folder path. For Linux, this is a VPMEM device.
	//
	//  Each layer is ref-counted so that multiple containers in the same utility VM can share them.
	var vsmbAdded []string
	var vpmemAdded []vpMemEntry
	attachedSCSIHostPath := ""

	for _, layerPath := range layerFolders[:len(layerFolders)-1] {
		var err error
		if uvm.OS() == "windows" {
			err = uvm.AddVSMB(layerPath, "", schema2.VsmbFlagReadOnly|schema2.VsmbFlagPseudoOplocks|schema2.VsmbFlagTakeBackupPrivilege|schema2.VsmbFlagCacheIO|schema2.VsmbFlagShareRead)
			if err == nil {
				vsmbAdded = append(vsmbAdded, layerPath)
			}
		} else {
			uvmPath := ""
			_, uvmPath, err = uvm.AddVPMEM(filepath.Join(layerPath, "layer.vhd"), true) // UVM path is calculated. Will be /tmp/vN/
			if err == nil {
				vpmemAdded = append(vpmemAdded,
					vpMemEntry{
						hostPath: filepath.Join(layerPath, "layer.vhd"),
						uvmPath:  uvmPath,
					})
			}
		}
		if err != nil {
			cleanupOnMountFailure(uvm, vsmbAdded, vpmemAdded, attachedSCSIHostPath)
			return nil, err
		}
	}

	// Add the scratch at an unused SCSI location. The container path inside the
	// utility VM will be C:\<ID>.
	hostPath := filepath.Join(layerFolders[len(layerFolders)-1], "sandbox.vhdx")

	// On Linux, we need to grant access to the scratch
	if uvm.OS() == "linux" {
		if err := wclayer.GrantVmAccess(uvm.ID(), hostPath); err != nil {
			cleanupOnMountFailure(uvm, vsmbAdded, vpmemAdded, attachedSCSIHostPath)
			return nil, err
		}
	}

	// BUGBUG Rename guestRoot better.
	containerScratchPathInUVM := ospath.Join(uvm.OS(), guestRoot, scratchPath)
	_, _, err := uvm.AddSCSI(hostPath, containerScratchPathInUVM)
	if err != nil {
		cleanupOnMountFailure(uvm, vsmbAdded, vpmemAdded, attachedSCSIHostPath)
		return nil, err
	}
	attachedSCSIHostPath = hostPath

	if uvm.OS() == "windows" {
		// 	Load the filter at the C:\s<ID> location calculated above. We pass into this request each of the
		// 	read-only layer folders.
		layers, err := computeV2Layers(uvm, vsmbAdded)
		if err != nil {
			cleanupOnMountFailure(uvm, vsmbAdded, vpmemAdded, attachedSCSIHostPath)
			return nil, err
		}
		hostedSettings := schema2.CombinedLayersV2{
			ContainerRootPath: containerScratchPathInUVM,
			Layers:            layers,
		}
		combinedLayersModification := &schema2.ModifySettingsRequestV2{
			ResourceType:   schema2.ResourceTypeCombinedLayers,
			RequestType:    schema2.RequestTypeAdd,
			HostedSettings: hostedSettings,
		}
		if err := uvm.Modify(combinedLayersModification); err != nil {
			cleanupOnMountFailure(uvm, vsmbAdded, vpmemAdded, attachedSCSIHostPath)
			return nil, err
		}
		logrus.Debugln("hcsshim::mountContainerLayers Succeeded")
		return hostedSettings, nil
	}

	// This is the LCOW layout inside the utilityVM. NNN is the container "number"
	// which increments for each container created in a utility VM.
	//
	// /run/gcs/c/NNN/config.json
	// /run/gcs/c/NNN/rootfs
	// /run/gcs/c/NNN/scratch/upper
	// /run/gcs/c/NNN/scratch/work
	//
	// /dev/sda on /tmp/scratch type ext4 (rw,relatime,block_validity,delalloc,barrier,user_xattr,acl)
	// /dev/pmem0 on /tmp/v0 type ext4 (ro,relatime,block_validity,delalloc,norecovery,barrier,dax,user_xattr,acl)
	// /dev/sdb on /run/gcs/c/NNN/scratch type ext4 (rw,relatime,block_validity,delalloc,barrier,user_xattr,acl)
	// overlay on /run/gcs/c/NNN/rootfs type overlay (rw,relatime,lowerdir=/tmp/v0,upperdir=/run/gcs/c/NNN/scratch/upper,workdir=/run/gcs/c/NNN/scratch/work)
	//
	// Where /dev/sda      is the scratch for utility VM itself
	//       /dev/pmemX    are read-only layers for containers
	//       /dev/sd(b...) are scratch spaces for each container

	layers := []schema2.ContainersResourcesLayerV2{}
	for _, vpmem := range vpmemAdded {
		layers = append(layers, schema2.ContainersResourcesLayerV2{Path: vpmem.uvmPath})
	}
	hostedSettings := schema2.CombinedLayersV2{
		ContainerRootPath: path.Join(guestRoot, rootfsPath),
		Layers:            layers,
		ScratchPath:       containerScratchPathInUVM,
	}
	combinedLayersModification := &schema2.ModifySettingsRequestV2{
		ResourceType:   schema2.ResourceTypeCombinedLayers,
		RequestType:    schema2.RequestTypeAdd,
		HostedSettings: hostedSettings,
	}
	if err := uvm.Modify(combinedLayersModification); err != nil {
		cleanupOnMountFailure(uvm, vsmbAdded, vpmemAdded, attachedSCSIHostPath)
		return nil, err
	}
	logrus.Debugln("hcsshim::mountContainerLayers Succeeded")
	return hostedSettings, nil

}

// unmountOperation is used when calling Unmount() to determine what type of unmount is
// required. In V1 schema, this must be unmountOperationAll. In V2, client can
// be more optimal and only unmount what they need which can be a minor performance
// improvement (eg if you know only one container is running in a utility VM, and
// the UVM is about to be torn down, there's no need to unmount the VSMB shares,
// just SCSI to have a consistent file system).
type unmountOperation uint

const (
	unmountOperationSCSI  unmountOperation = 0x01
	unmountOperationVSMB                   = 0x02
	unmountOperationVPMEM                  = 0x04
	unmountOperationAll                    = unmountOperationSCSI | unmountOperationVSMB | unmountOperationVPMEM
)

// unmountContainerLayers is a helper for clients to hide all the complexity of layer unmounting
func unmountContainerLayers(layerFolders []string, guestRoot string, uvm *uvm.UtilityVM, op unmountOperation) error {
	logrus.Debugln("hcsshim::unmountContainerLayers", layerFolders)
	if uvm == nil {
		// Must be an argon - folders are mounted on the host
		if op != unmountOperationAll {
			return fmt.Errorf("only operation supported for host-mounted folders is unmountOperationAll")
		}
		if len(layerFolders) < 1 {
			return fmt.Errorf("need at least one layer for Unmount")
		}
		path := layerFolders[len(layerFolders)-1]
		logrus.Debugln("hcsshim::Unmount UnprepareLayer", path)
		if err := wclayer.UnprepareLayer(path); err != nil {
			return err
		}
		// TODO Should we try this anyway?
		logrus.Debugln("hcsshim::unmountContainerLayers DeactivateLayer", path)
		return wclayer.DeactivateLayer(path)
	}

	// V2 Xenon

	// Base+Scratch as a minimum. This is different to v1 which only requires the scratch
	if len(layerFolders) < 2 {
		return fmt.Errorf("at least two layers are required for unmount")
	}

	var retError error

	// Unload the storage filter followed by the SCSI scratch
	if (op & unmountOperationSCSI) == unmountOperationSCSI {
		containerScratchPathInUVM := ospath.Join(uvm.OS(), guestRoot, scratchPath)
		logrus.Debugf("hcsshim::unmountContainerLayers CombinedLayers %s", containerScratchPathInUVM)
		combinedLayersModification := &schema2.ModifySettingsRequestV2{
			ResourceType:   schema2.ResourceTypeCombinedLayers,
			RequestType:    schema2.RequestTypeRemove,
			HostedSettings: schema2.CombinedLayersV2{ContainerRootPath: containerScratchPathInUVM},
		}
		if err := uvm.Modify(combinedLayersModification); err != nil {
			logrus.Errorf(err.Error())
		}

		// Hot remove the scratch from the SCSI controller
		hostScratchFile := filepath.Join(layerFolders[len(layerFolders)-1], "sandbox.vhdx")
		logrus.Debugf("hcsshim::unmountContainerLayers SCSI %s %s", containerScratchPathInUVM, hostScratchFile)
		if err := uvm.RemoveSCSI(hostScratchFile); err != nil {
			e := fmt.Errorf("failed to remove SCSI %s: %s", hostScratchFile, err)
			logrus.Debugln(e)
			if retError == nil {
				retError = e
			} else {
				retError = errors.Wrapf(retError, e.Error())
			}
		}
	}

	// Remove each of the read-only layers from VSMB. These's are ref-counted and
	// only removed once the count drops to zero. This allows multiple containers
	// to share layers.
	if uvm.OS() == "windows" && len(layerFolders) > 1 && (op&unmountOperationVSMB) == unmountOperationVSMB {
		for _, layerPath := range layerFolders[:len(layerFolders)-1] {
			if e := uvm.RemoveVSMB(layerPath); e != nil {
				logrus.Debugln(e)
				if retError == nil {
					retError = e
				} else {
					retError = errors.Wrapf(retError, e.Error())
				}
			}
		}
	}

	// Remove each of the read-only layers from VPMEM. These's are ref-counted and
	// only removed once the count drops to zero. This allows multiple containers
	// to share layers.
	if uvm.OS() == "linux" && len(layerFolders) > 1 && (op&unmountOperationVPMEM) == unmountOperationVPMEM {
		for _, layerPath := range layerFolders[:len(layerFolders)-1] {
			if e := uvm.RemoveVPMEM(filepath.Join(layerPath, "layer.vhd")); e != nil {
				logrus.Debugln(e)
				if retError == nil {
					retError = e
				} else {
					retError = errors.Wrapf(retError, e.Error())
				}
			}
		}
	}

	// TODO (possibly) Consider deleting the container directory in the utility VM

	return retError
}

func cleanupOnMountFailure(uvm *uvm.UtilityVM, vsmbShares []string, vpmemDevices []vpMemEntry, scsiHostPath string) {
	for _, vsmbShare := range vsmbShares {
		if err := uvm.RemoveVSMB(vsmbShare); err != nil {
			logrus.Warnf("Possibly leaked vsmbshare on error removal path: %s", err)
		}
	}
	for _, vpmemDevice := range vpmemDevices {
		if err := uvm.RemoveVPMEM(vpmemDevice.hostPath); err != nil {
			logrus.Warnf("Possibly leaked vpmemdevice on error removal path: %s", err)
		}
	}
	if scsiHostPath != "" {
		if err := uvm.RemoveSCSI(scsiHostPath); err != nil {
			logrus.Warnf("Possibly leaked SCSI disk on error removal path: %s", err)
		}
	}
}

func computeV2Layers(vm *uvm.UtilityVM, paths []string) (layers []schema2.ContainersResourcesLayerV2, err error) {
	for _, path := range paths {
		uvmPath, err := vm.GetVSMBUvmPath(path)
		if err != nil {
			return nil, err
		}
		layerID, err := wclayer.LayerID(path)
		if err != nil {
			return nil, err
		}
		layers = append(layers, schema2.ContainersResourcesLayerV2{
			Id:   layerID.String(),
			Path: uvmPath,
		})
	}
	return layers, nil
}
