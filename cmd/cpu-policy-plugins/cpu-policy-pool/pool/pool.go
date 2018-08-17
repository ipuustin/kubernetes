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
// The pool policy maintains a set of CPU pools to allocate CPU resources to
// containers. The pools are configured externally. Pods request CPU from a
// particular pool explicitly by requesting a corresponding external resource
// unique to the pool, or actually to the set of all pools with the same name
// on different nodes).
//
// There is a number of pre-defined pools which special semantics:
//
//  - ignored:
//    CPUs in this pool are ignored. They can be used outside of kubernetes.
//    Allocations are not allowed from this pool.
//
//  - offline:
//    CPUs in this pool are taken offline (typically to disable hyperthreading
//    for sibling cores). This pool is only used to administer the offline CPUs,
//    allocations are not allowed from this pool.
//
//  - reserved:
//    The reserved pool is the set of CPUs dedicated to system- and kube-
//    reserved pods and other processes.
//
//  - default:
//    Pods which do not request CPU from any particular pool by name are allocated
//    CPU from the default pool. Also, any CPU not assigned to any other pool is
//    automatically assigned to the default pool.
//
// Currently there is no difference in practice between the ignored and offline
// pools, since the actual offlining of CPUs is handled by an external component.
//

package pool

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kubeapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

const (
	logPrefix      = "[cpu-policy/pool] " // log message prefix
	ResourcePrefix = "intel.com/cpupool." // prefix for CPU pool resources
	IgnoredPool    = "ignored"            // CPUs we have to ignore
	OfflinePool    = "offline"            // CPUs which are offline
	ReservedPool   = "reserved"           // CPUs reserved for kube and system
	DefaultPool    = "default"            // CPUs in the default set
)

// CPUFlags has the CPU allocation flags
type CPUFlags int

const (
	AllocShared    CPUFlags = 0x00 // allocate to shared set in pool
	AllocExclusive CPUFlags = 0x01 // allocate exclusively in pool
	KubePinned     CPUFlags = 0x00 // we take care of CPU pinning
	WorkloadPinned CPUFlags = 0x02 // workload takes care of CPU pinning
	DefaultFlags   CPUFlags = AllocShared | KubePinned
)

// Configuration for a single CPU pool.
type Config struct {
	Size int            `json:"size"`           // number of CPUs to allocate
	Cpus *cpuset.CPUSet `json:"cpus,omitempty"` // explicit CPUs to allocate, if given
}

// Node CPU pool configuration.
type NodeConfig map[string]*Config

// A container assigned to run in a pool.
type Container struct {
	id   string        // container ID
	pool string        // assigned pool
	cpus cpuset.CPUSet // exclusive CPUs, if any
	req  int64         // requested milliCPUs
}

// A CPU pool is a set of cores, typically set aside for a class of workloads.
type Pool struct {
	shared cpuset.CPUSet // shared set of CPUs
	pinned cpuset.CPUSet // exclusively allocated CPUs
	used   int64         // total allocations in shared set
	cfg    *Config       // (requested) configuration
}

// A CPU allocator function.
type AllocCPUFunc func(*topology.CPUTopology, cpuset.CPUSet, int) (cpuset.CPUSet, error)

// All pools available for kube on this node.
type PoolSet struct {
	pools      map[string]*Pool      // all CPU pools
	containers map[string]*Container // containers assignments
	topology   *topology.CPUTopology // CPU topology info
	allocfn    AllocCPUFunc          // CPU allocator function
	free       cpuset.CPUSet         // free CPUs
	reconcile  bool                  // whether needs reconcilation
}

// Create default node CPU pool configuration.
func DefaultNodeConfig(numReservedCPUs int, cpuPoolConfig map[string]string) (NodeConfig, error) {
	nc := make(NodeConfig)

	for pool, cfg := range cpuPoolConfig {
		if pool != ReservedPool && pool != DefaultPool {
			return NodeConfig{}, fmt.Errorf("default config, invalid pool %s (%s)", pool, cfg)
		}

		if err := nc.setPoolConfig(pool, cfg); err != nil {
			return NodeConfig{}, err
		}
	}

	if err := nc.setCPUCount(ReservedPool, fmt.Sprintf("@%d", numReservedCPUs)); err != nil {
		return NodeConfig{}, err
	}

	if err := nc.claimLeftoverCpus(DefaultPool); err != nil {
		return NodeConfig{}, err
	}

	return nc, nil
}

