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

package v1alpha

const (
	// Supported version of the API.
	Version = "v1draft"
	// Directory where both the CPU Manager registration socket and the
	// CPU plugin socket is located. Only privileged pods have access to
	// this path.
	CpuPluginPath = "/var/lib/kubelet/cpu-plugin"
	// CPU Manager socket path.
	CpuManagerSocket = CpuPluginPath + "/cpumgr.sock"
)
