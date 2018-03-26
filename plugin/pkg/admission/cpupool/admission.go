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
	pluginName = "CpuPool"
	// label used to request a particular CPU pool by name
	LabelName = "cpupool"
	// resource namespace and prefix of extended resources for CPU pool allocation
	ResourcePrefix = "intel.com/cpupool"
)

// Register registers the plugin
func Register(plugins *admission.Plugins) {
	glog.Infof("[%s] registering plugin", pluginName)
	plugins.Register(pluginName, func(config io.Reader) (admission.Interface, error) {
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
		return apierrors.NewBadRequest(fmt.Sprintf("[%s]: Admit called with wrong resource type", pluginName))
	}

	return p.setupCpuPoolResource(pod)
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
		return apierrors.NewBadRequest(fmt.Sprintf("[%s]: Validate called with wrong resource type", pluginName))
	}

	if err := p.validateCpuPoolResource(pod); err != nil {
		return admission.NewForbidden(a, err)
	}

	return nil
}

// setupCpuPoolResource extends the pod spec with extended resource request for a CPU pool.
func (p *CpuPoolPlugin) setupCpuPoolResource(pod *api.Pod) error {
	var resname string

	if pool, found := pod.ObjectMeta.Labels[LabelName]; !found {
		return nil
	} else {
		glog.Infof("[%s] requested CPU pool: \"%s\"", pluginName, pool)
		resname = fmt.Sprintf("%s.%s", ResourcePrefix, pool)
	}

	for i := range pod.Spec.InitContainers {
		if err := addPoolResource(&pod.Spec.InitContainers[i], resname); err != nil {
			return err
		}
	}

	for i := range pod.Spec.Containers {
		if err := addPoolResource(&pod.Spec.Containers[i], resname); err != nil {
			return err
		}
	}

	return nil
}

// addPoolResource extends the given container with an extended resource request for a CPU pool.
func addPoolResource(c *api.Container, name string) error {
	var val int64

	req := c.Resources.Requests
	if req == nil {
		req = api.ResourceList{}
		c.Resources.Requests = req
	} else if _, preset := req[api.ResourceName(name)]; preset {
		// If there is a preset pool CPU request, leave it alone (but cross-check it later in
		// the validation phase against the stock CPU request).
		return nil
	}

	// If the pods container has a CPU request, we use that for our pool CPU request. If there
	// is no CPU request, we add the smallest allocatable pool CPU request (1), to guide the
	// scheduler to pick a node with available CPU in the correct pool.
	if cpu, ok := req[api.ResourceCPU]; ok {
		val = cpu.MilliValue()
	} else {
		val = 1
	}

	req[api.ResourceName(name)] = *resource.NewQuantity(val, resource.DecimalSI)

	glog.Infof("[%s] added resource request: %s = %d", pluginName, val, name)

	return nil
}

// Validate any CPU pool extended resources.
func (p *CpuPoolPlugin) validateCpuPoolResource(pod *api.Pod) error {
	var resname string

	if pool, ok := pod.ObjectMeta.Labels[LabelName]; ok {
		resname = fmt.Sprintf("%s.%s", ResourcePrefix, pool)
	}

	for i := range pod.Spec.InitContainers {
		if err := validatePoolResource(&pod.Spec.Containers[i], resname); err != nil {
			return err
		}
	}

	for i := range pod.Spec.Containers {
		if err := validatePoolResource(&pod.Spec.Containers[i], resname); err != nil {
			return err
		}
	}

	return nil
}

// validateResource validates the CPU pool extended resource against the requested CPU.
func validatePoolResource(c *api.Container, resname string) error {
	var pool, cpu int64 = -1, -1

	// For the validity check to pass we need to have:
	//  - no pool label, no CPU request, no pool request, or
	//  - no CPU request and a label-matching pool request with value 1, or
	//  - a CPU request and a label-matching pool request with equal value * 1000

	if c.Resources.Requests != nil {
		for name, res := range c.Resources.Requests {
			if name == api.ResourceCPU {
				cpu = res.MilliValue()
			} else if string(name) == resname {
				pool = res.Value()
			} else if strings.HasPrefix(string(name), ResourcePrefix) {
				return fmt.Errorf("inconsistent CPU pool vs. pool resource");
			}
		}
	}

	if resname != "" && pool == -1 {
		return fmt.Errorf("no matching pool resource (%s) for CPU pool label", resname)
	}

	if pool != cpu {
		return fmt.Errorf("inconsistent CPU (%d) vs. pool (%d) resource", cpu, pool)
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

