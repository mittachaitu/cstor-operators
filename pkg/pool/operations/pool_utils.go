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
	"strings"

	"github.com/openebs/api/pkg/apis/types"

	cstor "github.com/openebs/api/pkg/apis/cstor/v1"
	openebsapis "github.com/openebs/api/pkg/apis/openebs.io/v1alpha1"
	zpool "github.com/openebs/api/pkg/internalapis/apis/cstor"
	"github.com/openebs/api/pkg/util"
	"github.com/openebs/cstor-operators/pkg/pool"
	zfs "github.com/openebs/cstor-operators/pkg/zcmd"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
)

func (oc *OperationsConfig) getPathForBdevList(bdevs []cstor.CStorPoolClusterBlockDevice) (map[string][]string, error) {
	var err error

	vdev := make(map[string][]string, len(bdevs))
	for _, b := range bdevs {
		path, er := oc.getPathForBDev(b.BlockDeviceName)
		if er != nil || len(path) == 0 {
			err = ErrorWrapf(err, "Failed to fetch path for bdev {%s} {%s}", b.BlockDeviceName, er.Error())
			continue
		}
		vdev[b.BlockDeviceName] = path
	}
	return vdev, err
}

func (oc *OperationsConfig) getPathForBDev(bdev string) ([]string, error) {
	var path []string
	// TODO: replace `NAMESPACE` with env variable from CSPI deployment
	bd, err := oc.openebsclientset.
		OpenebsV1alpha1().
		BlockDevices(util.GetEnv(util.Namespace)).
		Get(bdev, metav1.GetOptions{})
	if err != nil {
		return path, err
	}
	return getPathForBDevFromBlockDevice(bd), nil
}

func getZFSDeviceType(dType string) string {
	if dType == DeviceTypeData {
		return ""
	}
	return dType
}

func getPathForBDevFromBlockDevice(bd *openebsapis.BlockDevice) []string {
	var paths []string
	if len(bd.Spec.DevLinks) != 0 {
		for _, v := range bd.Spec.DevLinks {
			paths = append(paths, v.Links...)
		}
	}

	if len(bd.Spec.Path) != 0 {
		paths = append(paths, bd.Spec.Path)
	}
	return paths
}

// checkIfPoolPresent returns true if pool is available for operations
func checkIfPoolPresent(name string) bool {
	if _, err := zfs.NewPoolGetProperty().
		WithParsableMode(true).
		WithScriptedMode(true).
		WithField("name").
		WithProperty("name").
		WithPool(name).
		Execute(); err != nil {
		return false
	}
	return true
}

/*
func isBdevPathChanged(bdev cstor.CStorPoolClusterBlockDevice) ([]string, bool, error) {
	var err error
	var isPathChanged bool

	newPath, er := getPathForBDev(bdev.BlockDeviceName)
	if er != nil {
		err = errors.Errorf("Failed to get bdev {%s} path err {%s}", bdev.BlockDeviceName, er.Error())
	}

	if err == nil && !util.ContainsString(newPath, bdev.DevLink) {
		isPathChanged = true
	}

	return newPath, isPathChanged, err
}
*/

func compareDisk(path []string, d []zpool.Vdev) (string, bool) {
	for _, v := range d {
		if util.ContainsString(path, v.Path) {
			return v.Path, true
		}
		for _, p := range v.Children {
			if util.ContainsString(path, p.Path) {
				return p.Path, true
			}
			if path, r := compareDisk(path, p.Children); r {
				return path, true
			}
		}
	}
	return "", false
}

func checkIfDeviceUsed(path []string, t zpool.Topology) (string, bool) {
	var isUsed bool
	var usedPath string

	if usedPath, isUsed = compareDisk(path, t.VdevTree.Topvdev); isUsed {
		return usedPath, isUsed
	}

	if usedPath, isUsed = compareDisk(path, t.VdevTree.Spares); isUsed {
		return usedPath, isUsed
	}

	if usedPath, isUsed = compareDisk(path, t.VdevTree.Readcache); isUsed {
		return usedPath, isUsed
	}
	return usedPath, isUsed
}

func (oc *OperationsConfig) checkIfPoolNotImported(cspi *cstor.CStorPoolInstance) (string, bool, error) {
	var cmdOut []byte
	var err error

	bdPath, err := oc.getPathForBDev(cspi.Spec.DataRaidGroups[0].BlockDevices[0].BlockDeviceName)
	if err != nil {
		return "", false, err
	}

	devID := pool.GetDevPathIfNotSlashDev(bdPath[0])
	if len(devID) != 0 {
		cmdOut, err = zfs.NewPoolImport().WithDirectory(devID).Execute()
		if strings.Contains(string(cmdOut), PoolName()) {
			return string(cmdOut), true, nil
		}
	}
	// there are some cases when import is succesful but zpool command return
	// noisy errors, hence better to check contains before return error
	cmdOut, err = zfs.NewPoolImport().Execute()
	if strings.Contains(string(cmdOut), PoolName()) {
		return string(cmdOut), true, nil
	}
	return string(cmdOut), false, err
}

