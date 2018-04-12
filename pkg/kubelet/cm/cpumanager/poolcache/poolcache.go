/*
Copyright 2018 The Kubernetes Authors.

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

package poolcache

import (
	// "fmt"

	// "github.com/golang/glog"
	// "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	// "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

type poolCache struct {
	// map from pools to cpu sets
	pools map[string]cpuset.CPUSet
	// map from container ids to pools
	containers  map[string]string
	initialized bool
}

type PoolCache interface {
	GetCPUPoolStats() *map[string]cpuset.CPUSet
	GetCPUPoolContainers() *map[string]string
	SetCPUPools(*map[string]cpuset.CPUSet)
	AddContainer(cid string, pool string)
	RemoveContainer(cid string)
	IsInitialized() bool
}

var _ PoolCache = &poolCache{}

var cache *poolCache

// singleton pattern, TODO: check concurrency
func GetCPUPoolCache() *poolCache {
	if cache == nil {
		p := make(map[string]cpuset.CPUSet)
		c := make(map[string]string)
		cache = &poolCache{
			initialized: false,
			pools:       p,
			containers:  c,
		}
	}
	return cache
}

func (p *poolCache) SetCPUPools(m *map[string]cpuset.CPUSet) {
	newMap := make(map[string]cpuset.CPUSet)
	for s, cpu := range *m {
		newMap[s] = cpu
	}
	p.pools = newMap
	p.initialized = true
}

func (p *poolCache) AddContainer(cid string, pool string) {
	p.containers[cid] = pool
}

func (p *poolCache) RemoveContainer(cid string) {
	delete(p.containers, cid)
}

func (p *poolCache) GetCPUPoolStats() *map[string]cpuset.CPUSet {
	return &p.pools
}

func (p *poolCache) GetCPUPoolContainers() *map[string]string {
	return &p.containers
}

func (p *poolCache) IsInitialized() bool {
	return p.initialized
}
