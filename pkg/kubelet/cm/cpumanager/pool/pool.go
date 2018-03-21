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

//
// The pool policy uses a set of CPU pools to allocate resources to containers.
// The pools are configured externally and are explicitly referenced by name in
// Pod specifications. Both exclusive and shared CPU allocations are supported.
// Exclusively allocated CPU cores are dedicated to the allocating container.
//
// There is a number of pre-defined pools which special semantics. These are
//
//  - reserved:
//    The reserved pool is the set of cores which system- and kube-reserved
//    are taken from. Excess capacity, anything beyond the reserved capacity,
//    is allocated to shared workloads in the default pool. Only containers
//    in the kube-system namespace are allowed to allocate CPU from this pool.
//
//  - default:
//    Pods which do not request any explicit pool by name are allocated CPU
//    from the default pool.
//
//  - offline:
//    Pods which are taken offline are in this pool. This pool is only used to
//    administed the offline CPUs, allocations are not allowed from this pool.
//
//  - ignored:
//    CPUs in this pool are ignored. They can be fused outside of kubernetes.
//    Allocations are not allowed from this pool.
//
// The actual allocation of CPU cores from the cpu set in a pool is done by an
// externally provided function. It is usally set to the stock topology-aware
// allocation function (takeByTopology) provided by CPUManager.
//

package pool

import (
	"fmt"
	"strings"
	"strconv"
	"encoding/json"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"

	admission "k8s.io/kubernetes/plugin/pkg/admission/cpupool"
)

// Predefined CPU pool names.
const (
	Prefix       = admission.ResourcePrefix // prefix for CPU pool resources
	IgnoredPool  = admission.IgnoredPool    // CPUs we have to ignore
	OfflinePool  = admission.OfflinePool    // CPUs which are offline
	ReservedPool = admission.ReservedPool   // CPUs reserved for kube and system
	DefaultPool  = admission.DefaultPool    // CPUs in the default set
)

// CPU allocation flags
type CpuFlags int

const (
	AllocShared    CpuFlags = 0x00 // allocate to shared set in pool
	AllocExclusive CpuFlags = 0x01 // allocate exclusively in pool
	KubePinned     CpuFlags = 0x00 // we take care of CPU pinning
	WorkloadPinned CpuFlags = 0x02 // workload takes care of CPU pinning
	DefaultFlags   CpuFlags = AllocShared | KubePinned
)

// Node CPU pool configuration (PoolSetConfig could be a more apt name).
type Config map[string]cpuset.CPUSet

// A container assigned to run in a pool.
type Container struct {
	id   string         // container ID
	pool string         // assigned pool
	cpus cpuset.CPUSet  // exclusive CPUs, if any
	mCPU int64          // requested milliCPUs
}

// A CPU pool is a set of cores, typically set aside for a class of workloads.
type Pool struct {
	shared cpuset.CPUSet    // shared set of CPUs
	exclusive cpuset.CPUSet // exclusively allocated CPUs
	used int64              // total allocations in shared set
}

// A CPU allocator function.
type AllocCpuFunc func(*topology.CPUTopology, cpuset.CPUSet, int) (cpuset.CPUSet, error)

// All pools available for kube on this node.
type PoolSet struct {
	active      Config                    // active pool configuration
	pools       map[string]*Pool          // all CPU pools
	containers  map[string]*Container     // containers assignments
	target      Config                    // requested pool configuration
	topology   *topology.CPUTopology      // CPU topology info
	allocfn     AllocCpuFunc              // CPU allocator function
}

// Adjust pool configuration to have at least a reserved and default.
func setConfigDefaults(cpuPoolConfig *map[string]string, numReservedCPUs int) {
	if *cpuPoolConfig == nil {
		*cpuPoolConfig = make(map[string]string)
	}

	cfg := *cpuPoolConfig

	if _, ok := cfg[ReservedPool]; !ok {
		cfg[ReservedPool] = fmt.Sprintf("@%d", numReservedCPUs)
	}
	if _, ok := cfg[DefaultPool]; !ok {
		cfg[DefaultPool] = "*"
	}
}