func ParseNodeConfig(numReservedCPUs int, path string) (NodeConfig, error) {
	files, err := ioutil.ReadDir(path)

	// maybe no-one loaded a ConfigMap for us?
	if err != nil || len(files) == 0 {
		return DefaultNodeConfig(numReservedCPUs, nil)
	}

	nc := make(NodeConfig)

	for _, file := range files {
		data, err := ioutil.ReadFile(file.Name())
		if err != nil {
			logWarning("Could not read file %s", file.Name())
			return NodeConfig{}, err
		}
		if err := nc.setPoolConfig(file.Name(), string(data[:])); err != nil {
			return NodeConfig{}, err
		}
	}

	if err := nc.setCPUCount(ReservedPool, fmt.Sprintf("@%d", numReservedCPUs)); err != nil {
		return NodeConfig{}, err
	}

	return nc, nil
}

// Dump node CPU pool configuration as string.
func (nc NodeConfig) String() string {
	if nc == nil {
		return "{}"
	}

	str := "{ "
	for pool, cfg := range nc {
		str += fmt.Sprintf("%s: %s, ", pool, cfg.String())
	}
	str += "}"

	return str
}

// Configure the given pool with the given configuration.
func (nc NodeConfig) setPoolConfig(pool string, cfg string) error {
	if _, ok := nc[pool]; ok {
		return fmt.Errorf("invalid configuration, multiple entries for pool %s", pool)
	}

	if cfg[0:1] == "@" {
		return nc.setCPUCount(pool, cfg)
	} else if cfg != "*" {
		return nc.setCPUIds(pool, cfg)
	} else /* if cfg == "*" */ {
		return nc.claimLeftoverCpus(pool)
	}
}

// Configure the given pool with a given number of CPUs.
func (nc NodeConfig) setCPUCount(pool string, cfg string) error {

	if pool == IgnoredPool || pool == OfflinePool {
		return fmt.Errorf("pool %s cannot be configured with CPU count", pool)
	}

	cnt, err := strconv.Atoi(cfg[1:])
	if err != nil {
		return err
	}

	if c, ok := nc[pool]; !ok {
		nc[pool] = &Config{
			Size: cnt,
		}
	} else {
		if c.Cpus != nil && c.Cpus.Size() != cnt {
			return fmt.Errorf("inconsistent configuration: CPUs %s, count: %d",
				c.Cpus.String(), cnt)
		}
	}

	return nil
}

// Configure the given pool with a given set of CPUs.
func (nc NodeConfig) setCPUIds(pool string, cfg string) error {
	cset, err := cpuset.Parse(cfg)

	if err != nil {
		return fmt.Errorf("invalid configuration for pool %s (%v)", pool, err)
	}

	nc[pool] = &Config{
		Size: cset.Size(),
		Cpus: &cset,
	}

	return nil
}

// Configure the given pool for claiming any leftover/unused CPUs.
func (nc NodeConfig) claimLeftoverCpus(pool string) error {
	if pool == IgnoredPool || pool == OfflinePool {
		return fmt.Errorf("pool %s cannot be configured with leftover CPUs", pool)
	}

	for name, cfg := range nc {
		if cfg.Size == -1 && name != pool {
			return fmt.Errorf("invalid configuration, multiple wildcard pools (%s, %s)",
				name, pool)
		}
	}

	nc[pool] = &Config{
		Size: -1,
	}

	return nil
}

