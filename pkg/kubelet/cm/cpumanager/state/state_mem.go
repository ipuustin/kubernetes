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

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

type stateMemory struct {
	sync.RWMutex
	assignments   ContainerCPUAssignments
	pools         map[string]cpuset.CPUSet
}

var _ State = &stateMemory{}

// NewMemoryState creates new State for keeping track of cpu/pod assignment
func NewMemoryState() State {
	glog.Infof("[cpumanager] initializing new in-memory state store")
	return &stateMemory{
		assignments:   ContainerCPUAssignments{},
		pools:         make(map[string]cpuset.CPUSet),
	}
}

func (s *stateMemory) GetCPUSet(containerID string) (cpuset.CPUSet, bool) {
	s.RLock()
	defer s.RUnlock()

	res, ok := s.assignments[containerID]
	return res.Clone(), ok
}

func (s *stateMemory) GetPoolCPUSet(pool string) (cpuset.CPUSet, bool) {
	s.RLock()
	defer s.RUnlock()

	if cset, ok := s.pools[pool]; !ok {
		return cpuset.NewCPUSet(), false
	} else {
		return cset.Clone(), true
	}
}

func (s *stateMemory) GetDefaultCPUSet() cpuset.CPUSet {
	cset, _ := s.GetPoolCPUSet("default")

	return cset
}

func (s *stateMemory) GetCPUSetOrDefault(containerID string) cpuset.CPUSet {
	if res, ok := s.GetCPUSet(containerID); ok {
		return res
	}
	return s.GetDefaultCPUSet()
}

func (s *stateMemory) GetCPUAssignments() ContainerCPUAssignments {
	s.RLock()
	defer s.RUnlock()
	return s.assignments.Clone()
}

func (s *stateMemory) GetCPUPools() map[string]cpuset.CPUSet {
	s.RLock()
	defer s.RUnlock()

	pools := make(map[string]cpuset.CPUSet)
	for poolName, cset := range s.pools {
		pools[poolName] = cset.Clone()
	}

	return pools
}

func (s *stateMemory) SetCPUSet(containerID string, cset cpuset.CPUSet) {
	s.Lock()
	defer s.Unlock()

	s.assignments[containerID] = cset
	glog.Infof("[cpumanager] updated desired cpuset (container id: %s, cpuset: \"%s\")", containerID, cset)
}

func (s *stateMemory) SetPoolCPUSet(pool string, cset cpuset.CPUSet) {
	s.Lock()
	defer s.Unlock()

	s.pools[pool] = cset
	glog.Infof("[cpumanager] updated \"%s\" cpuset: \"%s\"", pool, cset)
}

func (s *stateMemory) SetDefaultCPUSet(cset cpuset.CPUSet) {
	s.SetPoolCPUSet("default", cset)
}

func (s *stateMemory) SetCPUAssignments(a ContainerCPUAssignments) {
	s.Lock()
	defer s.Unlock()

	s.assignments = a.Clone()
	glog.Infof("[cpumanager] updated cpuset assignments: \"%v\"", a)
}

func (s *stateMemory) Delete(containerID string) {
	s.Lock()
	defer s.Unlock()

	delete(s.assignments, containerID)
	glog.V(2).Infof("[cpumanager] deleted cpuset assignment (container id: %s)", containerID)
}

func (s *stateMemory) ClearState() {
	s.Lock()
	defer s.Unlock()

	s.assignments = make(ContainerCPUAssignments)
	s.pools = make(map[string]cpuset.CPUSet)
	glog.V(2).Infof("[cpumanager] cleared state")
}
