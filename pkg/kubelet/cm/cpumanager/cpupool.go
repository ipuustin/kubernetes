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

package cpumanager

import (
	"fmt"
	"os"
	"strings"
	"math"
	"io/ioutil"
	"encoding/json"
	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"

	admission "k8s.io/kubernetes/plugin/pkg/admission/cpupool"
)

// Predefined CPU pool names.
const (
	ReservedPool = "reserved"   // system/kube-reserved cores
	DefaultPool  = "default"    // default set of cores
	OfflinePool  = "offline"    // offline cores
	IgnoredPool  = "ignored"    // ignored cores
)

// CPU allocation modifier flags for workloads.
type CpuFlags int

const (
	CPUShared      CpuFlags = 0x00 // allocate from shared set
	CPUExclusive   CpuFlags = 0x01 // allocate exclusively
	KubePinned     CpuFlags = 0x00 // we take care of pinning
	WorkloadPinned CpuFlags = 0x02 // workload takes care of final pinning
	CPUDefault     CpuFlags = CPUShared | KubePinned
)

// A CPU pool is a set of cores, usually set aside for a particular set of workloads.
type CpuPool struct {
	Shared     cpuset.CPUSet                // shared cores
	Exclusive  cpuset.CPUSet                // exclusively allocated cores
}

// Node CPU pool configuration.
type CpuPoolSetConfig map[string]cpuset.CPUSet

// A workload (pod container) allocated to a pool.
type CpuPoolContainer struct {
	ID   string        // container ID
	Pool string        // associated pool
	CPUs cpuset.CPUSet // exclusive cores if set
	Req  int           // requested milliCPUs
}

// All pools available on a node.
type CpuPoolSet struct {
	Policy      string                          // name of the selected policy
	Active      CpuPoolSetConfig                // active configuration
	Target      CpuPoolSetConfig     `json:"-"` // target configuration
	Pools       map[string]*CpuPool             // all CPU pools
	Topology   *topology.CPUTopology `json:"-"` // CPU topology info
	Containers  map[string]CpuPoolContainer     // allocations for containers
	StateFile   string               `json:"-"` // saved state file path
}

// Create a default configuration, where all CPUs except the reserved ones are in the default pool.
func DefaultCpuPoolSetConfig(reserved int, t *topology.CPUTopology) (CpuPoolSetConfig) {
	var cfg CpuPoolSetConfig = make(CpuPoolSetConfig)

	all := t.CPUDetails.CPUs()
	res, _ := takeByTopology(t, all, reserved)
	def := all.Difference(res)

	cfg[ReservedPool] = res
	cfg[DefaultPool] = def

	return cfg
}

