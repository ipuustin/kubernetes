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

package cpumanager

import (
	"fmt"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"

	admission "k8s.io/kubernetes/plugin/pkg/admission/cpupool"
)

// PolicyPool is the name of the pool policy
const PolicyPool policyName = "pool"

var _ Policy = &poolPolicy{}

// staticPolicy is a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
//
// This policy allocates CPUs exclusively for a container if all the following
// conditions are met:
//
// - The pod QoS class is Guaranteed.
// - The CPU request is a positive integer.
//
// The static policy maintains the following sets of logical CPUs:
//
// - SHARED: Burstable, BestEffort, and non-integral Guaranteed containers
//   run here. Initially this contains all CPU IDs on the system. As
//   exclusive allocations are created and destroyed, this CPU set shrinks
//   and grows, accordingly. This is stored in the state as the default
//   CPU set.
//
// - RESERVED: A subset of the shared pool which is not exclusively
//   allocatable. The membership of this pool is static for the lifetime of
//   the Kubelet. The size of the reserved pool is
//   ceil(systemreserved.cpu + kubereserved.cpu).
//   Reserved CPUs are taken topologically starting with lowest-indexed
//   physical core, as reported by cAdvisor.
//
// - ASSIGNABLE: Equal to SHARED - RESERVED. Exclusive CPUs are allocated
//   from this pool.
//
// - EXCLUSIVE ALLOCATIONS: CPU sets assigned exclusively to one container.
//   These are stored as explicit assignments in the state.
//
// When an exclusive allocation is made, the static policy also updates the
// default cpuset in the state abstraction. The CPU manager's periodic
// reconcile loop takes care of rewriting the cpuset in cgroupfs for any
// containers that may be running in the shared pool. For this reason,
// applications running within exclusively-allocated containers must tolerate
// potentially sharing their allocated CPUs for up to the CPU manager
// reconcile period.

type CPUPool struct {
	cpus cpuset.CPUSet
}

type poolPolicy struct {
	// cpu socket topology
	topology *topology.CPUTopology
	// CPU pools
	pools map[string]cpuset.CPUSet
	// set of CPUs that is not available for exclusive assignment
	reserved cpuset.CPUSet
}

// Ensure poolPolicy implements Policy interface
var _ Policy = &poolPolicy{}

// predefined CPU pool names
// reserved: pool for system- and kube-reserved CPUs
const CpuPoolReserved = "reserved"
// default: pool for workloads that don't ask for a specific pool
const CpuPoolDefault = "default"


// NewPoolPolicy returns a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
func NewPoolPolicy(topology *topology.CPUTopology, numReservedCPUs int, poolConfig map[string][]int) Policy {
	var reservedSet, reservedPool, defaultPool cpuset.CPUSet
	var err error
	var ok bool
	
	pools := make(map[string]cpuset.CPUSet)

	for name, cpuids := range poolConfig {
		pools[name] = cpuset.NewCPUSet(cpuids...)
		glog.Infof("[cpumanager] CPU pool %s => CPUs %s", name, pools[name].String())
	}

	if reservedPool, ok = pools[CpuPoolReserved]; !ok {
		panic(fmt.Sprintf("[cpumanager] missing kube-/system-reserved pool"))
	}

	if reservedSet, err = takeByTopology(topology, reservedPool, numReservedCPUs); err != nil {
		panic(err)
	}

	if reservedSet.Size() != numReservedCPUs {
		panic(fmt.Sprintf("[cpumanager] unable to reserve the required amount of CPUs (size of %s did not equal %d)", reservedSet, numReservedCPUs))
	}

	glog.Infof("[cpumanager] reserved %d CPUs (\"%s\") not available for normal workloads", reservedSet.Size(), reservedSet)

	extra := reservedPool.Difference(reservedSet)
	pools[CpuPoolReserved] = reservedSet

	if !extra.IsEmpty() {
		if defaultPool, ok = pools[CpuPoolDefault]; ok {
			glog.Warningf("[cpumanager] adding unreserved CPUs (%s) to default pool", extra.String())
			pools[CpuPoolDefault] = defaultPool.Union(extra)
		} else {
			glog.Warningf("[cpumanager] creating default pool from unreserved CPUs (%s)", extra.String())
			pools[CpuPoolDefault] = extra
		}
	}

	return &poolPolicy{
		topology: topology,
		pools: pools,
		reserved: reservedSet,
	}
}

func (p *poolPolicy) Name() string {
	return string(PolicyPool)
}

func (p *poolPolicy) Start(s state.State) {
	if err := p.validateState(s); err != nil {
		glog.Errorf("[cpumanager] pool policy invalid state: %s\n", err.Error())
		panic("[cpumanager] - please drain node and remove policy state file")
	}
}

func (p *poolPolicy) validateState(s state.State) error {
	tmpAssignments := s.GetCPUAssignments()
	tmpDefaultCPUset := s.GetDefaultCPUSet()

	// Default cpuset cannot be empty when assignments exist
	if tmpDefaultCPUset.IsEmpty() {
		if len(tmpAssignments) != 0 {
			return fmt.Errorf("default cpuset cannot be empty")
		}
		// state is empty initialize
		//allCPUs := p.topology.CPUDetails.CPUs()
		//s.SetDefaultCPUSet(allCPUs)
		glog.Infof("[cpumanager] setting default pool to CPUs %s", p.pools["default"].String())
		s.SetDefaultCPUSet(p.pools["default"])
		return nil
	}


	// State has already been initialized from file (is not empty)
	// 1 Check if the reserved cpuset is not part of default cpuset because:
	// - kube/system reserved have changed (increased) - may lead to some containers not being able to start
	// - user tampered with file
	//if !p.reserved.Intersection(tmpDefaultCPUset).Equals(p.reserved) {
	//	return fmt.Errorf("not all reserved cpus: \"%s\" are present in defaultCpuSet: \"%s\"",
	//		p.reserved.String(), tmpDefaultCPUset.String())
	//}

	// 2. Check if state for static policy is consistent
	for cID, cset := range tmpAssignments {
		// None of the cpu in DEFAULT cset should be in s.assignments
		if !tmpDefaultCPUset.Intersection(cset).IsEmpty() {
			return fmt.Errorf("container id: %s cpuset: \"%s\" overlaps with default cpuset \"%s\"",
				cID, cset.String(), tmpDefaultCPUset.String())
		}
	}
	return nil
}

func getContainerPool(pod *v1.Pod) string {
	if name, found := pod.Labels[admission.LabelName]; found {
		return name
	}

	return CpuPoolDefault
}

func (p *poolPolicy) AddContainer(s state.State, pod *v1.Pod, container *v1.Container, containerID string) error {
	pool := getContainerPool(pod)

	glog.Infof("[cpumanager] pool policy: AddContainer (pod: %s, container: %s, container id: %s, pool: %s)", pod.Name, container.Name, containerID, pool)

	cpuset, ok := p.pools[pool]
	if !ok {
		return fmt.Errorf("[cpumanager] pool policy: request for unknown pool %s", pool)
	}

	s.SetCPUSet(containerID, cpuset)

	return nil
}

func (p *poolPolicy) RemoveContainer(s state.State, containerID string) error {
	glog.Infof("[cpumanager] pool policy: RemoveContainer (container id: %s)", containerID)
	if _, ok := s.GetCPUSet(containerID); ok {
		s.Delete(containerID)
	}
	return nil
}
