/*
Copyright 2017 The Kubernetes Authors.

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

package state

import (
	"sync"
	"fmt"
	"os"
	"io/ioutil"
	"encoding/json"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/pool"
	"k8s.io/api/core/v1"
)

type stateFileData struct {
	PolicyName    string             `json:"policyName"`
	Pools        *pool.PoolSet       `json:"pools"`
}

var _ State = &stateFile{}

type stateFile struct {
	sync.RWMutex
	stateFilePath string
	policyName    string
	pools         pool.PoolSet
}

// NewFileState creates new State for keeping track of cpu/pod assignment with file backend
func NewFileState(filePath string, policyName string) State {
	stateFile := &stateFile{
		stateFilePath: filePath,
		policyName:    policyName,
	}

	if pools, err := stateFile.tryRestoreState(); err != nil {
		// could not restore state, init new state file
		msg := fmt.Sprintf("[cpumanager] state file: unable to restore state from disk (%s)\n", err.Error()) +
			"Panicking because we cannot guarantee sane CPU affinity for existing containers.\n" +
			fmt.Sprintf("Please drain this node and delete the CPU manager state file \"%s\" before restarting Kubelet.", stateFile.stateFilePath)
		panic(msg)
	} else {
		if pools == nil {
			pools, _ = pool.NewPoolSet(nil)
			stateFile.pools = *pools
			stateFile.storeState()
			glog.Infof("[cpumanager] state file: created new state file \"%s\"", stateFile.stateFilePath)
		} else {
			stateFile.pools = *pools
		}
	}

	glog.Infof("[cpumanager] state file path: %s", filePath)

	return stateFile
}

// tryRestoreState tries to read state file, upon any error,
// err message is logged and state is left clean. un-initialized
func (sf *stateFile) tryRestoreState() (*pool.PoolSet, error) {
	sf.Lock()
	defer sf.Unlock()
	var err error
	var pools *pool.PoolSet

	var content []byte

	content, err = ioutil.ReadFile(sf.stateFilePath)

	// If the state file does not exist or has zero length, write a new file.
	if os.IsNotExist(err) || len(content) == 0 {
		return pool.NewPoolSet(nil)
	}

	// Fail on any other file read error.
	if err != nil {
		return nil, err
	}

	// File exists; try to read it.
	var readState stateFileData

	if err = json.Unmarshal(content, &readState); err != nil {
		glog.Errorf("[cpumanager] state file: could not unmarshal, corrupted state file - \"%s\"", sf.stateFilePath)
		return nil, err
	}

	if sf.policyName != readState.PolicyName {
		return nil, fmt.Errorf("policy configured \"%s\" != policy from state file \"%s\"", sf.policyName, readState.PolicyName)
	}

	pools = readState.Pools

	glog.V(2).Infof("[cpumanager] state file: restored state from state file \"%s\"", sf.stateFilePath)

	return pools, nil
}

// saves state to a file, caller is responsible for locking
func (sf *stateFile) storeState() {
	var content []byte
	var err error

	data := stateFileData{
		PolicyName:    sf.policyName,
		Pools:        &sf.pools,
	}

	if content, err = json.Marshal(data); err != nil {
		panic("[cpumanager] state file: could not serialize state to json")
	}

	if err = ioutil.WriteFile(sf.stateFilePath, content, 0644); err != nil {
		panic("[cpumanager] state file not written")
	}
}

func (sf *stateFile) GetCPUSet(containerID string) (cpuset.CPUSet, bool) {
	sf.RLock()
	defer sf.RUnlock()

	return sf.pools.GetContainerCPUSet(containerID)
}

func (sf *stateFile) GetDefaultCPUSet() cpuset.CPUSet {
	sf.RLock()
	defer sf.RUnlock()

	cset, _ := sf.pools.GetPoolCPUSet(pool.DefaultPool)

	return cset
}

func (sf *stateFile) GetCPUSetOrDefault(containerID string) cpuset.CPUSet {
	if cset, ok := sf.GetCPUSet(containerID); ok {
		return cset
	}

	return sf.GetDefaultCPUSet()
}

func (sf *stateFile) GetCPUAssignments() ContainerCPUAssignments {
	sf.RLock()
	defer sf.RUnlock()
	return sf.pools.GetCPUAssignments()
}

func (sf *stateFile) GetPoolCPUs() map[string]cpuset.CPUSet {
	sf.RLock()
	defer sf.RUnlock()
	return sf.pools.GetPoolCPUs()
}

func (sf *stateFile) GetPoolAssignments() map[string]cpuset.CPUSet {
	sf.RLock()
	defer sf.RUnlock()
	return sf.pools.GetPoolAssignments()
}

func (sf *stateFile) SetCPUSet(containerID string, cset cpuset.CPUSet) {
	glog.Errorf("[cpumanager] deprecated function State.SetCPUSet called")
}

func (sf *stateFile) SetDefaultCPUSet(cset cpuset.CPUSet) {
	glog.Errorf("[cpumanager] deprecated function State.SetDefaultCPUSet called")
}

func (sf *stateFile) SetCPUAssignments(a ContainerCPUAssignments) {
	glog.Errorf("[cpumanager] deprecated function State.SetCPUAssignments called")
}

func (sf *stateFile) Delete(containerID string) {
	glog.Errorf("[cpumanager] deprecated function State.Delete called")
}

func (sf *stateFile) ClearState() {
	sf.Lock()
	defer sf.Unlock()

	if pools, err := pool.NewPoolSet(nil); err != nil {
		glog.Warningf("[cpumanager] clearing state failed: %s", err.Error())
	} else {
		sf.pools = *pools
		glog.V(2).Infof("[cpumanager] cleared state")
	}
	sf.storeState()
}

func (sf *stateFile) SetAllocator(allocfn pool.AllocCpuFunc, t *topology.CPUTopology) {
	sf.pools.SetAllocator(allocfn, t)
}

func (sf *stateFile) Reconfigure(cfg pool.Config) error {
	sf.Lock()
	defer sf.Unlock()

	if err := sf.pools.Reconfigure(cfg); err != nil {
		return err
	}

	sf.storeState()

	return nil
}

func (sf *stateFile) AllocateCPUs(id string, pool string, numCPUs int) (cpuset.CPUSet, error) {
	sf.Lock()
	defer sf.Unlock()

	cset, err := sf.pools.AllocateCPUs(id, pool, numCPUs)
	sf.storeState()

	return cset, err
}

func (sf *stateFile) AllocateCPU(id string, pool string, milliCPU int64) (cpuset.CPUSet, error) {
	sf.Lock()
	defer sf.Unlock()

	cset, err := sf.pools.AllocateCPU(id, pool, milliCPU)
	sf.storeState()

	return cset, err
}

func (sf *stateFile) ReleaseCPU(id string) {
	sf.Lock()
	defer sf.Unlock()

	sf.pools.ReleaseCPU(id)
	sf.storeState()
}

func (sf *stateFile) GetPoolCapacity() v1.ResourceList {
	sf.RLock()
	defer sf.RUnlock()

	return sf.pools.GetPoolCapacity()
}
