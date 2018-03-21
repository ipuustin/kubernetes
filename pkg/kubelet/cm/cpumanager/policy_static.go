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
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/pool"
)

// PolicyStatic is the name of the static policy
const PolicyStatic policyName = "static"

var _ Policy = &staticPolicy{}

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
type staticPolicy struct {
	// cpu socket topology
	topology *topology.CPUTopology
	// pool configuration
	poolCfg pool.Config
}

// Ensure staticPolicy implements Policy interface
var _ Policy = &staticPolicy{}

// NewStaticPolicy returns a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
func NewStaticPolicy(topology *topology.CPUTopology, numReservedCPUs int, cpuPoolConfig map[string]string) Policy {
	cfg, err := pool.DefaultConfig(topology, takeByTopology, numReservedCPUs, cpuPoolConfig)
	if err != nil {
		panic(fmt.Errorf("[cpumanager] failed to set up default CPU pools: %v", err))
	}

	return &staticPolicy{
		topology: topology,
		poolCfg:  cfg,
	}
}

func (p *staticPolicy) Name() string {
	return string(PolicyStatic)
}

func (p *staticPolicy) Start(s state.State) {
	s.SetAllocator(takeByTopology, p.topology)

	cfg := p.poolCfg
	reserved, _ := cfg[pool.ReservedPool]
	glog.Infof("[cpumanager] reserved %d CPUs (\"%s\") not available for exclusive assignment", reserved.Size(), reserved)

	if err := s.Reconfigure(cfg); err != nil {
		glog.Errorf("[cpumanager] static policy failed to start: %s\n", err.Error())
		panic("[cpumanager] - please drain node and remove policy state file")
	}

	if err := p.validateState(s); err != nil {
		glog.Errorf("[cpumanager] static policy invalid state: %s\n", err.Error())
		panic("[cpumanager] - please drain node and remove policy state file")
	}
}

func (p *staticPolicy) validateState(s state.State) error {
	pools := s.GetPoolCPUs()
	containers := s.GetPoolAssignments()

	res := pools[pool.ReservedPool]
	def := pools[pool.DefaultPool]

	// State has already been initialized from file (is not empty)
	// 1. Check that the reserved and default cpusets are disjoint:
	// - kube/system reserved have changed (increased) - may lead to some containers not being able to start
	// - user tampered with file
	if !res.Intersection(def).IsEmpty() {
		return fmt.Errorf("overlapping reserved (%s) and default (%s) CPI pools", res.String(), def.String())
	}

	// 2. Check if state for static policy is consistent: exclusive assignments and (default U reserved) are disjoint.
	resdef := res.Union(def)
	for id, cpus := range containers {
		if !cpus.IsEmpty() {
			if !resdef.Union(cpus).IsEmpty() {
				return fmt.Errorf("container id: %s cpuset: \"%s\" overlaps with default cpuset \"%s\"",
					id, cpus.String(), resdef.String())
			}
		}
	}

	return nil
}

func (p *staticPolicy) AddContainer(s state.State, pod *v1.Pod, container *v1.Container, containerID string) error {
	if numCPUs := guaranteedCPUs(pod, container); numCPUs != 0 {
		glog.Infof("[cpumanager] static policy: AddContainer (pod: %s, container: %s, container id: %s)", pod.Name, container.Name, containerID)
		// container belongs in an exclusively allocated pool

		if _, ok := s.GetCPUSet(containerID); ok {
			glog.Infof("[cpumanager] static policy: container already present in state, skipping (container: %s, container id: %s)", container.Name, containerID)
			return nil
		}

		_, err := s.AllocateCPUs(containerID, pool.DefaultPool, numCPUs)
		if err != nil {
			glog.Errorf("[cpumanager] unable to allocate %d CPUs (container id: %s, error: %v)", numCPUs, containerID, err)
			return err
		}
	}

	return nil
}

func (p *staticPolicy) RemoveContainer(s state.State, containerID string) error {
	glog.Infof("[cpumanager] static policy: RemoveContainer (container id: %s)", containerID)
	s.ReleaseCPU(containerID)

	return nil
}

func guaranteedCPUs(pod *v1.Pod, container *v1.Container) int {
       if v1qos.GetPodQOS(pod) != v1.PodQOSGuaranteed {
               return 0
        }
       cpuQuantity := container.Resources.Requests[v1.ResourceCPU]
       if cpuQuantity.Value()*1000 != cpuQuantity.MilliValue() {
               return 0
        }
       // Safe downcast to do for all systems with < 2.1 billion CPUs.
       // Per the language spec, `int` is guaranteed to be at least 32 bits wide.
       // https://golang.org/ref/spec#Numeric_types
       return int(cpuQuantity.Value())
}