// Create a default pool configuration.
func DefaultConfig(topo *topology.CPUTopology, allocfn AllocCpuFunc, numReservedCPUs int, cpuPoolConfig map[string]string) (Config, error) {
	var err error

	defCfg := make(Config)
	allCPUs := topo.CPUDetails.CPUs()

	setConfigDefaults(&cpuPoolConfig, numReservedCPUs)

	// explicitly ignore any custom pools
	for pool, cfg := range cpuPoolConfig {
		switch pool {
		case IgnoredPool:
		case OfflinePool:
		case ReservedPool:
		case DefaultPool:
		default:
			glog.Warningf("[cpumanager] ignoring unexpected pool %s (%s)", pool, cfg)
			delete(cpuPoolConfig, pool)
		}
	}

	//
	// Our default pool setup logic is ignoring all CPUs in the ignored and
	// offline pools, then allocating the reserved, and the default ones.
	//
	// If the reserved pool is unspecified it will be populated starting
	// from the lowest-numbered CPU, like in the original static policy.
	// If the default pool is unspecified it will be populated with all
	// remaining unused CPUs, again like in the orignal static policy.
	//

	if cfg, ok := cpuPoolConfig[IgnoredPool]; ok {
		if defCfg[IgnoredPool], err = cpuset.Parse(cfg); err != nil {
			return nil, fmt.Errorf("[cpumanager] invalid pool configuration: %s = %s", IgnoredPool, cfg)
		}

		allCPUs = allCPUs.Difference(defCfg[IgnoredPool])
	}

	if cfg, ok := cpuPoolConfig[OfflinePool]; ok {
		if defCfg[OfflinePool], err = cpuset.Parse(cfg); err != nil {
			return nil, fmt.Errorf("[cpumanager] invalid pool configuration: %s = %s, %v", OfflinePool, cfg, err)
		}

		allCPUs = allCPUs.Difference(defCfg[OfflinePool])
	}

	cfg, _ := cpuPoolConfig[ReservedPool]
	if cfg[0:1] == "@" {
		var count int
		if count, err = strconv.Atoi(cfg[1:]); err != nil {
			return nil, fmt.Errorf("[cpumanager] invalid pool configuration: %s = %s, uses invalid count: %v", ReservedPool, cfg, err)
		} else {
			if count > numReservedCPUs {
				numReservedCPUs = count
			}
		}
		if defCfg[ReservedPool], err = allocfn(topo, allCPUs, count); err != nil {
			return nil, err
		}
	} else {
		if defCfg[ReservedPool], err = cpuset.Parse(cfg); err != nil {
			return nil, err
		}

		reserved := defCfg[ReservedPool]
		diff := reserved.Size() - numReservedCPUs
		if diff < 0 {
			return nil, fmt.Errorf("[cpumanager] invalid pool configuration %s has less than %d CPUs", reserved.String(), numReservedCPUs)
		}
		if diff > 0 {
			defCfg[ReservedPool], _ = allocfn(topo, reserved, numReservedCPUs)
		}
	}
	allCPUs = allCPUs.Difference(defCfg[ReservedPool])

	cfg, _ = cpuPoolConfig[DefaultPool]
	if cfg == "*" {
		defCfg[DefaultPool] = allCPUs
		allCPUs = cpuset.NewCPUSet()
	} else {
		if cfg[0:1] == "@" {
			if count, err := strconv.Atoi(cfg[1:]); err != nil {
				return nil, fmt.Errorf("[cpumanager] invalid pool configuration: %s = %s, uses invalid count: %v", DefaultPool, cfg, err)
			} else if defCfg[ReservedPool], err = allocfn(topo, allCPUs, count); err != nil {
				return nil, err
			}
		} else {
			if defCfg[DefaultPool], err = cpuset.Parse(cfg); err != nil {
				return nil, fmt.Errorf("[cpumanager] invalid pool configuration: %s = %s, %v", DefaultPool, cfg, err)
			}

			if !defCfg[DefaultPool].IsSubsetOf(allCPUs) {
				return nil, fmt.Errorf("[cpumanager] invalid pool configuration (%s), CPUs %s already taken",
					DefaultPool, defCfg[DefaultPool].Difference(allCPUs).String())
			}
		}
	}
	allCPUs = allCPUs.Difference(defCfg[DefaultPool])

	glog.Infof("[cpumanager] parsed default pool configuration (%s):", cpuPoolConfig)
	for pool, cpus := range defCfg {
		glog.Infof("[cpumanager]   pool %s: %s", pool, cpus.String())
	}

	if allCPUs.Size() != 0 {
		glog.Warningf("[cpumanager] unused CPUs in default configuration: %s", allCPUs.String())
	}

	return defCfg, nil
}