// Dump a configuration as a string.
func (cfg *Config) String() string {
	if cfg == nil {
		return "<to be removed>"
	}

	if cfg.Cpus != nil {
		return fmt.Sprintf("<CPU#%s>", cfg.Cpus.String())
	}

	return fmt.Sprintf("<any %d CPUs>", cfg.Size)
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

	for name := range c.Resources.Requests {
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

// Dump a pool as a string.
func (p *Pool) String() string {
	if p == nil {
		return "<nil pool>"
	}

	var shared, pinned string = "-", "-"

	if !p.shared.IsEmpty() {
		shared = "CPU#" + p.shared.String()
	}

	if !p.pinned.IsEmpty() {
		pinned = "CPU#" + p.pinned.String()
	}

	return fmt.Sprintf("<shared: %s, pinned: %s, cfg: %s>", shared, pinned, p.cfg.String())
}

// Create a new CPU pool set with the given configuration.
func NewPoolSet(cfg NodeConfig) (*PoolSet, error) {
	logInfo("creating new CPU pool set")

	var ps = &PoolSet{
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
	required := []string{ReservedPool, DefaultPool}

	for _, name := range required {
		if _, ok := ps.pools[name]; !ok {
			return fmt.Errorf("missing %s pool", name)
		}
	}

	return nil
}

// Check the given configuration for obvious errors.
func (ps *PoolSet) checkConfig(cfg NodeConfig) error {
	allCPUs := ps.topology.CPUDetails.CPUs()
	numCPUs := allCPUs.Size()
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

// Check if the pool set is up to date wrt. the configuration.
func (ps *PoolSet) isUptodate() bool {
	for _, p := range ps.pools {
		if !p.isUptodate() {
			return false
		}
	}

	return true
}

// Reconfigure the CPU pool set.
func (ps *PoolSet) Reconfigure(cfg NodeConfig) error {
	if cfg == nil {
		return nil
	}

	if err := ps.checkConfig(cfg); err != nil {
		return err
	}

	// configure new pools, update existing ones
	for pool, c := range cfg {
		if p, ok := ps.pools[pool]; !ok {
			ps.pools[pool] = &Pool{
				shared: cpuset.NewCPUSet(),
				pinned: cpuset.NewCPUSet(),
				cfg:    c,
			}
		} else {
			p.cfg = c
		}

		logInfo("pool %s configured to %s", pool, c.String())
	}

	// mark removed pools for removal
	for pool, p := range ps.pools {
		if pool == ReservedPool || pool == DefaultPool {
			continue
		}
		if _, ok := cfg[pool]; !ok {
			p.cfg = nil
		}
	}

	if err := ps.ReconcileConfig(); err != nil {
		return err
	}

	// make sure we update pool metrics upon startup
	ps.updateMetrics()
	return nil
}

// Is pool pinned ?
func (p *Pool) isPinned() bool {
	if p.cfg != nil && p.cfg.Cpus != nil {
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

// Calculate the shrinkable capacity of a pool.
func (ps *PoolSet) freeCapacity(pool string) int {
	p, ok := ps.pools[pool]
	if ok {
		return 1000*p.shared.Size() - int(p.used)
	}
	return 0
}

// Is the given pool marked for removal ?
func (ps *PoolSet) isRemoved(pool string) bool {
	p, ok := ps.pools[pool]
	if !ok {
		return false
	}
	return p.cfg == nil
}

// Is the given pool idle ?
func (ps *PoolSet) isIdle(pool string) bool {
	p, ok := ps.pools[pool]
	if !ok {
		return false
	}
	return p.used == 0 && p.pinned.IsEmpty()
}

// Remove the given (assumed to be idle) pool.
func (ps *PoolSet) removePool(pool string) {
	if p, ok := ps.pools[pool]; ok {
		ps.free = ps.free.Union(p.shared)
		delete(ps.pools, pool)
	}
}

// Shrink a pool to its minimum possible size.
func (ps *PoolSet) trimPool(pool string) bool {
	free := ps.freeCapacity(pool) / 1000
	if free < 1 {
		return false
	}

	p, _ := ps.pools[pool]
	if _, err := ps.freeCPUs(&p.shared, &ps.free, free); err != nil {
		logWarning("failed to shrink pool %s by %d CPUs", pool, free)
		return false
	}

	logInfo("pool %s: trimmed by %d CPUs", pool, free)

	return true
}

// Allocate reserved pool.
func (ps *PoolSet) allocateReservedPool() {
	r := ps.pools[ReservedPool]

	if r.cfg.Cpus != nil && !r.cfg.Cpus.Intersection(ps.free).IsEmpty() {
		cset := r.cfg.Cpus.Intersection(ps.free)
		ps.free = ps.free.Difference(cset).Union(r.shared)
		r.shared = cset
	}

	if more := r.cfg.Size - r.shared.Size(); more > 0 {
		ps.takeCPUs(&ps.free, &r.shared, more)
	}

	logInfo("pool %s: allocated CPU#%s (%d)", ReservedPool,
		r.shared.String(), r.shared.Size())

	if r.shared.Size() < r.cfg.Size {
		logError("pool %s: insufficient cpus %s (need %d)", ReservedPool,
			r.shared.String(), r.cfg.Size)
	}
}

// Allocate pools specified by explicit CPU ids.
func (ps *PoolSet) allocateByCPUId() {
	for pool, p := range ps.pools {
		if ps.isRemoved(pool) {
			continue
		}

		if !p.isPinned() || p.isUptodate() {
			continue
		}

		if cpus := p.cfg.Cpus.Intersection(ps.free); !cpus.IsEmpty() {
			p.shared = p.shared.Union(cpus)
			ps.free = ps.free.Difference(cpus)

			logInfo("pool %s: allocated requested CPU#%s (%d)", pool,
				cpus.String(), cpus.Size())
		}
	}
}

// Allocate pools specified by size.
func (ps *PoolSet) allocateByCPUCount() {
	for pool, p := range ps.pools {
		if ps.isRemoved(pool) {
			continue
		}

		if p.isPinned() || p.isUptodate() {
			continue
		}

		cnt := p.cfg.Size - (p.shared.Size() + p.pinned.Size())
		cpus, _ := ps.takeCPUs(&ps.free, &p.shared, cnt)

		logInfo("pool %s: allocated available CPU#%s (%d)", pool,
			cpus.String(), cpus.Size())
	}
}

// Allocate any remaining unused CPUs to the given pool.
func (ps *PoolSet) claimLeftoverCPUs(pool string) {
	p, ok := ps.pools[pool]
	if !ok || ps.free.IsEmpty() {
		return
	}
	p.shared = p.shared.Union(ps.free)
	logInfo("pool %s: claimed leftover CPU#%s (%d)", pool,
		ps.free.String(), ps.free.Size())
	ps.free = cpuset.NewCPUSet()
}

// Get the full set of CPUs in the pool set.
func (ps *PoolSet) getFreeCPUs() {
	ps.free = ps.topology.CPUDetails.CPUs()
	for _, p := range ps.pools {
		ps.free = ps.free.Difference(p.shared.Union(p.pinned))
	}
}

// Run one round of reconcilation of the CPU pool set configuration.
func (ps *PoolSet) ReconcileConfig() error {
	// check if everything is up-to-date
	if ps.reconcile = !ps.isUptodate(); !ps.reconcile {
		logInfo("pools already up-to-date, nothing to reconcile")
		return nil
	}

	logInfo("CPU pools not up-to-date, reconciling...")

	//
	// Our pool reconcilation algorithm is:
	//
	//   1. update list of free CPUs
	//   2. trim pools (removing unused idle ones)
	//   3. allocate the reserved pool
	//   4. allocate pools configured with specific CPUs
	//   5. allocate pools configured by total CPU count
	//   6. slam any remaining CPUs to the default pool
	//
	// Check the pool allocations vs. configuration and if
	// everything adds up, mark the pool set as reconciled.
	// Update pool metrics at the same time.
	//

	ps.getFreeCPUs()

	for pool := range ps.pools {
		if ps.isRemoved(pool) && ps.isIdle(pool) {
			ps.removePool(pool)
		} else {
			ps.trimPool(pool)
		}
	}

	ps.allocateReservedPool()
	ps.allocateByCPUId()
	ps.allocateByCPUCount()
	ps.claimLeftoverCPUs(DefaultPool)

	ps.reconcile = false
	for pool, p := range ps.pools {
		ps.updatePoolMetrics(pool)
		if !p.isUptodate() {
			ps.reconcile = true
		}

		logInfo("pool %s: %s", pool, p.String())
	}

	if !ps.reconcile {
		logInfo("CPU pools are now up-to-date")
	} else {
		logInfo("CPU pools need further reconcilation...")
	}

	return nil
}

// Set the CPU allocator function, and CPU topology information.
func (ps *PoolSet) SetAllocator(allocfn AllocCPUFunc, topo *topology.CPUTopology) {
	ps.allocfn = allocfn
	ps.topology = topo
}

// Take up to cnt CPUs from a given CPU set to another.
func (ps *PoolSet) takeCPUs(from, to *cpuset.CPUSet, cnt int) (cpuset.CPUSet, error) {
	if from == nil {
		from = &ps.free
	}

	if cnt > from.Size() {
		cnt = from.Size()
	}

	if cnt == 0 {
		return cpuset.NewCPUSet(), nil
	}

	cpus, err := ps.allocfn(ps.topology, *from, cnt)
	if err != nil {
		return cpuset.NewCPUSet(), err
	}
	*from = from.Difference(cpus)

	if to != nil {
		*to = to.Union(cpus)
	}

	return cpus, nil
}

// Free up to cnt CPUs from a given CPU set to another.
func (ps *PoolSet) freeCPUs(from, to *cpuset.CPUSet, cnt int) (cpuset.CPUSet, error) {
	if to == nil {
		to = &ps.free
	}

	if cnt > from.Size() {
		cnt = from.Size()
	}

	if cnt == 0 {
		return cpuset.NewCPUSet(), nil
	}

	if keep := from.Size() - cnt; keep > 0 {
		kept, err := ps.allocfn(ps.topology, *from, keep)
		if err != nil {
			return cpuset.NewCPUSet(), err
		}
		cpus := from.Difference(kept)
		*to = to.Union(cpus)
		*from = kept

		return cpus, nil
	}
	cpus := from.Clone()
	*to = to.Union(cpus)
	*from = cpuset.NewCPUSet()

	return cpus, nil
}

// Check it the given pool can be allocated CPUs from.
func isAllowedPool(pool string) error {
	if pool == IgnoredPool || pool == OfflinePool {
		return fmt.Errorf("allocation from pool %s is forbidden", pool)
	}
	return nil
}

// Allocate a number of CPUs exclusively from a pool.
func (ps *PoolSet) AllocateCPUs(id string, pool string, numCPUs int) (cpuset.CPUSet, error) {
	if pool == ReservedPool {
		return ps.AllocateCPU(id, pool, int64(numCPUs*1000))
	}

	if pool == "" {
		pool = DefaultPool
	}

	if err := isAllowedPool(pool); err != nil {
		return cpuset.NewCPUSet(), err
	}

	p, ok := ps.pools[pool]
	if !ok {
		return cpuset.NewCPUSet(), fmt.Errorf("non-existent pool %s", pool)
	}

	cpus, err := ps.takeCPUs(&p.shared, &p.pinned, numCPUs)
	if err != nil {
		return cpuset.NewCPUSet(), err
	}
	ps.containers[id] = &Container{
		id:   id,
		pool: pool,
		cpus: cpus,
		req:  int64(cpus.Size()) * 1000,
	}

	logInfo("gave %s/CPU#%s to container %s", pool, cpus.String(), id)

	return cpus.Clone(), nil
}

// Allocate CPU for a container from a pool.
func (ps *PoolSet) AllocateCPU(id string, pool string, req int64) (cpuset.CPUSet, error) {
	if err := isAllowedPool(pool); err != nil {
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

	logInfo("gave %dm of %s/CPU#%s to container %s", req, pool,
		p.shared.String(), id)

	return p.shared.Clone(), nil
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
		p.shared = p.shared.Union(c.cpus)
		p.pinned = p.pinned.Difference(c.cpus)

		logInfo("released %s/CPU:%s for container %s", c.pool, p.shared.String(), c.id)
	}

	ps.updatePoolMetrics(c.pool)
	ps.ReconcileConfig()
}

// Get the name of the CPU pool a container is assigned to.
func (ps *PoolSet) GetContainerPoolName(id string) string {
	if c, ok := ps.containers[id]; ok {
		return c.pool
	}
	return ""
}

// Get the CPU capacity of pools.
func (ps *PoolSet) GetPoolCapacity() v1.ResourceList {
	cap := v1.ResourceList{}

	for pool, p := range ps.pools {
		qty := 1000 * (p.shared.Size() + p.pinned.Size())
		res := v1.ResourceName(ResourcePrefix + pool)
		cap[res] = *resource.NewQuantity(int64(qty), resource.DecimalSI)
	}

	return cap
}

// Get the (shared) CPU sets for pools.
func (ps *PoolSet) GetPoolCPUs() map[string]cpuset.CPUSet {
	cpus := make(map[string]cpuset.CPUSet)

	for pool, p := range ps.pools {
		cpus[pool] = p.shared.Clone()
	}

	return cpus
}

// Get the exclusively allocated CPU sets.
func (ps *PoolSet) GetPoolAssignments(exclusive bool) map[string]cpuset.CPUSet {
	cpus := make(map[string]cpuset.CPUSet)

	for id, c := range ps.containers {
		if exclusive {
			cpus[id] = c.cpus.Clone()
		} else {
			if !c.cpus.IsEmpty() {
				cpus[id] = c.cpus.Clone()
			} else {
				cpus[id], _ = ps.GetPoolCPUSet(c.pool)
			}
		}
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
	}
	if c.pool == ReservedPool || c.pool == DefaultPool {
		r := ps.pools[ReservedPool]
		d := ps.pools[DefaultPool]
		return r.shared.Union(d.shared), true
	}
	p := ps.pools[c.pool]
	return p.shared.Clone(), true
}

// Get the shared CPUs of a pool.
func (ps *PoolSet) GetPoolCPUSet(pool string) (cpuset.CPUSet, bool) {
	p, ok := ps.pools[pool]
	if !ok {
		return cpuset.NewCPUSet(), false
	}

	if pool == DefaultPool || pool == ReservedPool {
		return ps.pools[DefaultPool].shared.Union(ps.pools[ReservedPool].shared), true
	}
	return p.shared.Clone(), true
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

	return int64(1000*int64(p.pinned.Size()) + p.used)
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

// Update metrics for the given pool.
func (ps *PoolSet) updatePoolMetrics(pool string) {
	/* XXX TODO
	if name, s, e, c, u := ps.getPoolMetrics(pool); name != "" {
		ps.stats.UpdatePool(name, s, e, c, u)
	}
	*/
}

// Update all pool metrics.
func (ps *PoolSet) updateMetrics() {
	for pool := range ps.pools {
		ps.updatePoolMetrics(pool)
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

//
// JSON marshalling and unmarshalling
//

// Container JSON marshalling interface
type marshalContainer struct {
	ID   string        `json:"id"`
	Pool string        `json:"pool"`
	Cpus cpuset.CPUSet `json:"cpus"`
	Req  int64         `json:"req"`
}

func (pc Container) MarshalJSON() ([]byte, error) {
	return json.Marshal(marshalContainer{
		ID:   pc.id,
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

	pc.id = m.ID
	pc.pool = m.Pool
	pc.cpus = m.Cpus
	pc.req = m.Req

	return nil
}

// Pool JSON marshalling interface
type marshalPool struct {
	Shared cpuset.CPUSet `json:"shared"`
	Pinned cpuset.CPUSet `json:"exclusive"`
	Used   int64         `json:"used"`
	Cfg    *Config       `json:"cfg,omitempty"`
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
	p.used = m.Used
	p.cfg = m.Cfg

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

	ps.pools = m.Pools
	ps.containers = m.Containers
	ps.reconcile = m.Reconcile

	return nil
}
