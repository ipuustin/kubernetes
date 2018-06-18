// Copyright 2018 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stub

import (
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"

	api "k8s.io/kubernetes/pkg/kubelet/apis/cpuplugin/v1alpha"
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

// ContainerMemoryAssignments type used in cpu manager state
type ContainerMemoryAssignments map[string]int64

// ContainerCacheAssignments type used in cpu manager state
type ContainerCacheAssignments map[string]string

type State interface {
	GetCPUSet(containerID string) (cpuset.CPUSet, bool)
	GetDefaultCPUSet() cpuset.CPUSet
	GetCPUAssignments() ContainerCPUAssignments
	GetMemory(containerID string) (int64, bool)
	GetMemoryAssignments() ContainerMemoryAssignments
	GetCache(containerID string) (string, bool)
	GetCacheAssignments() ContainerCacheAssignments
	GetPolicyData() map[string]string
	GetPolicyEntry(string) (string, bool)
	SetCPUSet(containerID string, cset cpuset.CPUSet)
	SetDefaultCPUSet(cset cpuset.CPUSet)
	SetMemory(containerID string, mem int64)
	SetCache(containerID string, cache string)
	SetPolicyData(map[string]string)
	SetPolicyEntry(string, string)
	Delete(containerID string)
	DeclareResource(name v1.ResourceName, qty resource.Quantity)
	UpdateResource(name v1.ResourceName, qty resource.Quantity)
}

type stubState struct {
	assignments       ContainerCPUAssignments  // non-default container CPU assignments
	updatedContainers map[string]bool          // updated containers
	defaultCPUSet     cpuset.CPUSet            // the default CPU set
	resources         v1.ResourceList          // resource declarations
	updatedResources  map[v1.ResourceName]bool // updated resource names
	pluginState       map[string]string        // updated plugin-specific state
}

func newStubState(in *api.State, vendor string) stubState {
	s := CoreState(in)
	s.resources = v1.ResourceList{}
	if in.PluginState != nil {
		s.pluginState = in.PluginState
	} else {
		s.pluginState = make(map[string]string)
	}
	s.Reset()

	return s
}

func (s *stubState) Reset() {
	s.updatedContainers = make(map[string]bool)
	s.updatedResources = make(map[v1.ResourceName]bool)
}

func (s *stubState) GetCPUSet(containerID string) (cpuset.CPUSet, bool) {
	cset, ok := s.assignments[containerID]
	return cset.Clone(), ok
}

func (s *stubState) GetDefaultCPUSet() cpuset.CPUSet {
	return s.defaultCPUSet.Clone()
}

func (s *stubState) GetCPUAssignments() ContainerCPUAssignments {
	return s.assignments.Clone()
}

func (s *stubState) GetMemory(containerID string) (int64, bool) {
	return 0, false
}

func (s *stubState) GetMemoryAssignments() ContainerMemoryAssignments {
	return nil
}

func (s *stubState) GetCache(containerID string) (string, bool) {
	return "", false
}

func (s *stubState) GetCacheAssignments() ContainerCacheAssignments {
	return nil
}

func (s *stubState) GetPolicyData() map[string]string {
	return s.pluginState
}

func (s *stubState) GetPolicyEntry(key string) (string, bool) {
	value, ok := s.pluginState[key]
	return value, ok
}

func (s *stubState) SetCPUSet(containerID string, cset cpuset.CPUSet) {
	if old, ok := s.assignments[containerID]; ok {
		if cset.Equals(old) {
			return
		}
	}

        s.assignments[containerID] = cset
	s.updatedContainers[containerID] = true
}

func (s *stubState) SetDefaultCPUSet(cset cpuset.CPUSet) {
	s.defaultCPUSet = cset
}

func (s *stubState) Delete(containerID string) {
	if _, ok := s.assignments[containerID]; !ok {
		return
	}

	delete(s.assignments, containerID)
	s.updatedContainers[containerID] = true
}

func (s *stubState) SetMemory(containerID string, mem int64) {
	return
}


func (s *stubState) SetCache(containerID string, cache string) {
	return
}

func (s *stubState) SetPolicyData(data map[string]string) {
	s.pluginState = data
}

func (s *stubState) SetPolicyEntry(key, value string) {
	s.pluginState[key] = value
}

func (s *stubState) DeclareResource(name v1.ResourceName, qty resource.Quantity) {
	s.resources[name] = qty
	s.updatedResources[name] = true
}

func (s *stubState) UpdateResource(name v1.ResourceName, qty resource.Quantity) {
	if old, ok := s.resources[name]; ok {
		if old.Cmp(qty) == 0 {
			return
		}
	}

	s.resources[name] = qty
	s.updatedResources[name] = true
}

func (s *stubState) ContainerChanges() map[string]*api.ContainerHint {
	hints := make(map[string]*api.ContainerHint)

	for id, _ := range s.updatedContainers {
		h := &api.ContainerHint{
			Id: id,
		}

		if cset, ok := s.assignments[id]; ok {
			h.Cpuset = cset.String()
		} else {
			h.Cpuset = "deleted"
		}

		hints[id] = h
	}

	return hints
}

func (s *stubState) ResourceChanges(declare bool) api.ResourceList {
	if declare {
		return StubResourceList(s.resources)
	} else {
		resources := api.ResourceList{}

		for id, _ := range s.updatedResources {
			qty := s.resources[id]
			resources[string(id)] = StubQuantity(qty)
		}

		return resources
	}
}

func (s *stubState) StateChanges() *api.State {
	return &api.State{
		DefaultCPUSet: s.defaultCPUSet.String(),
		PluginState: s.pluginState,
	}
}