// Parse the given CPU pool configuration (file or string).
func ParseConfig(topo *topology.CPUTopology, allocfn AllocCpuFunc, numReservedCPUs int, cpuPoolConfig map[string]string) (Config, error) {
	var err error

	setConfigDefaults(&cpuPoolConfig, numReservedCPUs)

	//
	// Our pool setup logic is basically to go through pools from the most
	// to the least specifically configured ones. We do a few extra checks
	// and adjustments on the way and finally slam the remaining unused
	// CPUs to the default pool.
	//
	// More specifically:
	//   1. create pools specified by CPU ids
	//   2. create the reserved pool if it was specified by CPU count
	//   3. create other pools specified by CPU count
	//   4. adjust reserved size to match numReservedCPUs
	//   5. set up wildcard pool if we have one
	//   6. slam the remaining CPUs into the default pool
	//

	// split up to pools specified specific CPU id, CPU count, or a wildcard
	byCpuId :=  make([]string, 0)
	byCpuCnt := make([]string, 0)
	wildcard := ""
	resCfg := ""

	for pool, cpus := range cpuPoolConfig {
		if cpus == "*" {
			if pool == IgnoredPool || pool == OfflinePool {
				return nil, fmt.Errorf("[cpumanager] pool %s can't use wildcard", pool)
			}
			if wildcard != "" {
				return nil, fmt.Errorf("[cpumanager] invalid pool configuration, multiple pools (%s, %s) has wildcard CPU", wildcard, pool)
			}
			wildcard = pool
			continue
		}
		if cpus[0:1] == "@" {
			if pool == IgnoredPool || pool == OfflinePool {
				return nil, fmt.Errorf("[cpumanager] pool %s must be declared using specific CPU IDs", pool)
			}
			if pool == ReservedPool {
				resCfg = cpus
				continue
			}
			byCpuCnt = append(byCpuCnt, pool)
		} else {
			byCpuId = append(byCpuId, pool)
		}
	}

	cfg := make(Config)
	allCPUs := topo.CPUDetails.CPUs()

	// create pools by specific CPU ids
	for _, pool := range byCpuId {
		cpus := cpuPoolConfig[pool]
		if cfg[pool], err = cpuset.Parse(cpus); err != nil {
			return nil, err
		}

		if !cfg[pool].IsSubsetOf(allCPUs) {
			return nil, fmt.Errorf("[cpumanager] invalid pool configuration (%s), CPUs %s already taken",
				pool, cfg[pool].Difference(allCPUs).String())
		}

		allCPUs = allCPUs.Difference(cfg[pool])
	}
	
	// create reserved pool if it was specified by CPU count
	pool := ReservedPool
	if _, ok := cfg[pool]; !ok {
		if count, err := strconv.Atoi(resCfg[1:]); err != nil {
			return nil, fmt.Errorf("[cpumanager] invalid pool configuration: %s = %s, uses invalid count: %v", pool, resCfg, err)
		} else if cfg[pool], err = allocfn(topo, allCPUs, count); err != nil {
			return nil, err
		}

		allCPUs = allCPUs.Difference(cfg[pool])
	}

	// create other pools by CPU count
	for _, pool := range byCpuCnt {
		countCfg := cpuPoolConfig[pool]

		if count, err := strconv.Atoi(countCfg[1:]); err != nil {
			return nil, fmt.Errorf("[cpumanager] invalid pool configuration: %s = %s, uses invalid count: %v", pool, countCfg, err)
		} else if cfg[pool], err = allocfn(topo, allCPUs, count); err != nil {
			return nil, err
		}

		allCPUs = allCPUs.Difference(cfg[pool])
	}

	// shrink reserved pool to numReservedCPUs
	reserved, _ := cfg[ReservedPool]
	diff := reserved.Size() - numReservedCPUs
	if diff < 0 {
		return nil, fmt.Errorf("[cpumanager] invalid pool configuration %s has less than %d CPUs", reserved.String(), numReservedCPUs)
	}
	if diff > 0 {
		cpus, _ := allocfn(topo, reserved, numReservedCPUs)
		extra := reserved.Difference(cpus)
		cfg[ReservedPool] = cpus
		if def, ok := cfg[DefaultPool]; ok {
			cfg[DefaultPool] = def.Union(extra)
		} else {
			cfg[DefaultPool] = extra
		}
	}

	// assign remaining CPUs to wildcard pool or the default one
	if wildcard != "" {
		cfg[wildcard] = allCPUs
		allCPUs = cpuset.NewCPUSet()
	} else {
		if def, ok := cfg[DefaultPool]; ok {
			cfg[DefaultPool] = def.Union(allCPUs)
		} else {
			cfg[DefaultPool] = allCPUs
		}
		allCPUs = cpuset.NewCPUSet()
	}

	glog.Infof("[cpumanager] parsed pool configuration (%s):", cpuPoolConfig)
	for pool, cpus := range cfg {
		glog.Infof("[cpumanager]   pool %s: %s", pool, cpus.String())
	}

	if allCPUs.Size() != 0 {
		glog.Warningf("[cpumanager] unused CPUs in configuration: %s", allCPUs.String())
	}

	return cfg, nil
}

