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
	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/pool"
)

// ContainerCPUAssignments type used in cpu manger state
type ContainerCPUAssignments map[string]cpuset.CPUSet

// Clone returns a copy of ContainerCPUAssignments
func (as ContainerCPUAssignments) Clone() ContainerCPUAssignments {
	ret := make(ContainerCPUAssignments)
	for key, val := range as {
		ret[key] = val
	}
	return ret
}

// Reader interface used to read current cpu/pod assignment state
type Reader interface {
	GetCPUSet(containerID string) (cpuset.CPUSet, bool)
	GetDefaultCPUSet() cpuset.CPUSet
	GetCPUSetOrDefault(containerID string) cpuset.CPUSet
	GetCPUAssignments() ContainerCPUAssignments
	GetPoolCPUs() map[string]cpuset.CPUSet
	GetPoolAssignments() map[string]cpuset.CPUSet
	GetPoolCapacity() v1.ResourceList
}

type writer interface {
	SetCPUSet(containerID string, cpuset cpuset.CPUSet)
	SetDefaultCPUSet(cpuset cpuset.CPUSet)
	SetCPUAssignments(ContainerCPUAssignments)
	Delete(containerID string)
	ClearState()
	SetAllocator(allocfn pool.AllocCpuFunc, t *topology.CPUTopology)
	Reconfigure(cfg pool.Config) error
	AllocateCPUs(containerID string, pool string, numCPUs int) (cpuset.CPUSet, error)
	AllocateCPU(containerID string, pool string, milliCPU int64) (cpuset.CPUSet, error)
	ReleaseCPU(containerID string)
}

// State interface provides methods for tracking and setting cpu/pod assignment
type State interface {
	Reader
	writer
}
