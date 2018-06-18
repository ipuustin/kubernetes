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

package main

import (
	"fmt"
	"flag"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
//	api "k8s.io/kubernetes/pkg/kubelet/apis/cpuplugin/v1alpha"
	stub "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/stub"
)

const (
	logPrefix    = "[static-cpu-policy] "
	PolicyStatic = "static"
)

type staticPolicy struct {
	topology *topology.CPUTopology
	reserved cpuset.CPUSet
}

// Ensure that staticPolicy implements the CpuPolicy interface.
var _ stub.CpuPolicy = &staticPolicy{}

func NewStaticPolicy() stub.CpuPlugin {
	policy := staticPolicy{}
	plugin, err := stub.NewCpuPlugin(&policy, "intel.com")
	if err != nil {
		logPanic("failed to create CPU plugin stub for static policy: %+v", err)
	}

	return plugin
}

func (p *staticPolicy) Name() string {
	return string(PolicyStatic)
}

func (p *staticPolicy) Start(s stub.State, topology *topology.CPUTopology, numReservedCPUs int) error {
	allCPUs := topology.CPUDetails.CPUs()
	// takeByTopology allocates CPUs associated with low-numbered cores from
	// allCPUs.
	//
	// For example: Given a system with 8 CPUs available and HT enabled,
	// if numReservedCPUs=2, then reserved={0,4}
	reserved, _ := cpumanager.TakeByTopology(topology, allCPUs, numReservedCPUs)

	if reserved.Size() != numReservedCPUs {
		panic(fmt.Sprintf("[cpumanager] unable to reserve the required amount of CPUs (size of %s did not equal %d)", reserved, numReservedCPUs))
	}

	glog.Infof("[cpumanager] reserved %d CPUs (\"%s\") not available for exclusive assignment", reserved.Size(), reserved)

	p.topology = topology
	p.reserved = reserved
	
	if err := p.validateState(s); err != nil {
		glog.Errorf("[cpumanager] static policy invalid state: %s\n", err.Error())
		return fmt.Errorf("[cpumanager] - invalid state (%v), please drain node and remove policy state file", err)
	}

	return nil
}

func (p *staticPolicy) Configure(s stub.State, config map[string]string) error {
	return nil
}

func (p *staticPolicy) validateState(s stub.State) error {
	tmpAssignments := s.GetCPUAssignments()
	tmpDefaultCPUset := s.GetDefaultCPUSet()

	// Default cpuset cannot be empty when assignments exist
	if tmpDefaultCPUset.IsEmpty() {
		if len(tmpAssignments) != 0 {
			return fmt.Errorf("default cpuset cannot be empty")
		}
		// state is empty initialize
		allCPUs := p.topology.CPUDetails.CPUs()
		s.SetDefaultCPUSet(allCPUs)
		return nil
	}

	// State has already been initialized from file (is not empty)
	// 1 Check if the reserved cpuset is not part of default cpuset because:
	// - kube/system reserved have changed (increased) - may lead to some containers not being able to start
	// - user tampered with file
	if !p.reserved.Intersection(tmpDefaultCPUset).Equals(p.reserved) {
		return fmt.Errorf("not all reserved cpus: \"%s\" are present in defaultCpuSet: \"%s\"",
			p.reserved.String(), tmpDefaultCPUset.String())
	}

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

// assignableCPUs returns the set of unassigned CPUs minus the reserved set.
func (p *staticPolicy) assignableCPUs(s stub.State) cpuset.CPUSet {
	return s.GetDefaultCPUSet().Difference(p.reserved)
}

func (p *staticPolicy) AddContainer(s stub.State, pod *v1.Pod, container *v1.Container, containerID string) error {
	logInfo("AddContainer")
	if numCPUs := guaranteedCPUs(pod, container); numCPUs != 0 {
		fmt.Printf("[cpumanager] static policy: AddContainer (pod: %s, container: %s, container id: %s)", pod.Name, container.Name, containerID)
		// container belongs in an exclusively allocated pool

		if _, ok := s.GetCPUSet(containerID); ok {
			glog.Infof("[cpumanager] static policy: container already present in state, skipping (container: %s, container id: %s)", container.Name, containerID)
			return nil
		}

		cpuset, err := p.allocateCPUs(s, numCPUs)
		if err != nil {
			glog.Errorf("[cpumanager] unable to allocate %d CPUs (container id: %s, error: %v)", numCPUs, containerID, err)
			return err
		}
		s.SetCPUSet(containerID, cpuset)
	}
	// container belongs in the shared pool (nothing to do; use default cpuset)
	return nil
}

func (p *staticPolicy) RemoveContainer(s stub.State, containerID string) {
	logInfo("RemoveContainer")
	glog.Infof("[cpumanager] static policy: RemoveContainer (container id: %s)", containerID)
	if toRelease, ok := s.GetCPUSet(containerID); ok {
		s.Delete(containerID)
		// Mutate the shared pool, adding released cpus.
		s.SetDefaultCPUSet(s.GetDefaultCPUSet().Union(toRelease))
	}
}

func (p *staticPolicy) allocateCPUs(s stub.State, numCPUs int) (cpuset.CPUSet, error) {
	glog.Infof("[cpumanager] allocateCpus: (numCPUs: %d)", numCPUs)
	result, err := cpumanager.TakeByTopology(p.topology, p.assignableCPUs(s), numCPUs)
	if err != nil {
		return cpuset.NewCPUSet(), err
	}
	// Remove allocated CPUs from the shared CPUSet.
	s.SetDefaultCPUSet(s.GetDefaultCPUSet().Difference(result))

	glog.Infof("[cpumanager] allocateCPUs: returning \"%v\"", result)
	return result, nil
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

func main() {
	flag.Parse()

	plugin := NewStaticPolicy()

	if err := plugin.StartCpuPlugin(); err != nil {
		logPanic("failed to start CPU plugin stub with static policy: %+v", err)
	}
}


//
// errors and logging
//

func logFormat(format string, args ...interface{}) string {
	return fmt.Sprintf(logPrefix+format, args...)
}

func logVerbose(level glog.Level, format string, args ...interface{}) {
	glog.V(level).Infof(logFormat(logPrefix+format, args...))
}

func logInfo(format string, args ...interface{}) {
	glog.Info(logFormat(format, args...))
}

func logWarning(format string, args ...interface{}) {
	glog.Warningf(logFormat(format, args...))
}

func logError(format string, args ...interface{}) {
	glog.Errorf(logFormat(format, args...))
}

func logFatal(format string, args ...interface{}) {
	glog.Fatalf(logFormat(format, args...))
}

func logPanic(format string, args ...interface{}) {
	logFatal(format, args...)
	panic(logFormat(format, args...))
}