// Get the CPU pool, request, and limit of a container.
func GetContainerPoolResources(c *v1.Container) (string, int64, int64) {
	var pool string = DefaultPool
	var req, lim int64

	if c.Resources.Requests == nil {
		return DefaultPool, 0, 0
	}

	for name, _ := range c.Resources.Requests {
		if strings.HasPrefix(name.String(), Prefix) {
			pool = strings.TrimPrefix(name.String(), Prefix)
			break
		}
	}

	if res, ok := c.Resources.Requests[v1.ResourceCPU]; ok {
		req = res.MilliValue()
	}

	if res, ok := c.Resources.Limits[v1.ResourceCPU]; ok {
		lim = res.MilliValue()
	}

	return pool, req, lim
}

// Create a new CPU pool set with the given configuration.
func NewPoolSet(cfg Config) (*PoolSet, error) {
	glog.Infof("[cpumanager]: creating new CPU pool set")

	var ps *PoolSet = &PoolSet{
		pools:      make(map[string]*Pool),
		containers: make(map[string]*Container),
	}

	if err := ps.Reconfigure(cfg); err != nil {
		return nil, err
	}

	return ps, nil
}

// Verify the current pool state.
func (ps *PoolSet) Verify() error {
	required := []string{ ReservedPool, DefaultPool }

	for _, name := range required {
		if _, ok := ps.pools[name]; !ok {
			return fmt.Errorf("[cpumanager]: missing %s pool", name)
		}
	}

	return nil
}

// Reconfigure the CPU pool set.
func (ps *PoolSet) Reconfigure(cfg Config) error {
	if cfg == nil {
		return nil
	}

	glog.Infof("[cpumanager]: reconfiguring CPU pools with %v", cfg)

	ps.target = cfg
	_, err := ps.ReconcileConfig()

	return err
}

