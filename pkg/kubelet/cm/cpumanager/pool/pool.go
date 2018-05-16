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
	kubeapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/poolcache"
	admission "k8s.io/kubernetes/plugin/pkg/admission/cpupool"
)

const (
	logPrefix      = "[cpumanager/pool] "     // log message prefix
	ResourcePrefix = admission.ResourcePrefix // prefix for CPU pool resources
	IgnoredPool    = admission.IgnoredPool    // CPUs we have to ignore
	OfflinePool    = admission.OfflinePool    // CPUs which are offline
	ReservedPool   = admission.ReservedPool   // CPUs reserved for kube and system
	DefaultPool    = admission.DefaultPool    // CPUs in the default set
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

// Configuration for a single CPU pool.
type Config struct {
	Size  int           `json:"size"`           // number of CPUs to allocate
	Cpus *cpuset.CPUSet `json:"cpus,omitempty"` // explicit CPUs to allocate, if given
}

// Node CPU pool configuration.
type NodeConfig map[string]*Config

// A container assigned to run in a pool.
type Container struct {
	id   string         // container ID
	pool string         // assigned pool
	cpus cpuset.CPUSet  // exclusive CPUs, if any
	req  int64          // requested milliCPUs
}

// A CPU pool is a set of cores, typically set aside for a class of workloads.
type Pool struct {
	shared  cpuset.CPUSet // shared set of CPUs
	pinned  cpuset.CPUSet // exclusively allocated CPUs
	used    int64         // total allocations in shared set
	cfg    *Config        // (requested) configuration
}

// A CPU allocator function.
type AllocCpuFunc func(*topology.CPUTopology, cpuset.CPUSet, int) (cpuset.CPUSet, error)

// All pools available for kube on this node.
type PoolSet struct {
	pools       map[string]*Pool          // all CPU pools
	containers  map[string]*Container     // containers assignments
	topology   *topology.CPUTopology      // CPU topology info
	allocfn     AllocCpuFunc              // CPU allocator function
	stats       poolcache.PoolCache       // CPU pool stats/metrics cache
	reconcile   bool                      // whether needs reconcilation
}


// Create default node CPU pool configuration.
func DefaultNodeConfig(numReservedCPUs int, cpuPoolConfig map[string]string) (NodeConfig, error) {
	cfg := make(map[string]*Config)

	cfg[ReservedPool] = &Config{
		Size: numReservedCPUs,
	}
	cfg[DefaultPool] = &Config{
		Size: -1,
	}

	if cpuPoolConfig != nil {
		logWarning("ignoring given CPU pool config %v", cpuPoolConfig)
	}

	return cfg, nil
}

// Parse the given node CPU pool configuration.
func ParseNodeConfig(numReservedCPUs int, cpuPoolConfig map[string]string) (NodeConfig, error) {
	if cpuPoolConfig == nil {
		return DefaultNodeConfig(numReservedCPUs, nil)
	}

	cfg := make(map[string]*Config)

	for name, pcfg := range cpuPoolConfig {
		if pcfg[0:1] == "@" {
			if cnt, err := strconv.Atoi(pcfg[1:]); err != nil {
				return nil, err
			} else {
				cfg[name] = &Config{
					Size: cnt,
				}
			}
		} else if pcfg != "*" {
			if cset, err := cpuset.Parse(pcfg); err != nil {
				return nil, err
			} else {
				cfg[name] = &Config{
					Size: cset.Size(),
					Cpus: &cset,
				}
			}
		} else /* if pcfg == "*" */ {
			cfg[name] = &Config{
				Size: -1, // mark as wildcard
			}
		}
	}

	if c, ok := cfg[ReservedPool]; ok {
		if c.Size != numReservedCPUs {
			logWarning("setting reserved pool size to %d CPUs (was %d)",
				numReservedCPUs, c.Size)
			c.Size = numReservedCPUs
		}
	} else {
		cfg[ReservedPool] = &Config{
			Size: numReservedCPUs,
		}
	}

	return cfg, nil
}

// Get the CPU pool, request, and limit of a container.
func GetContainerPoolResources(p *v1.Pod, c *v1.Container) (string, int64, int64) {
	var pool string
	var req, lim int64

	if p.ObjectMeta.Namespace == kubeapi.NamespaceSystem {
		pool = ReservedPool
	} else {
		pool = DefaultPool
	}

	if c.Resources.Requests == nil {
		return pool, 0, 0
	}

	for name, _ := range c.Resources.Requests {
		if strings.HasPrefix(name.String(), ResourcePrefix) {
			pool = strings.TrimPrefix(name.String(), ResourcePrefix)
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
func NewPoolSet(cfg NodeConfig) (*PoolSet, error) {
	logInfo("creating new CPU pool set")

	var ps *PoolSet = &PoolSet{
		pools:      make(map[string]*Pool),
		containers: make(map[string]*Container),
		stats:      poolcache.GetCPUPoolCache(),
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
			return fmt.Errorf("missing %s pool", name)
		}
	}

	return nil
}

// Check the given configuration for obvious errors.
func (ps *PoolSet) checkConfig(cfg NodeConfig) error {
	allCPUs  := ps.topology.CPUDetails.CPUs()
	numCPUs  := allCPUs.Size()
	leftover := ""

	for name, c := range cfg {
		if c.Size < 0 {
			leftover = name
			continue
		}

		if c.Size > numCPUs {
			return fmt.Errorf("not enough CPU (%d) left for pool %s (%d)",
				numCPUs, name, c.Size)
		}

		numCPUs -= c.Size
	}

	if leftover != "" {
		cfg[leftover] = &Config{
			Size: numCPUs,
		}
	} else {
		if _, ok := cfg[DefaultPool]; !ok {
			cfg[DefaultPool] = &Config{
				Size: numCPUs,
			}
		}
	}

	return nil
}

// Reconfigure the CPU pool set.
func (ps *PoolSet) Reconfigure(cfg NodeConfig) error {
	if cfg == nil {
		return nil
	}

	if err := ps.checkConfig(cfg); err != nil {
		return err
	}

	logInfo("reconfiguring CPU pools with %v", cfg)

	// update configuration for existing pools
	for name, c := range cfg {
		if p, ok := ps.pools[name]; !ok {
			ps.pools[name] = &Pool{
				shared: cpuset.NewCPUSet(),
				pinned: cpuset.NewCPUSet(),
				cfg:    c,
			}
		} else {
			p.cfg = c
		}
	
		logInfo("configured pool %s: %v", name, ps.pools[name].cfg)
	}

	// mark removed pools for removal
	for name, p := range ps.pools {
		if name == ReservedPool || name == DefaultPool {
			continue
		}
		if _, ok := cfg[name]; !ok {
			p.cfg = nil
		}
	}

	ps.reconcile = true
	_, err := ps.ReconcileConfig()

	return err
}

// Is pool idle ?
func (p *Pool) isIdle() bool {
	if p.used == 0 && p.pinned.IsEmpty() {
		return true
	}

	return false
}

// Is pool pinned ?
func (p *Pool) isPinned() bool {
	if p.cfg != nil && p.cfg.Cpus != nil {
		return true
	}

	return false
}

// Is pool removable ?
func (p *Pool) isRemovable() bool {
	if p.cfg == nil {
		return true
	} else {
		return false
	}
}

// Is pool shrinkable ?
func (p *Pool) isShrinkable() bool {
	if p.cfg == nil {
		return true
	}

	size := p.shared.Size() * 1000
	used := int(p.used)

	if size - used > 1000 {
		return true
	}

	return false
}

// Is pool up-to-date ?
func (p *Pool) isUptodate() bool {
	if p.cfg == nil {
		return false
	}

	if p.cfg.Cpus != nil {
		return p.cfg.Cpus.Equals(p.shared.Union(p.pinned))
	}

	if p.cfg.Size == p.shared.Union(p.pinned).Size() {
		return true
	}

	return false
}

// Remove up idle pools which have been removed from the configuration.
func (ps *PoolSet) removeIdlePools(unused cpuset.CPUSet) cpuset.CPUSet {
	logInfo("removing unused idle pools...")

	for name, p := range ps.pools {
		if name == ReservedPool || name == DefaultPool || !p.isRemovable() {
			continue
		}

		unused = unused.Union(p.shared)
		delete(ps.pools, name)

		logInfo("   removed unused idle pool '%s'", name)
	}

	return unused
}

// Shrink oversized pools.
func (ps *PoolSet) shrinkPools(unused cpuset.CPUSet) cpuset.CPUSet {
	logInfo("shrinking pools...")

	for name, p := range ps.pools {
		if !p.isShrinkable() {
			continue
		}

		// try to free some of the shared CPUs
		size  := p.shared.Size() * 1000
		used  := int(p.used)
		extra := (size - used) / 1000

		if extra <= 0 {
			continue
		}

		size = p.shared.Size() - extra
		shrunk, _ := ps.allocfn(ps.topology, p.shared, size)

		unused = unused.Union(p.shared.Difference(shrunk))

		logInfo("shrunk pool %s (shared %s -> %s)", name,
			p.shared.String(), shrunk.String())

		p.shared = shrunk
	}

	return unused
}

// Allocate reserved pool.
func (ps *PoolSet) allocateReservedPool(free cpuset.CPUSet) cpuset.CPUSet {
	r := ps.pools[ReservedPool]

	if r.cfg.Cpus != nil && !r.cfg.Cpus.Intersection(free).IsEmpty() {
		cset := r.cfg.Cpus.Intersection(free)
		free = free.Difference(cset).Union(r.shared)
		r.shared = cset

		if more := r.cfg.Size - r.shared.Size(); more > 0 {
			if more >= free.Size() {
				r.shared = r.shared.Union(free)
				free = cpuset.NewCPUSet()
			} else {
				cset, _ = ps.allocfn(ps.topology, free, more)
				r.shared = r.shared.Union(cset)
				free = free.Difference(cset)
			}
		}
	} else {
		if more := r.cfg.Size - r.shared.Size(); more > 0 {
			if more >= free.Size() {
				r.shared = r.shared.Union(free)
				free = cpuset.NewCPUSet()
			} else {
				cset, _ := ps.allocfn(ps.topology, free, more)
				r.shared = r.shared.Union(cset)
				free = free.Difference(cset)
			}
		}
	}

	logInfo("allocated reserved pool: %s, free set now: %s",
		r.shared.Union(r.pinned).String(), free.String())

	if r.shared.Union(r.pinned).Size() < r.cfg.Size {
		logError("reserved pool has insufficient cpus %s (need %d)",
			r.shared.Union(r.pinned).String(), r.cfg.Size)
	}

	return free
}

// Allocate pools specified by explicit CPU ids.
func (ps *PoolSet) allocatePinnedPools(free cpuset.CPUSet) cpuset.CPUSet {
	logInfo("allocating pinned pools...")

	for name, p := range ps.pools {
		if p.isUptodate() || !p.isPinned() {
			continue
		}

		logInfo("pinned pool %s...", name)

		avail := p.cfg.Cpus.Intersection(free)
		if avail.IsEmpty() {
			continue
		}

		p.shared = p.shared.Union(avail)
		free = free.Difference(avail)

		logInfo("  pool %s: added CPUs %s", name, avail.String())
	}

	return free
}

// Allocate pool specificed by size.
func (ps *PoolSet) allocateSizedPools(free cpuset.CPUSet) cpuset.CPUSet {
	logInfo("allocating unpinned pools (from set %s)...", free.String())

	for _, p := range ps.pools {
		if p.isUptodate() || p.isPinned() {
			continue
		}

		more := p.cfg.Size - (p.shared.Size() + p.pinned.Size())

		if more <= 0 {
			continue
		}

		if more > free.Size() {
			more = free.Size()
		}

		cpus, _ := ps.allocfn(ps.topology, free, more)
		p.shared = p.shared.Union(cpus)
		free = free.Difference(cpus)
	}

	return free
}

// Get the full set of CPUs in the pool set.
func (ps *PoolSet) freeCPUs() cpuset.CPUSet {
	cpus := ps.topology.CPUDetails.CPUs()
	for _, p := range ps.pools {
		cpus = cpus.Difference(p.shared.Union(p.pinned))
	}

	return cpus
}

// Run one round of reconcilation of the CPU pool set configuration.
func (ps *PoolSet) ReconcileConfig() (bool, error) {
	if !ps.reconcile {
		logInfo("pools configuration is up to date")
		return false, nil
	}

	logInfo("pool configuration needs reconcilation...")

	free := ps.freeCPUs()

	// remove uneccesary pools
	free = ps.removeIdlePools(free)

	// shrink oversized pools
	free = ps.shrinkPools(free)

	// allocate the reserved pool
	free = ps.allocateReservedPool(free)

	// allocate pinned pools
	free = ps.allocatePinnedPools(free)

	// allocate unpinned pools
	free = ps.allocateSizedPools(free)

	// slam any remaining CPUs to the default pool
	if !free.IsEmpty() {
		def := ps.pools[DefaultPool]
		def.shared = def.shared.Union(free)
	}

	ps.reconcile = false

	logInfo("reconciled pool state:")
	for name, p := range ps.pools {
		ps.reconcile = ps.reconcile || !p.isUptodate()

		logInfo("* pool %s:", name)
		logInfo("   - cpus: shared %s, exclusive %s",
			p.shared.String(), p.pinned.String())
		if p.cfg == nil {
			logInfo("   - scheduled for removal")
		} else {
			if p.cfg.Cpus != nil {
				logInfo("   - cfg: %s", p.cfg.Cpus.String())
			} else {
				logInfo("   - cfg: %d cpus", p.cfg.Size)
			}
		}
	}

	if ps.reconcile {
		logInfo("pool set needs further reconcilation")
	} else {
		logInfo("pool set now up to date")
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
		return fmt.Errorf("can't allocate from pool %s", pool)
	} else {
		return nil
	}
}

// Allocate a number of CPUs exclusively from a pool.
func (ps *PoolSet) AllocateCPUs(id string, pool string, numCPUs int) (cpuset.CPUSet, error) {
	if pool == ReservedPool {
		return ps.AllocateCPU(id, pool, int64(numCPUs * 1000))
	}

	if pool == "" {
		pool = DefaultPool
	}

	if err := checkAllowedPool(pool); err != nil {
		return cpuset.NewCPUSet(), err
	}

	p, ok := ps.pools[pool]
	if !ok {
		return cpuset.NewCPUSet(), fmt.Errorf("non-existent pool %s", pool)
	}

	cpus, err := ps.allocfn(ps.topology, p.shared, numCPUs)
	if err != nil {
		return cpuset.NewCPUSet(), err
	}

	p.shared = p.shared.Difference(cpus)
	p.pinned = p.pinned.Union(cpus)

	ps.containers[id] = &Container{
		id:   id,
		pool: pool,
		cpus: cpus,
		req:  int64(cpus.Size()) * 1000,
	}

	logInfo("allocated %s/CPU:%s for container %s", pool, cpus.String(), id)

	return cpus.Clone(), nil
}

// Allocate CPU for a container from a pool.
func (ps *PoolSet) AllocateCPU(id string, pool string, req int64) (cpuset.CPUSet, error) {
	var cpus cpuset.CPUSet

	if err := checkAllowedPool(pool); err != nil {
		return cpuset.NewCPUSet(), nil
	}

	p, ok := ps.pools[pool]
	if !ok {
		return cpuset.NewCPUSet(), fmt.Errorf("pool %s not found", pool)
	}

	ps.containers[id] = &Container{
		id:   id,
		pool: pool,
		cpus: cpuset.NewCPUSet(),
		req:  req,
	}

	p.used += req

	logInfo("allocated %dm of %s/CPU:%s for container %s", req, pool, p.shared.String(), id)

	return cpus, nil
}

// Return CPU from a container to a pool.
func (ps *PoolSet) ReleaseCPU(id string) {
	c, ok := ps.containers[id]
	if !ok {
		logWarning("couldn't find allocations for container %s", id)
		return
	}

	delete(ps.containers, id)

	p, ok := ps.pools[c.pool]
	if !ok {
		logWarning("couldn't find pool %s for container %s", c.pool, id)
		return
	}

	if c.cpus.IsEmpty() {
		p.used -= c.req
		logInfo("cpumanager] released %dm of %s/CPU:%s for container %s", c.req, c.pool, p.shared.String(), c.id)
	} else {
		p.shared    = p.shared.Union(c.cpus)
		p.pinned = p.pinned.Difference(c.cpus)

		logInfo("released %s/CPU:%s for container %s", c.pool, p.shared.String(), c.id)
	}

	ps.ReconcileConfig()
}

// Get the name of the CPU pool a container is assigned to.
func (ps *PoolSet) GetContainerPoolName(id string) (string) {
	if c, ok := ps.containers[id]; ok {
		return c.pool
	}
	return ""
}

// Get the CPU capacity of pools.
func (ps *PoolSet) GetPoolCapacity() v1.ResourceList {
	cap := v1.ResourceList{}

	for name, p := range ps.pools {
		qty := 1000 * (p.shared.Size() + p.pinned.Size())
		res := v1.ResourceName(ResourcePrefix + name)
		cap[res] = *resource.NewQuantity(int64(qty), resource.DecimalSI)
	}

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

	if !c.cpus.IsEmpty() {
		return c.cpus.Clone(), true
	} else {
		if c.pool == ReservedPool || c.pool == DefaultPool {
			r := ps.pools[ReservedPool]
			d := ps.pools[DefaultPool]
			return r.shared.Union(d.shared), true
		} else {
			p := ps.pools[c.pool]
			return p.shared.Clone(), true
		}
	}
}

// Get the shared CPUs of a pool.
func (ps *PoolSet) GetPoolCPUSet(pool string) (cpuset.CPUSet, bool) {
	p, ok := ps.pools[pool]
	if !ok {
		return cpuset.NewCPUSet(), false
	}

	if pool == DefaultPool || pool == ReservedPool {
		return ps.pools[DefaultPool].shared.Union(ps.pools[ReservedPool].shared), true
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

// Get metrics for the given pool.
func (ps *PoolSet) getPoolMetrics(pool string) (string, cpuset.CPUSet, cpuset.CPUSet, int64, int64) {
	if _, ok := ps.pools[pool]; ok && pool != ReservedPool {
		c, s, e := ps.getPoolCPUSets(pool)
		u := ps.getPoolUsage(pool)

		return pool, s, e, c, u
	}

	return "", cpuset.NewCPUSet(), cpuset.NewCPUSet(), 0, 0
}

// Get the shared and exclusive CPUs and the total capacity for the given pool.
func (ps *PoolSet) getPoolCPUSets(pool string) (int64, cpuset.CPUSet, cpuset.CPUSet) {
	var s, e cpuset.CPUSet

	if p, ok := ps.pools[pool]; ok {
		s = p.shared.Clone()
		e = p.pinned.Clone()
	} else {
		s = cpuset.NewCPUSet()
		e = cpuset.NewCPUSet()
	}

	return int64(1000 * (s.Size() + e.Size())), s, e
}

// Get the total CPU allocations for the given pool (in MilliCPUs).
func (ps *PoolSet) getPoolUsage(pool string) int64 {
	p := ps.pools[pool]

	return int64(1000 * int64(p.pinned.Size()) + p.used)
}

// Get the total size of a pool (in CPUs).
func (ps *PoolSet) getPoolSize(pool string) int {
	p := ps.pools[pool]

	return p.shared.Size() + p.pinned.Size()
}

// Get the total CPU capacity for the given pool (in MilliCPUs).
func (ps *PoolSet) getPoolCapacity(pool string) int64 {
	p := ps.pools[pool]
	n := p.shared.Size() + p.pinned.Size()

	return int64(1000 * n)
}

// Update pool metrics.
func (ps *PoolSet) updatePoolMetrics(pool string) {
	if name, s, e, c, u := ps.getPoolMetrics(pool); name != "" {
		ps.stats.UpdatePool(name, s, e, c, u)
	}
}

//
// errors and logging
//

func logFormat(format string, args... interface{}) string {
	return fmt.Sprintf(logPrefix + format, args...)
}

func logVerbose(level glog.Level, format string, args... interface{}) {
	glog.V(level).Infof(logFormat(logPrefix + format, args...))
}

func logInfo(format string, args... interface{}) {
	glog.Info(logFormat(format, args...))
}

func logWarning(format string, args... interface{}) {
	glog.Warningf(logFormat(format, args...))
}

func logError(format string, args... interface{}) {
	glog.Errorf(logFormat(format, args...))
}

func logFatal(format string, args... interface{}) {
	glog.Fatalf(logFormat(format, args...))
}



//
// JSON marshalling and unmarshalling
//

// Container JSON marshalling interface
type marshalContainer struct {
	Id   string        `json:"id"`
	Pool string        `json:"pool"`
	Cpus cpuset.CPUSet `json:"cpus"`
	Req  int64         `json:"req"`
}

func (pc Container) MarshalJSON() ([]byte, error) {
	return json.Marshal(marshalContainer{
		Id:   pc.id,
		Pool: pc.pool,
		Cpus: pc.cpus,
		Req:  pc.req,
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
	pc.req  = m.Req

	return nil
}

// Pool JSON marshalling interface
type marshalPool struct {
	Shared  cpuset.CPUSet `json:"shared"`
	Pinned  cpuset.CPUSet `json:"exclusive"`
	Used    int64         `json:"used"`
	Cfg    *Config        `json:"cfg,omitempty"`
}

func (p Pool) MarshalJSON() ([]byte, error) {
	return json.Marshal(marshalPool{
		Shared: p.shared,
		Pinned: p.pinned,
		Used:   p.used,
		Cfg:    p.cfg,
	})
}

func (p *Pool) UnmarshalJSON(b []byte) error {
	var m marshalPool

	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	p.shared = m.Shared
	p.pinned = m.Pinned
	p.used   = m.Used
	p.cfg    = m.Cfg

	return nil
}

// PoolSet JSON marshalling interface
type marshalPoolSet struct {
	Pools      map[string]*Pool      `json:"pools"`
	Containers map[string]*Container `json:"containers"`
	Reconcile  bool                  `json:"reconcile"`
}

func (ps PoolSet) MarshalJSON() ([]byte, error) {
	return json.Marshal(marshalPoolSet{
		Pools:      ps.pools,
		Containers: ps.containers,
		Reconcile:  ps.reconcile,
	})
}

func (ps *PoolSet) UnmarshalJSON(b []byte) error {
	var m marshalPoolSet

	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	ps.pools      = m.Pools
	ps.containers = m.Containers
	ps.reconcile  = m.Reconcile
	ps.stats      = poolcache.GetCPUPoolCache()

	return nil
}
