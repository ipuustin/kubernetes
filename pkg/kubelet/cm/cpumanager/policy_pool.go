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
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/pool"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/poolcache"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

// PolicyPool is the name of the pool policy
const PolicyPool policyName = "pool"

var _ Policy = &poolPolicy{}

type poolPolicy struct {
	// cpu socket topology
	topology *topology.CPUTopology
	// pool configuration
	poolCfg pool.NodeConfig
	// pool usage/stats cache
	stats poolcache.PoolCache
}

// Ensure poolPolicy implements Policy interface
var _ Policy = &poolPolicy{}

// NewPoolPolicy returns a CPU manager policy that can do both shared
// and exclusive allocations from named and preconfigured pools of CPU
// cores.
func NewPoolPolicy(topology *topology.CPUTopology, numReservedCPUs int, cpuPoolConfig map[string]string) Policy {
	cfg, err := pool.ParseNodeConfig(numReservedCPUs, cpuPoolConfig)
	if err != nil {
		panic(fmt.Errorf("[cpumanager] failed to parse pool configuration %v: %v", cpuPoolConfig, err))
	}

	return &poolPolicy{
		topology: topology,
		poolCfg:  cfg,
		stats: poolcache.NewCPUPoolCache(),
	}
}

func (p *poolPolicy) Name() string {
	return string(PolicyPool)
}

func (p *poolPolicy) Start(s state.State) {
	s.SetAllocator(takeByTopology, p.topology)

	if err := s.Reconfigure(p.poolCfg); err != nil {
		glog.Errorf("[cpumanager] pool policy failed to start: %s", err.Error())
		panic("[cpumanager] - please drain node and remove policy state file")
	}

	if err := p.validateState(s); err != nil {
		glog.Errorf("[cpumanager] pool policy invalid state: %s", err.Error())
		panic("[cpumanager] - please drain node and remove policy state file")
	}
}

func (p *poolPolicy) validateState(s state.State) error {
	pools := s.GetPoolCPUs()
	containers := s.GetPoolAssignments()

	//res := pools[pool.ReservedPool]
	//def := pools[pool.DefaultPool]

	// Check that all shared sets of CPU pools are disjoint.
	cpus := cpuset.NewCPUSet()
	for name, cset := range pools {
		if !cpus.Intersection(cset).IsEmpty() {
			return fmt.Errorf("[cpumanager] pool %s (%s) has overlapping CPUs with another pool", name, cset)
		}
		cpus = cpus.Union(cset)
	}

	// Check that exclusive allocations and shared sets of pools are disjoint.
	for id, cset := range containers {
		if !cpus.Intersection(cset).IsEmpty() {
			return fmt.Errorf("[cpumanager] container %s (%s) has overlapping exclusive CPUs with a shared pool", id, cset)
		}
	}

	return nil
}

func (p *poolPolicy) AddContainer(s state.State, pod *v1.Pod, container *v1.Container, containerID string) error {
	var err error

	if _, ok := s.GetCPUSet(containerID); ok {
		glog.Infof("[cpumanager] pool policy: container already present in state, skipping (container: %s, container id: %s)", container.Name, containerID)
		return nil
	}

	pool, req, lim := pool.GetContainerPoolResources(pod, container)

	glog.Infof("[cpumanager] pool policy: container %s asks for %d/%d from pool %s", containerID, req, lim, pool)

	if req != 0 && req == lim && req % 1000 == 0 {
		_, err = s.AllocateCPUs(containerID, pool, int(req / 1000))
	} else {
		_, err = s.AllocateCPU(containerID, pool, req)
	}

	if err != nil {
		glog.Errorf("[cpumanager] unable to allocate CPUs (container id: %s, error: %v)", containerID, err)
		return err
	}

	p.stats.AddContainer(pool, containerID, pod.Name, container.Name, int64(req))

	return nil
}

func (p *poolPolicy) RemoveContainer(s state.State, containerID string) error {
	pool := s.GetContainerPoolName(containerID)
	s.ReleaseCPU(containerID)
	p.stats.RemoveContainer(pool, containerID)

	return nil
}

func (p *poolPolicy) GetCapacity(s state.State) v1.ResourceList {
	return s.GetPoolCapacity()
}
