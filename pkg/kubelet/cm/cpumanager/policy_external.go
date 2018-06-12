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
	"context"
	"fmt"
	"sync"

	"github.com/golang/glog"
	"google.golang.org/grpc"
	"k8s.io/api/core/v1"
	cpumanagerapi "k8s.io/kubernetes/pkg/kubelet/apis/cpumanager/v1alpha"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/extended"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
)

// PolicyExternal is the name of the external policy
const PolicyExternal policyName = "external"

var _ Policy = &externalPolicy{}

type externalPolicy struct {
	// which plugin is connected
	pluginName   string
	resourceName string
	nonePolicy   Policy
	c            *grpc.ClientConn
	mutex        *sync.Mutex
	client       cpumanagerapi.CpuManagerPluginClient
	started      bool
	startState   state.State
	config       map[string]string
}

// NewExternalPolicy returns a CPU manager policy.
func NewExternalPolicy(pluginName string, config map[string]string) (Policy, extended.EndpointRegisterCallback) {
	// TODO: add configuration as the parameter -> the configuration is then passed to the plugin when it registers.

	glog.Infof("[cpumanager] initialized external policy '%s'", pluginName)

	p := &externalPolicy{
		pluginName: pluginName,
		nonePolicy: NewNonePolicy(),
		started:    false,
		config:     config,
	}

	return p, p.registerCallback
}

func (p *externalPolicy) Name() string {
	return string(PolicyExternal)
}

func (p *externalPolicy) Start(s state.State) {

	p.startState = s

	if p.mutex == nil || p.client == nil {
		p.nonePolicy.Start(s)
		return
	}

	p.started = true

	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Send the CPU manager plugin configuration to the plugin.
	res, err := p.client.SetConfigData(context.Background(), &cpumanagerapi.ConfigDataRequest{
		State: stateToMsg(s, ""),
	})

	if err != nil {
		glog.Errorf("[cpumanager] failed to set configuration to external policy plugin: '%s'", err)
	}

	shared, err := cpuset.Parse(res.GetSharedCpuSet())

	if err != nil {
		glog.Errorf("[cpumanager] failed to parse shared CPUset data from plugin: '%s'", err)
		return
	}

	s.SetDefaultCPUSet(shared)
}

func (p *externalPolicy) AddContainer(s state.State, pod *v1.Pod, container *v1.Container, containerID string) error {

	if p.mutex == nil || p.client == nil {
		return p.nonePolicy.AddContainer(s, pod, container, containerID)
	}

	p.mutex.Lock()
	defer p.mutex.Unlock()

	res, err := p.client.AddContainer(context.Background(), &cpumanagerapi.AddContainerRequest{
		ContainerId: containerID,
		State:       stateToMsg(s, containerID),
	})

	if err != nil {
		glog.Errorf("[cpumanager] failed to add container to external policy plugin: '%s'", err)
		return err
	}

	// Set the CPUSets according to the result values in the result.

	exclusive, err := cpuset.Parse(res.GetExclusiveCpuSet())

	if err != nil {
		glog.Errorf("[cpumanager] failed to parse exclusive CPUset data from plugin: '%s'", err)
		return err
	}

	shared, err := cpuset.Parse(res.GetSharedCpuSet())

	if err != nil {
		glog.Errorf("[cpumanager] failed to parse shared CPUset data from plugin: '%s'", err)
		return err
	}

	s.SetCPUSet(containerID, exclusive)
	s.SetDefaultCPUSet(shared)

	fmt.Printf("AddContainer result: '%v'", res)

	return nil
}

func (p *externalPolicy) RemoveContainer(s state.State, containerID string) error {

	// FIXME: keep track if the container was registered with none or external
	// policy and call the corresponding RemoveContainer functtion

	if p.mutex == nil || p.client == nil {
		return p.nonePolicy.RemoveContainer(s, containerID)
	}

	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Tell CPU manager plugin that the container was removed.
	res, err := p.client.RemoveContainer(context.Background(), &cpumanagerapi.RemoveContainerRequest{
		ContainerId: containerID,
		State:       stateToMsg(s, containerID),
	})

	if err != nil {
		glog.Errorf("[cpumanager] failed to remove container from external policy plugin: '%s'", err)
		return err
	}

	if res.Error != "" {
		glog.Errorf("[cpumanager] failed to remove container from external policy plugin: '%s'", res.Error)
		return fmt.Errorf(res.Error)
	}

	shared, err := cpuset.Parse(res.GetSharedCpuSet())

	if err != nil {
		glog.Errorf("[cpumanager] failed to parse shared CPUset data from plugin: '%s'", err)
		return err
	}

	s.Delete(containerID)
	s.SetDefaultCPUSet(shared)
	return nil
}

func stateToMsg(s state.State, cid string) *cpumanagerapi.State {

	assignments := make(map[string]string)

	for k, v := range s.GetCPUAssignments() {
		assignments[k] = v.String()
	}

	cpuSet := ""

	maybeCPUSet, ok := s.GetCPUSet(cid)
	if ok {
		cpuSet = maybeCPUSet.String()
	}

	stateMsg := &cpumanagerapi.State{
		Cpuset:          cpuSet,
		Defaultcpuset:   s.GetDefaultCPUSet().String(),
		Cpusetordefault: s.GetCPUSetOrDefault(cid).String(),
		Cpuassignments:  assignments,
	}

	return stateMsg
}

func (p *externalPolicy) registerCallback(resourceName string, c *grpc.ClientConn, mutex *sync.Mutex) error {

	// TODO: think about DM plugin lifecycle. What should happen if this is called twice?

	glog.Infof("[cpumanager] extended policy registration callback for %s", resourceName)

	p.resourceName = resourceName
	p.c = c
	p.mutex = mutex

	p.client = cpumanagerapi.NewCpuManagerPluginClient(c)

	if !p.started {
		p.Start(p.startState)
	}

	return nil
}
