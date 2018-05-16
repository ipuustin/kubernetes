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
	"sync"
	"time"

	// "fmt"
	// "github.com/golang/glog"
	// "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	// "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	stats "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

type poolCache struct {
	sync.RWMutex
	pools       map[string]stats.CPUPoolUsage
	initialized bool
}

type PoolCache interface {
	GetCPUPoolStats() stats.CPUPoolStats
	UpdatePool(pool string, shared, exclusive cpuset.CPUSet, capacity, usage int64)
	AddContainer(pool, id, pod, name string, cpu int64)
	RemoveContainer(pool, id string)
	IsInitialized() bool
}

var _ PoolCache = &poolCache{}

var cache *poolCache

// singleton pattern, TODO: check concurrency
func NewCPUPoolCache() PoolCache {
	if cache == nil {
		cache = &poolCache{
			pools:       make(map[string]stats.CPUPoolUsage),
			initialized: false,
		}
	}

	return cache
}

func GetCPUPoolCache() PoolCache {
	return cache
}

func (c *poolCache) UpdatePool(pool string, shared, exclusive cpuset.CPUSet, capacity, usage int64) {
	c.Lock()
	defer c.Unlock()

	p, ok := c.pools[pool]
	if !ok {
		p = stats.CPUPoolUsage{
			Name:       pool,
			Containers: make(map[string]stats.CPUPoolContainer),
		}
		c.pools[pool] = p
	}

	p.SharedCPUs = shared.String()
	p.ExclusiveCPUs = exclusive.String()
	p.Capacity = capacity
	p.Usage = usage
}

func (c *poolCache) AddContainer(pool, cid, pod, name string, cpu int64) {
	c.Lock()
	defer c.Unlock()

	if p, ok := c.pools[pool]; ok {
		p.Containers[cid] = stats.CPUPoolContainer{
			ID:        cid,
			Pod:       pod,
			Container: name,
			Time:      metav1.NewTime(time.Now()),
			CPU:       cpu,
		}
	}
}

func (c *poolCache) RemoveContainer(pool, cid string) {
	c.Lock()
	defer c.Unlock()

	if p, ok := c.pools[pool]; ok {
		delete(p.Containers, cid)
	}
}

func (c *poolCache) GetCPUPoolStats() stats.CPUPoolStats {
	c.RLock()
	defer c.RUnlock()

	pools := make(map[string]stats.CPUPoolUsage)

	for pool, usage := range c.pools {
		pools[pool] = usage
	}

	return stats.CPUPoolStats{
		Time:  metav1.NewTime(time.Now()),
		Pools: pools,
	}
}

func (c *poolCache) IsInitialized() bool {
	if c == nil {
		return false
	}

	c.RLock()
	defer c.RUnlock()

	return c.initialized
}
