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

package cpupool

import (
	"fmt"
	"strings"
	"io"

	"github.com/golang/glog"
	"k8s.io/apiserver/pkg/admission"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	api "k8s.io/kubernetes/pkg/apis/core"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	kubefeatures "k8s.io/kubernetes/pkg/features"
)

const (
	// plugin name
	PluginName = "CpuPool"
	// resource namespace and prefix of extended resources for CPU pool allocation
	ResourcePrefix = "intel.com/cpupool."
	// predefined pool names
	IgnoredPool  = "ignored"
	OfflinePool  = "offline"
	ReservedPool = "reserved"
	DefaultPool  = "default"
)

// Register registers the plugin
func Register(plugins *admission.Plugins) {
	glog.Infof("[%s] registering plugin", PluginName)

	plugins.Register(PluginName, func(config io.Reader) (admission.Interface, error) {
		return NewCpuPoolPlugin(), nil
	})
}

// CpuPoolPlugin sets up extra resource constraints for pool-based CPU allocation
type CpuPoolPlugin struct {
	*admission.Handler
}

var _ admission.MutationInterface = &CpuPoolPlugin{}
var _ admission.ValidationInterface = &CpuPoolPlugin{}

// Create a new admission controller for CPU pool allocation.
func NewCpuPoolPlugin() *CpuPoolPlugin {
	// hook into resource creation. TODO: should we handle updates as well ?
	return &CpuPoolPlugin{
		Handler: admission.NewHandler(admission.Create /*, admission.Update*/),
	}
}

// Admit enforces, if necessary, extra resource constraints for CPU pool allocation by adding
// extended resource requests which prevent the scheduler for picking a node with enough
// free CPU capacity in the requested CPU pool.
func (p *CpuPoolPlugin) Admit(a admission.Attributes) error {
	if !isPluginEnabled() {
		return nil
	}

	if !shouldHandleOperation(a) {
		return nil
	}

	pod, ok := a.GetObject().(*api.Pod)
	if !ok {
		return apierrors.NewBadRequest(fmt.Sprintf("[%s]: Admit called with wrong resource type", PluginName))
	}

	return p.setupCpuPool(pod)
}

// Validate verifies that the extended CPU pool request is consistent with the core CPU request.
func (p *CpuPoolPlugin) Validate(a admission.Attributes) error {
	if !isPluginEnabled() {
		return nil
	}

	if !shouldHandleOperation(a) {
		return nil
	}

	pod, ok := a.GetObject().(*api.Pod)
	if !ok {
		return apierrors.NewBadRequest(fmt.Sprintf("[%s]: Validate called with wrong resource type", PluginName))
	}

	if err := p.validateCpuPool(pod); err != nil {
		return admission.NewForbidden(a, err)
	}

	return nil
}

// setupCpuPool extends the pod spec with extended resource request for a CPU pool.
func (p *CpuPoolPlugin) setupCpuPool(pod *api.Pod) error {
	// leave system-pods alone, they're supposed to have enough reserved CPU on each node
	//
	// TODO: Maybe we should insert a request for the 'reserved' pool here. After all, that
	//       is what we set up on the nodes based on the CPU kube- and system-reservations.
	if pod.ObjectMeta.Namespace == api.NamespaceSystem {
		return nil
	}

	for i := range pod.Spec.InitContainers {
		if err := addPoolResource(&pod.Spec.InitContainers[i]); err != nil {
			return err
		}
	}

	for i := range pod.Spec.Containers {
		if err := addPoolResource(&pod.Spec.Containers[i]); err != nil {
			return err
		}
	}

	return nil
}

// addPoolResource extends the given container with an extended resource request for a CPU pool.
func addPoolResource(c *api.Container) error {
	var pool, cpu *resource.Quantity = nil, nil

	if c.Resources.Requests == nil {
		return nil
	}

	//
	// Find any native and pool CPU requests, then
	//
	// - if both present, do nothing (will be validated later)
	// - if pool present, add corresponding native
	// - if native present, add corresponding default pool
	//

	if res, ok := c.Resources.Requests[api.ResourceCPU]; ok {
		cpu = &res
	}

	for name, res := range c.Resources.Requests {
		if strings.HasPrefix(name.String(), ResourcePrefix) {
			pool = &res
			break
		}
	}

	if cpu != nil && pool != nil {
		// both native and pool CPU request found
		return nil
	}

	if pool != nil {
		// only pool CPU request, add native
		val := pool.Value()
		cpu = resource.NewMilliQuantity(val, resource.DecimalSI)

		c.Resources.Requests[api.ResourceCPU] = *cpu

		glog.Infof("[%s] requesting native CPU %s = %dm", PluginName, api.ResourceCPU.String(), val)
	} else {
		// only native CPU request, add 'default' pool one
		val := cpu.MilliValue()
		pool = resource.NewQuantity(val, resource.DecimalSI)
		name := api.ResourceName(ResourcePrefix + DefaultPool)

		c.Resources.Requests[name] = *pool

		glog.Infof("[%s] requesting pool CPU %s = %d", PluginName, name.String(), val)
	}

	return nil
}

// ValidateCpuPool validates CPU pool resource requests.
func (p *CpuPoolPlugin) validateCpuPool(pod *api.Pod) error {
	// leave system-pods alone, they're supposed to have enough reserved CPU on each node
	//
	// TODO: If we start inserting requests for the 'reserved' pool, we'll need to start
	//       verifying the presence of that here.
	if pod.ObjectMeta.Namespace == api.NamespaceSystem {
		return nil
	}

	for i := range pod.Spec.InitContainers {
		if err := validatePoolResource(&pod.Spec.Containers[i]); err != nil {
			return err
		}
	}

	for i := range pod.Spec.Containers {
		if err := validatePoolResource(&pod.Spec.Containers[i]); err != nil {
			return err
		}
	}

	return nil
}

// validatePoolResource validates the CPU pool request against the native CPU request.
func validatePoolResource(c *api.Container) error {
	var  pool, cpu *resource.Quantity = nil, nil

	// For the validity check to pass we need to have:
	//  - no pool label, no CPU request, no pool request, or
	//  - no CPU request and a label-matching pool request with value 1, or
	//  - a CPU request and a label-matching pool request with equal value * 1000

	if c.Resources.Requests != nil {
		for name, res := range c.Resources.Requests {
			if name == api.ResourceCPU {
				cpu = &res
			} else if strings.HasPrefix(name.String(), ResourcePrefix) {
				if pool != nil {
					return fmt.Errorf("container %s: multiple CPU pools", c.Name)
				}
				pool = &res
			}
		}
	}

	if pool == nil && cpu == nil {
		return nil
	}

	if pool == nil || cpu == nil {
		return fmt.Errorf("container %s: inconsistent native vs. pool CPU requests", c.Name)
	}

	if cpu.MilliValue() != pool.Value() {
		return fmt.Errorf("container %s: inconsistent native (%d) vs. pool (%d) CPU requests", cpu.MilliValue(), pool.Value())
	}

	return nil
}

// isPluginEnabled checks if our associated feature gate is enabled
func isPluginEnabled() bool {
	return utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUManager)
}

// shouldHandleOperation checks the plugin should act on the given admission operation
func shouldHandleOperation(a admission.Attributes) bool {
	// ignore all calls to subresources or resources othern than pods.
	if a.GetSubresource() != "" || a.GetResource().GroupResource() != api.Resource("pods") {
		return false
	}

	// hook into resource creation. TODO: should we handle updates as well ?
	if a.GetOperation() != admission.Create /*&& a.GetOperation() != admission.Update*/ {
		return false
	}

	return true
}

