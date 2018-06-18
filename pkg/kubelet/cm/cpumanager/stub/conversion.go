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
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/apimachinery/pkg/api/resource"

	api "k8s.io/kubernetes/pkg/kubelet/apis/cpuplugin/v1alpha"
)

//
// Conversion functions between core and relayed/plugin/stub types.
//

// CPUSet
func CoreCPUSet(in string) cpuset.CPUSet {
	return cpuset.MustParse(in)
}

func StubCPUSet(in cpuset.CPUSet) string {
	return in.String()
}

// Quantity
func CoreQuantity(in *api.Quantity) resource.Quantity {
	return resource.MustParse(in.Value)
}

func StubQuantity(in resource.Quantity) *api.Quantity {
	return &api.Quantity{
		Value: in.String(),
	}
}

// ResourceList
func CoreResourceList(in api.ResourceList) v1.ResourceList {
	out := v1.ResourceList{}

	for r, q := range in {
		out[v1.ResourceName(r)] = CoreQuantity(q)
	}

	return out
}

func StubResourceList(in v1.ResourceList) api.ResourceList {
	out := api.ResourceList{}

	for r, q := range in {
		out[string(r)] = StubQuantity(q)
	}

	return out
}

// ResourceRequirements
func CoreResourceRequirements(in *api.ResourceRequirements) v1.ResourceRequirements {
	out := v1.ResourceRequirements{}

	if in != nil {
		out.Limits = CoreResourceList(in.Limits)
		out.Requests = CoreResourceList(in.Requests)
	}

	return out
}

func StubResourceRequirements(in v1.ResourceRequirements) *api.ResourceRequirements {
	out := &api.ResourceRequirements{}

	out.Limits = StubResourceList(in.Limits)
	out.Requests = StubResourceList(in.Requests)

	return out
}

// PodSpec
func CorePodSpec(in *api.PodSpec) v1.PodSpec {
	return v1.PodSpec{
		SchedulerName: in.SchedulerName,
		PriorityClassName: in.PriorityClassName,
	}
}

func StubPodSpec(in v1.PodSpec) *api.PodSpec {
	return &api.PodSpec{
		SchedulerName: in.SchedulerName,
		PriorityClassName: in.PriorityClassName,
	}
}

// Pod
func CorePod(in *api.Pod) v1.Pod {
	return v1.Pod{
//		Name: in.Name,
//		Namespace: in.Namespace,
//		Labels: in.Labels,
		Spec: CorePodSpec(in.Spec),
	}
}

func StubPod(in v1.Pod) *api.Pod {
	return &api.Pod{
//		Name: in.Name,
//		Namespace: in.Namespace,
//		Labels: in.Labels,
		Spec: StubPodSpec(in.Spec),
	}
}

// Container
func CoreContainer(in *api.Container) v1.Container {
	return v1.Container{
		Name: in.Name,
		Resources: CoreResourceRequirements(in.Resources),
	}
}

func StubContainer(in v1.Container) *api.Container {
	return &api.Container{
		Name: in.Name,
		Resources: StubResourceRequirements(in.Resources),
	}
}

// CPUInfo
func CoreCPUInfo(in *api.CPUInfo) topology.CPUInfo {
	return topology.CPUInfo{
		SocketID: int(in.SocketID),
		CoreID: int(in.CoreID),
	}
}

func StubCPUInfo(in topology.CPUInfo) *api.CPUInfo {
	return &api.CPUInfo{
		SocketID: int32(in.SocketID),
		CoreID: int32(in.CoreID),
	}
}

// CPUDetails
func CoreCPUDetails(in api.CPUDetails) topology.CPUDetails {
	out := topology.CPUDetails{}
	for id, info := range in {
		out[int(id)] = CoreCPUInfo(info)
	}
	return out
}

func StubCPUDetails(in topology.CPUDetails) api.CPUDetails {
	out := api.CPUDetails{}
	for id, info := range in {
		out[int32(id)] = StubCPUInfo(info)
	}
	return out
}

// CPUTopology
func CoreCPUTopology(in *api.CPUTopology) topology.CPUTopology {
	return topology.CPUTopology{
		NumCPUs: int(in.NumCPUs),
		NumCores: int(in.NumCores),
		NumSockets: int(in.NumSockets),
		CPUDetails: CoreCPUDetails(in.CPUDetails),
	}
}

func StubCPUTopology(in topology.CPUTopology) *api.CPUTopology {
	return &api.CPUTopology{
		NumCPUs: int32(in.NumCPUs),
		NumCores: int32(in.NumCores),
		NumSockets: int32(in.NumSockets),
		CPUDetails: StubCPUDetails(in.CPUDetails),
	}
}

// State
func CoreState(in *api.State) stubState {
	out := stubState{
		assignments:   make(map[string]cpuset.CPUSet),
		defaultCPUSet: cpuset.NewCPUSet(),
	}

	if in != nil {
		for id, cset := range in.Assignments {
			out.assignments[id] = cpuset.MustParse(cset)
		}
		out.defaultCPUSet = cpuset.MustParse(in.DefaultCPUSet)
	}

	return out
}

func StubState(in stubState) *api.State {
	out := &api.State{
		Assignments: make(map[string]string),
		DefaultCPUSet: in.defaultCPUSet.String(),
		PluginState: in.pluginState,
	}

	for id, _ := range in.updatedContainers {
		if cset, ok := in.assignments[id]; ok {
			out.Assignments[id] = cset.String()
		} else {
			out.Assignments[id] = "deleted"
		}
	}

	return out
}
