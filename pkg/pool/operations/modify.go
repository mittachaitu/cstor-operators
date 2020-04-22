/*
Copyright 2019 The OpenEBS Authors.

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

package v1alpha2

import (
	"os"

	cstor "github.com/openebs/api/pkg/apis/cstor/v1"
	"github.com/openebs/api/pkg/apis/types"
	"github.com/openebs/api/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog"
)

const (
	// DeviceTypeSpare .. spare device type
	DeviceTypeSpare = "spare"
	// DeviceTypeReadCache .. read cache device type
	DeviceTypeReadCache = "cache"
	// DeviceTypeWriteCache .. write cache device type
	DeviceTypeWriteCache = "log"
	// DeviceTypeData .. data disk device type
	DeviceTypeData = "data"
)

//TODO: Get better naming conventions
type raidConfiguration struct {
	RaidGroupType string
	RaidGroups    []cstor.RaidGroup
}

// getRaidGroupsConfiguration returns map of DeviceType and raidConfiguration
func getRaidGroupsConfiguration(cspi *cstor.CStorPoolInstance) map[string]raidConfiguration {
	raidGroupsMap := map[string]raidConfiguration{}
	raidGroupsMap[DeviceTypeData] = raidConfiguration{
		RaidGroups:    cspi.Spec.DataRaidGroups,
		RaidGroupType: cspi.Spec.PoolConfig.DataRaidGroupType,
	}
	raidGroupsMap[DeviceTypeWriteCache] = raidConfiguration{
		RaidGroups:    cspi.Spec.WriteCacheRaidGroups,
		RaidGroupType: cspi.Spec.PoolConfig.WriteCacheGroupType,
	}
	return raidGroupsMap
}

// Update will update the deployed pool according to given cspi object
// NOTE: Update returns both CSPI as well as error
func (oc *OperationsConfig) Update(cspi *cstor.CStorPoolInstance) (*cstor.CStorPoolInstance, error) {
	var isObjChanged, isRaidGroupChanged bool

	bdClaimList, err := oc.getBlockDeviceClaimList(
		types.CStorPoolClusterLabelKey,
		cspi.GetLabels()[types.CStorPoolClusterLabelKey])
	if err != nil {
		return cspi, err
	}

	raidGroupConfigMap := getRaidGroupsConfiguration(cspi)

	for _, raidGroupsConfig := range raidGroupConfigMap {
		// first we will check if there any bdev is replaced or removed
		for raidIndex := 0; raidIndex < len(raidGroupsConfig.RaidGroups); raidIndex++ {
			isRaidGroupChanged = false
			raidGroup := raidGroupsConfig.RaidGroups[raidIndex]

			for bdevIndex := 0; bdevIndex < len(raidGroup.CStorPoolInstanceBlockDevices); bdevIndex++ {
				bdev := raidGroup.CStorPoolInstanceBlockDevices[bdevIndex]

				bdClaim, er := bdClaimList.GetBlockDeviceClaimFromBDName(
					bdev.BlockDeviceName)
				if er != nil {
					// This case is not possible
					err = ErrorWrapf(err,
						"Failed to get claim of blockdevice {%s}.. %s",
						bdev.BlockDeviceName,
						er.Error())
					// If claim doesn't exist for current blockdevice continue with
					// other blockdevices in cspi
					continue
				}

				// If current blockdevice is replaced blockdevice then get the
				// predecessor from claim of current blockdevice and if current
				// blockdevice is not replaced then predecessorBDName will be empty
				predecessorBDName := bdClaim.GetAnnotations()[types.PredecessorBDLabelKey]
				oldPath := []string{}
				if predecessorBDName != "" {
					// Get device links from old block device
					oldPath, er = oc.getPathForBDev(predecessorBDName)
					if er != nil {
						err = ErrorWrapf(err, "Failed to check bdev change {%s}.. %s", bdev.BlockDeviceName, er.Error())
						continue
					}
				}

				diskPath := ""
				var diskCapacityInBytes uint64
				// Let's check if any replacement is needed for this BDev
				newPath, er := oc.getPathForBDev(bdev.BlockDeviceName)
				if er != nil {
					err = ErrorWrapf(err, "Failed to check bdev change {%s}.. %s", bdev.BlockDeviceName, er.Error())
				} else {
					if diskPath, diskCapacityInBytes, er = replacePoolVdev(cspi, oldPath, newPath); er != nil {
						err = ErrorWrapf(err, "Failed to replace bdev for {%s}.. %s", bdev.BlockDeviceName, er.Error())
						continue
					} else {
						if !IsEmpty(diskPath) && (diskPath != bdev.DevLink || diskCapacityInBytes != bdev.Capacity) {
							// Here We are updating in underlying slice so no problem
							// Let's update devLink with new path for this bdev
							raidGroup.CStorPoolInstanceBlockDevices[bdevIndex].DevLink = diskPath
							raidGroup.CStorPoolInstanceBlockDevices[bdevIndex].Capacity = diskCapacityInBytes
							isRaidGroupChanged = true
						}
					}
				}
				// Only To Generate an BlockDevice Replacement event
				if len(oldPath) != 0 && len(newPath) != 0 {
					oc.recorder.Eventf(cspi,
						corev1.EventTypeNormal,
						"BlockDevice Replacement",
						"Replacement of %s BlockDevice with %s BlockDevice is in-Progress",
						predecessorBDName,
						bdev.BlockDeviceName,
					)
				}

				// If disk got replaced check resilvering status.
				// 1. If resilvering is in progress don't do any thing.
				// 2. If resilvering is completed then perform cleanup process
				//   2.1 Unclaim the old blockdevice which was used by pool
				//   2.2 Remove the annotation from blockdeviceclaim which is
				//       inuse by cstor pool
				if predecessorBDName != "" && !isResilveringInProgress(executeZpoolDump, cspi, diskPath) {
					oldBDClaim, _ := bdClaimList.GetBlockDeviceClaimFromBDName(
						predecessorBDName)
					if er := oc.cleanUpReplacementMarks(oldBDClaim, bdClaim); er != nil {
						err = ErrorWrapf(
							err,
							"Failed cleanup replacement marks of replaced blockdevice {%s}.. %s",
							bdev.BlockDeviceName,
							er.Error(),
						)
					} else {
						oc.recorder.Eventf(cspi,
							corev1.EventTypeNormal,
							"BlockDevice Replacement",
							"Resilvering is successfull on BlockDevice %s",
							bdev.BlockDeviceName,
						)
					}
				}
			}
			// If raidGroup is changed then update the cspi.spec.raidgroup entry
			// If raidGroup doesn't have any blockdevice then remove that raidGroup
			// and set isObjChanged
			if isRaidGroupChanged {
				//NOTE: Remove below code since we are not supporting removal of raid group/block device alone
				if len(raidGroup.CStorPoolInstanceBlockDevices) == 0 {
					cspi.Spec.DataRaidGroups = append(cspi.Spec.DataRaidGroups[:raidIndex], cspi.Spec.DataRaidGroups[raidIndex+1:]...)
					// We removed the raidIndex entry cspi.Spec.raidGroup
					raidIndex--
				}
				isObjChanged = true
			}
		}
	}

	//TODO revisit for day 2 ops
	if er := oc.addNewVdevFromCSP(cspi); er != nil {
		oc.recorder.Eventf(cspi,
			corev1.EventTypeWarning,
			"Pool Expansion",
			"Failed to expand pool... Error: %s", er.Error(),
		)
	}

	if isObjChanged {
		if ncspi, er := OpenEBSClient.
			CstorV1().
			CStorPoolInstances(cspi.Namespace).
			Update(cspi); er != nil {
			err = ErrorWrapf(err, "Failed to update object.. err {%s}", er.Error())
		} else {
			cspi = ncspi
		}
	}

	if ncspi, er := oc.ExpandPoolIfDiskExpanded(cspi); er == nil {
		cspi = ncspi
	}
	return cspi, err
}

// TODO: Combine ExpandPoolIfDiskExpanded func with Update func(So that
// we can reduce n/w calls)

// ExpandPoolIfDiskExpanded performs pool expansion when underlying disk got expanded
// currently it will identify by comparing the capacity of blockdevice and capacity exist
// on CStorPoolInstanceBlockDevice
func (oc *OperationsConfig) ExpandPoolIfDiskExpanded(
	cspi *cstor.CStorPoolInstance) (*cstor.CStorPoolInstance, error) {
	var err error
	var isPoolExpanded bool
	minimumMB := uint64(500 * 1024 * 1024)
	openebsNamespace := os.Getenv(string(util.Namespace))
	deviceTypeAndRaidConfigurationMap := getRaidGroupsConfiguration(cspi)

	for _, raidGroupConfig := range deviceTypeAndRaidConfigurationMap {
		for raidIndex := 0; raidIndex < len(raidGroupConfig.RaidGroups); raidIndex++ {
			raidGroup := raidGroupConfig.RaidGroups[raidIndex]
			for _, cspiBlockDevice := range raidGroup.CStorPoolInstanceBlockDevices {
				if cspiBlockDevice.Capacity > uint64(0) && cspiBlockDevice.DevLink != "" {
					bdObj, er := oc.getBlockDevice(cspiBlockDevice.BlockDeviceName, openebsNamespace)
					if er != nil {
						err = ErrorWrapf(err,
							"Failed to get blockdevice %s .. err {%s}", cspiBlockDevice.BlockDeviceName, er.Error())
						continue
					}
					if (bdObj.Spec.Capacity.Storage - minimumMB) > cspiBlockDevice.Capacity {
						er = oc.expandPool(cspiBlockDevice.DevLink, cspiBlockDevice.Capacity)
						if er != nil {
							err = ErrorWrapf(err, "Failed to expand disk %s in pool", cspiBlockDevice.DevLink)
							continue
						}
						klog.Infof("Successfully expanded the blockdevice %s in CSPI %s", cspiBlockDevice.BlockDeviceName, cspi.Name)
						isPoolExpanded = true
					}
				}
			}
		}
	}

	if err != nil {
		oc.recorder.Eventf(cspi,
			corev1.EventTypeWarning,
			"Pool Expansion",
			"Failed to expand pool when underlying disk expanded err {%s}", err.Error(),
		)
	} else if isPoolExpanded {
		oc.recorder.Event(cspi,
			corev1.EventTypeNormal,
			"Pool Expansion",
			"Expanded the disks which were in use by pool",
		)
	}
	return cspi, nil
}