// Parse the given configuration (file or string).
func ParseCpuPoolSetConfig(config string) (CpuPoolSetConfig, error) {
	var data []byte
	var err error
	var tmp map[string]string
	var cfg CpuPoolSetConfig = make(CpuPoolSetConfig)

	if config[0] != '/' && config[0:1] != "./" {
		data = []byte(config)
	} else {
		if data, err = ioutil.ReadFile(config); err != nil {
			return nil, err
		}
	}

	if err = json.Unmarshal(data, &tmp); err != nil {
		return nil, err
	}

	for name, s := range tmp {
		if cfg[name], err = cpuset.Parse(s); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// Create a new CPU pool set.
func NewCpuPoolSet(stateFilePath string, policy string, cfg CpuPoolSetConfig, topology *topology.CPUTopology) (CpuPoolSet, error) {
	var set CpuPoolSet = CpuPoolSet{
		Policy:    policy,
		Topology:  topology,
		StateFile: stateFilePath,
	}

	set.LoadState()
	err := set.Reconfigure(cfg)

	if err == nil {
		set.SaveState()
	}

	return set, err
}

// Verify the configuration of a CPU pool set.
func (set *CpuPoolSet) Verify() error {
	if set.Pools[ReservedPool] == nil {
		return fmt.Errorf("CpuPool: missing reserved pool")
	}
	if set.Pools[DefaultPool] == nil {
		return fmt.Errorf("CpuPool: missing default pool")
	}

	return nil
}

// Reconfigure CPU pool set.
func (set *CpuPoolSet) Reconfigure(cfg CpuPoolSetConfig) error {
	set.Target = cfg
	_, err := set.ReconcileConfig()

	return err
}

// Run one round of reconcilation of the CPU pool set configuration.
func (set *CpuPoolSet) ReconcileConfig() (bool, error) {
	if set.Target == nil {
		return false, nil
	}

	// right now we only handle the trivial initial case

	// trivial case: no active containers
	//   - create new empty pool set
	//   - create new empty container set
	//   - set up pools according to the desired configuration
	if set.Containers == nil || len(set.Containers) == 0 {
		set.Active = set.Target
		set.Target = nil
		set.Pools = make(map[string]*CpuPool)
		set.Containers = make(map[string]CpuPoolContainer)

		for name, cset := range set.Active {
			set.Pools[name] = &CpuPool{
				Shared:     cset.Clone(),
				Exclusive:  cpuset.NewCPUSet(),
			}
		}

		if err := set.Verify(); err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

// Save the state of the CPU pool set to the given file.
func (set *CpuPoolSet) SaveState() {
	var buf []byte
	var err error

	if set.StateFile == "" {
		panic("[cpumanager] cpupool: no state file path given")
	}

	if buf, err = json.Marshal(set); err != nil {
		panic("[cpumanager] cpupool: could not serialize CpuPoolSet to JSON")
	}

	if err = ioutil.WriteFile(set.StateFile, buf, 0644); err != nil {
		panic("[cpumanager] cpupool: failed to write state file")
	}
}

// Load the state of the CPU pool set from the given file.
func (set *CpuPoolSet) LoadState() error {
	var state CpuPoolSet
	var buf []byte
	var err error

	if set.StateFile == "" {
		panic("[cpumanager] cpupool: no state file path given")
	}

	if buf, err = ioutil.ReadFile(set.StateFile); os.IsNotExist(err) {
		if _, err = os.Create(set.StateFile); err != nil {
			panic("[cpumanager] cpupool: failed to create state file")
		}
		return nil
	}

	if err = json.Unmarshal(buf, &state); err != nil {
		glog.Warningf("[cpumanager] cpupool: failed to reload state from \"%s\"", set.StateFile)
		return err
	}

	if state.Policy != set.Policy {
		return fmt.Errorf("restored policy \"%s\" != configured policy \"%s\"", state.Policy, set.Policy)
	}

	set.Active     = state.Active
	set.Pools      = state.Pools
	set.Containers = state.Containers

	return nil
}

// Allocate CPU resources for a container.
func (set *CpuPoolSet) AddContainer(id string, mCPU int, pool string, flags CpuFlags) error {
	var p *CpuPool

	p, ok := set.Pools[pool]
	if !ok {
		return fmt.Errorf("pool \"%s\" not found", pool)
	}

	if flags & CPUExclusive != 0 {
		if mCPU % 1000 != 0 {
			return fmt.Errorf("can't allocate exclusive fractional CPU (%f mCPU)", mCPU)
		}

		ncpu := int(math.Ceil(float64(mCPU) / 1000))

		glog.Infof("[cpumanager] container %s: exclusive %d CPUs from pool %s", id, ncpu, pool)

		cset, err := takeByTopology(set.Topology, p.Shared, ncpu)
		if err != nil {
			return err
		}

		p.Shared = p.Shared.Difference(cset)
		p.Exclusive = p.Exclusive.Union(cset)

		set.Containers[id] = CpuPoolContainer{
			ID:   id,
			Pool: pool,
			CPUs: cset,
			Req:  mCPU,
		}
	} else {
		glog.Infof("[cpumanager] container %s: shared %d mCPU from pool %s", id, mCPU, pool)

		set.Containers[id] = CpuPoolContainer{
			ID:   id,
			Pool: pool,
			CPUs: cpuset.NewCPUSet(),
			Req:  mCPU,
		}
	}

	set.SaveState()

	return nil
}

// Free CPU resources associated with the given container.
func (set *CpuPoolSet) RemoveContainer(id string) {
	glog.Infof("container %s: free allocated CPU");

	c, ok := set.Containers[id]
	if !ok {
		glog.Warningf("[cpumanager] can't find container %s", id)
		return
	}

	delete(set.Containers, id)

	if !c.CPUs.IsEmpty() {
		p, ok := set.Pools[c.Pool]
		if !ok {
			glog.Errorf("[cpumanager] can't find pool \"%s\" for container %s", c.Pool, id)
			set.SaveState()
			return
		}

		glog.Infof("[cpumanager] returning CPUs %s to shared pool %s", c.CPUs.String(), c.Pool)

		p.Shared = p.Shared.Union(c.CPUs)
		p.Exclusive = p.Exclusive.Difference(c.CPUs)
	}

	set.SaveState()
}

// Get the CPUSet for a given container.
func (set *CpuPoolSet) GetContainerCPUSet(id string) cpuset.CPUSet {
	c, ok := set.Containers[id]
	if !ok {
		glog.Errorf("[cpumanager] can't find container %s in CpuPoolSet", id)
		return cpuset.NewCPUSet()
	}

	if !c.CPUs.IsEmpty() {
		glog.Infof("[cpumanager] container %s: dedicated CPUs %s", id, c.CPUs.String())
		return c.CPUs.Clone()
	}

	p, ok := set.Pools[c.Pool]
	if !ok {
		glog.Errorf("[cpumanager] can't find pool %s for container %s", c.Pool, id)
		return cpuset.NewCPUSet()
	}

	cset := p.Shared.Clone()

	if c.Pool == DefaultPool {
		r, ok := set.Pools[ReservedPool]

		if !ok {
			glog.Errorf("[cpumanager] can't find pool %s for container %s", c.Pool, id)
			return cpuset.NewCPUSet()
		}

		cset = cset.Union(r.Shared)
	}

	glog.Infof("[cpumanager] container %s: shared CPUs %s", id, cset.String())
	return cset
}

// Dig out the container CPU request (in milliCPUs) and pool.
func ContainerCpuRequest(c *v1.Container) (int, string) {
	var pool string = DefaultPool
	var prefix string = string(admission.ResourcePrefix) + "."
	var mCPU int

	if req, ok := c.Resources.Requests[v1.ResourceCPU]; ok {
		mCPU = int(req.MilliValue())
	}

	if c.Resources.Requests != nil {
		for name, req := range c.Resources.Requests {
			if strings.HasPrefix(string(name), prefix) {
				mCPU = int(req.Value())
				pool = strings.TrimPrefix(string(name), prefix)
				break
			}
		}
	}

	return mCPU, pool
}