// getBlockDeviceClaimList returns list of block device claims based on the
// label passed to the function
func (oc *OperationsConfig) getBlockDeviceClaimList(key, value string) (
	*openebsapis.BlockDeviceClaimList, error) {
	namespace := util.GetEnv(util.Namespace)
	bdcClient := oc.openebsclientset.OpenebsV1alpha1().BlockDeviceClaims(namespace)
	bdcAPIList, err := bdcClient.List(metav1.ListOptions{
		LabelSelector: key + "=" + value,
	})
	if err != nil {
		return nil, errors.Wrapf(err,
			"failed to list bdc related to key: %s value: %s",
			key,
			value,
		)
	}
	return bdcAPIList, nil
}

func executeZpoolDump(cspi *cstor.CStorPoolInstance) (zpool.Topology, error) {
	return zfs.NewPoolDump().
		WithPool(PoolName()).
		WithStripVdevPath().
		Execute()
}

// isResilveringInProgress returns true if resilvering is inprogress at cstor
// pool
func isResilveringInProgress(
	executeCommand func(cspi *cstor.CStorPoolInstance) (zpool.Topology, error),
	cspi *cstor.CStorPoolInstance,
	path string) bool {
	poolTopology, err := executeCommand(cspi)
	if err != nil {
		// log error
		klog.Errorf("Failed to get pool topology error: %v", err)
		return true
	}
	vdev, isVdevExist := getVdevFromPath(path, poolTopology)
	if !isVdevExist {
		return true
	}
	// If device in raid group didn't got replaced then there won't be any info
	// related to scan stats
	if len(vdev.ScanStats) == 0 {
		return false
	}
	// If device didn't underwent resilvering then no.of scaned bytes will be
	// zero
	if vdev.VdevStats[zpool.VdevScanProcessedIndex] == 0 {
		return false
	}
	// To decide whether resilvering is completed then check following steps
	// 1. Current device should be child device.
	// 2. Device Scan State should be completed
	if len(vdev.Children) == 0 &&
		vdev.ScanStats[zpool.VdevScanStatsStateIndex] == uint64(zpool.PoolScanFinished) &&
		vdev.ScanStats[zpool.VdevScanStatsScanFuncIndex] == uint64(zpool.PoolScanFuncResilver) {
		return false
	}
	return true
}

func getVdevFromPath(path string, topology zpool.Topology) (zpool.Vdev, bool) {
	var vdev zpool.Vdev
	var isVdevExist bool

	if vdev, isVdevExist = zpool.
		VdevList(topology.VdevTree.Topvdev).
		GetVdevFromPath(path); isVdevExist {
		return vdev, isVdevExist
	}

	if vdev, isVdevExist = zpool.
		VdevList(topology.VdevTree.Spares).
		GetVdevFromPath(path); isVdevExist {
		return vdev, isVdevExist
	}

	if vdev, isVdevExist = zpool.
		VdevList(topology.VdevTree.Readcache).
		GetVdevFromPath(path); isVdevExist {
		return vdev, isVdevExist
	}
	return vdev, isVdevExist
}

//cleanUpReplacementMarks should be called only after resilvering is completed.
//It does the following work
// 1. RemoveFinalizer on old block device claim exists and delete the old block
//   device claim.
// 2. Remove link of old block device in new block device claim
// oldObj is block device claim of replaced block device object which is
// detached from pool
// newObj is block device claim of current block device object which is in use
// by pool
func (oc *OperationsConfig) cleanUpReplacementMarks(oldObj, newObj *openebsapis.BlockDeviceClaim) error {
	if oldObj != nil {
		oldObj.RemoveFinalizer(types.CSPCFinalizer)
		err := oc.openebsclientset.OpenebsV1alpha1().BlockDeviceClaims(newObj.Namespace).Delete(oldObj.Name, &metav1.DeleteOptions{})
		if err != nil {
			return errors.Wrapf(
				err,
				"Failed to unclaim old blockdevice {%s}",
				oldObj.Spec.BlockDeviceName,
			)
		}
	}
	bdAnnotations := newObj.GetAnnotations()
	delete(bdAnnotations, types.PredecessorBDLabelKey)
	newObj.SetAnnotations(bdAnnotations)
	_, err := oc.openebsclientset.OpenebsV1alpha1().BlockDeviceClaims(newObj.Namespace).Update(newObj)
	if err != nil {
		return errors.Wrapf(
			err,
			"Failed to remove annotation {%s} from blockdeviceclaim {%s}",
			types.PredecessorBDLabelKey,
			newObj.Name,
		)
	}
	return nil
}