// Run one round of reconcilation of the CPU pool set configuration.
func (ps *PoolSet) ReconcileConfig() (bool, error) {
	glog.Infof("[cpumanager]: trying to reconciling configuration: %v -> %v", ps.active, ps.target)

	if ps.target == nil {
		return false, nil
	}

	//
	// trivial case: no active container assignments
	//
	// Discard everything, and take the configuration in use.
	//
	if len(ps.containers) == 0 {
		ps.active     = ps.target
		ps.target     = nil
		ps.pools      = make(map[string]*Pool)
		ps.containers = make(map[string]*Container)

		for name, cpus := range ps.active {
			ps.pools[name] = &Pool{
				shared:    cpus.Clone(),
				exclusive: cpuset.NewCPUSet(),
			}
		}

		if err := ps.Verify(); err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

// Set the CPU allocator function, and CPU topology information.
func (ps *PoolSet) SetAllocator(allocfn AllocCpuFunc, topo *topology.CPUTopology) {
	ps.allocfn  = allocfn
	ps.topology = topo
}

func checkAllowedPool(pool string) error {
	if pool == IgnoredPool || pool == OfflinePool {
		return fmt.Errorf("[cpumanager] can't allocate from pool %s", pool)
	} else {
		return nil
	}
}

// Allocate a number of CPUs exclusively from a pool.
func (ps *PoolSet) AllocateCPUs(id string, pool string, numCPUs int) (cpuset.CPUSet, error) {
	if pool == "" {
		pool = DefaultPool
	}

	if err := checkAllowedPool(pool); err != nil {
		return cpuset.NewCPUSet(), err
	}

	p, ok := ps.pools[pool]
	if !ok {
		return cpuset.NewCPUSet(), fmt.Errorf("[cpumanager] non-existent pool %s", pool)
	}

	cpus, err := ps.allocfn(ps.topology, p.shared, numCPUs)
	if err != nil {
		return cpuset.NewCPUSet(), err
	}

	p.shared = p.shared.Difference(cpus)
	p.exclusive = p.exclusive.Union(cpus)

	ps.containers[id] = &Container{
		id:   id,
		pool: pool,
		cpus: cpus,
		mCPU: int64(cpus.Size()) * 1000,
	}

	glog.Infof("[cpumanager] allocated %s/CPU:%s for container %s", pool, cpus.String(), id)

	return cpus.Clone(), nil
}

// Allocate CPU for a container from a pool.
func (ps *PoolSet) AllocateCPU(id string, pool string, milliCPU int64) (cpuset.CPUSet, error) {
	var cpus cpuset.CPUSet

	if err := checkAllowedPool(pool); err != nil {
		return cpuset.NewCPUSet(), nil
	}

	p, ok := ps.pools[pool]
	if !ok {
		return cpuset.NewCPUSet(), fmt.Errorf("[cpumanager] pool %s not found", pool)
	}

	ps.containers[id] = &Container{
		id:   id,
		pool: pool,
		cpus: cpuset.NewCPUSet(),
		mCPU: milliCPU,
	}

	cpus = p.shared.Clone()
	p.used += milliCPU

	glog.Infof("[cpumanager] allocated %dm of %s/CPU:%s for container %s", milliCPU, pool, cpus.String(), id)

	return cpus, nil
}

// Return CPU from a container to a pool.
func (ps *PoolSet) ReleaseCPU(id string) {
	c, ok := ps.containers[id]
	if !ok {
		glog.Warningf("[cpumanager] couldn't find allocations for container %s", id)
		return
	}

	delete(ps.containers, id)

	p, ok := ps.pools[c.pool]
	if !ok {
		glog.Warningf("[cpumanager] couldn't find pool %s for container %s", c.pool, id)
		return
	}

	if c.cpus.IsEmpty() {
		p.used -= c.mCPU
		glog.Infof("cpumanager] released %dm of %s/CPU:%s for container %s", c.mCPU, c.pool, p.shared.String(), c.id)
	} else {
		p.shared    = p.shared.Union(c.cpus)
		p.exclusive = p.exclusive.Difference(c.cpus)

		glog.Infof("[cpumanager] released %s/CPU:%s for container %s", c.pool, p.shared.String(), c.id)
	}
}

// Get the CPU capacity of pools.
func (ps *PoolSet) GetPoolCapacity() v1.ResourceList {
	cap := v1.ResourceList{}
	def := 0

	//
	// collect pool CPU capacity
	//
	// Note: Currently we omit the reserved pool altogether
	//       and report its capacity in the default pool.
	//       This is in line how we allocate CPU for the
	//       default pool (and it mimicks the static policy).
	//

	for name, pool := range ps.pools {
		qty := 1000 * (pool.shared.Size() + pool.exclusive.Size())

		if name == ReservedPool || name == DefaultPool {
			def += qty
		} else {
			res := v1.ResourceName(Prefix + name)
			cap[res] = *resource.NewQuantity(int64(qty), resource.DecimalSI)
		}
	}

	res := v1.ResourceName(Prefix + DefaultPool)
	cap[res] = *resource.NewQuantity(int64(def), resource.DecimalSI)

	return cap
}

// Get the (shared) CPU sets for pools.
func (ps *PoolSet) GetPoolCPUs() map[string]cpuset.CPUSet {
	cpus := make(map[string]cpuset.CPUSet)

	for name, p := range ps.pools {
		cpus[name] = p.shared.Clone()
	}

	return cpus
}

// Get the exclusively allocated CPU sets.
func (ps *PoolSet) GetPoolAssignments() map[string]cpuset.CPUSet {
	cpus := make(map[string]cpuset.CPUSet)

	for id, c := range ps.containers {
		cpus[id] = c.cpus.Clone()
	}

	return cpus
}

// Get the CPU allocations for a container.
func (ps *PoolSet) GetContainerCPUSet(id string) (cpuset.CPUSet, bool) {
	c, ok := ps.containers[id]
	if !ok {
		return cpuset.NewCPUSet(), false
	}

	cpus := c.cpus.Clone()

	if c.pool == DefaultPool {
		cpus = cpus.Union(ps.pools[ReservedPool].shared)
	}

	return cpus, true
}

// Get the shared CPUs of a pool.
func (ps *PoolSet) GetPoolCPUSet(pool string) (cpuset.CPUSet, bool) {
	p, ok := ps.pools[pool]
	if !ok {
		return cpuset.NewCPUSet(), false
	}

	if pool == DefaultPool {
		return p.shared.Union(ps.pools[ReservedPool].shared), true
	} else {
		return p.shared.Clone(), true
	}
}

// Get the exclusive CPU assignments as ContainerCPUAssignments.
func (ps *PoolSet) GetCPUAssignments() map[string]cpuset.CPUSet {
	a := make(map[string]cpuset.CPUSet)

	for _, c := range ps.containers {
		if !c.cpus.IsEmpty() {
			a[c.id] = c.cpus.Clone()
		}
	}

	return a
}

//
// JSON mashalling and unmarshalling
//


// Container JSON marshalling interface
type marshalContainer struct {
	Id   string        `json:"id"`
	Pool string        `json:"pool"`
	Cpus cpuset.CPUSet `json:"cpus"`
	MCPU int64         `json:"mCPU"`
}

func (pc Container) MarshalJSON() ([]byte, error) {
	return json.Marshal(marshalContainer{
		Id:   pc.id,
		Pool: pc.pool,
		Cpus: pc.cpus,
		MCPU: pc.mCPU,
	})
}

func (pc *Container) UnmarshalJSON(b []byte) error {
	var m marshalContainer

	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	pc.id   = m.Id
	pc.pool = m.Pool
	pc.cpus = m.Cpus
	pc.mCPU = m.MCPU

	return nil
}

// Pool JSON marshalling interface
type marshalPool struct {
	Shared    cpuset.CPUSet `json:"shared"`
	Exclusive cpuset.CPUSet `json:"exclusive"`
	Used      int64         `json:"used"`
}

func (p Pool) MarshalJSON() ([]byte, error) {
	return json.Marshal(marshalPool{
		Shared:    p.shared,
		Exclusive: p.exclusive,
		Used:      p.used,
	})
}

func (p *Pool) UnmarshalJSON(b []byte) error {
	var m marshalPool

	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	p.shared    = m.Shared
	p.exclusive = m.Exclusive
	p.used      = m.Used

	return nil
}

// PoolSet JSON marshalling interface
type marshalPoolSet struct {
	Active     Config                    `json:"active"`
	Pools      map[string]*Pool          `json:"pools"`
	Containers map[string]*Container     `json:"containers"`
	Target     Config                    `json:"target,omitempty"`
}

func (ps PoolSet) MarshalJSON() ([]byte, error) {
	return json.Marshal(marshalPoolSet{
		Active:     ps.active,
		Pools:      ps.pools,
		Containers: ps.containers,
		Target:     ps.target,
	})
}

func (ps *PoolSet) UnmarshalJSON(b []byte) error {
	var m marshalPoolSet

	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	ps.active     = m.Active
	ps.pools      = m.Pools
	ps.containers = m.Containers
	ps.target     = m.Target

	return nil
}
