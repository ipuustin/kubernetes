// Copyright 2018 Intel Corporation. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stub

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"

	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"

	api "k8s.io/kubernetes/pkg/kubelet/apis/cpuplugin/v1alpha"
)

const (
	logPrefix = "[cpu-policy/stub] "
)

// CpuPolicy interface, implements the actual policy logic.
type CpuPolicy interface {
	Name() string
	Start(s State, t *topology.CPUTopology, numReservedCPUs int) error
	Configure(s State) error
	AddContainer(s State, p *v1.Pod, c *v1.Container, id string) error
	RemoveContainer(s State, id string)
}

// CPU plugin interface.
type CpuPlugin interface {
	StartCpuPlugin() error
}

// policy logic.
type cpuPlugin struct {
	serverAddr string
	clientAddr string
	policy     CpuPolicy
	srv        *grpc.Server
	vendor     string
	state      stubState
}

// Create a new CPU plugin.
func NewCpuPlugin(policy CpuPolicy, vendor string) (CpuPlugin, error) {

	if !strings.Contains(vendor, ".") {
		return nil, fmt.Errorf("Invalid vendor string %s, should be a domain name.", vendor)
	}

	p := &cpuPlugin{
		serverAddr: api.CpuManagerSocket,
		clientAddr: filepath.Join(api.CpuPluginPath, policy.Name()) + ".sock",
		policy:     policy,
		vendor:     vendor,
	}

	return p, nil
}

// Set up and start the CPU plugin, register with the CPUManager.
func (p *cpuPlugin) StartCpuPlugin() error {
	for {
		// Check that another plugin is not running.
		if err := waitForServer(p.clientAddr, time.Second); err == nil {
			return fmt.Errorf("socket %s is already in use", p.clientAddr)
		}
		os.Remove(p.clientAddr)

		// Create our socket.
		lis, err := net.Listen("unix", p.clientAddr)
		if err != nil {
			return fmt.Errorf("failed to create/listen on plugin socket %s (%+v)",
				p.clientAddr, err)
		}

		// Set up our CPU Plugin interface/server.
		p.srv = grpc.NewServer()
		api.RegisterCpuPluginServer(p.srv, p)

		go func() {
			logInfo("CPU plugin starting server at: %s\n", p.clientAddr)
			p.srv.Serve(lis)
		}()

		// Wait for our server to start up.
		if err = waitForServer(p.clientAddr, 10*time.Second); err != nil {
			return fmt.Errorf("failed to wait for our gRPC server: %+v", err)
		}

		// Register with the CPUManager/kubelet relay.
		err = p.registerWithCPUManager()
		if err != nil {
			return fmt.Errorf("failed to register with CPUManager: %+v", err)
		}

		logInfo("CPU policy plugin %s registered", p.policy.Name())

		for {
			if _, err := os.Stat(p.clientAddr); os.IsNotExist(err) {
				logInfo("CPU policy plugin socket removed, stopping...")
				p.srv.Stop()
				break
			}
			time.Sleep(1 * time.Second)
		}
	}
}

// Check if a gRPC server is alive
func waitForServer(addr string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)

	if conn != nil {
		conn.Close()
	}

	return err
}

// Register with the CPUManager
func (p *cpuPlugin) registerWithCPUManager() error {
	conn, err := grpc.Dial(p.serverAddr, grpc.WithInsecure(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}))
	defer conn.Close()
	if err != nil {
		return fmt.Errorf("CPU plugin stub: cannot connect to CPUManager: %+v", err)
	}

	c := api.NewRegistrationClient(conn)
	req := &api.RegisterRequest{
		Version: api.Version,
		Name:    p.policy.Name(),
		Vendor:  p.vendor,
	}

	_, err = c.Register(context.Background(), req)
	if err != nil {
		return fmt.Errorf("CPU plugin stub: cannot register with CPUManager: %+v", err)
	}

	return nil
}

// Relay configuration request.
func (p *cpuPlugin) Configure(ctx context.Context, req *api.ConfigureRequest) (*api.ConfigureResponse, error) {
	logInfo("Configure request")

	topology := CoreCPUTopology(req.Topology)
	numReservedCPUs := int(req.NumReservedCPUs)
	p.state = newStubState(req.State, p.vendor)

	if err := p.policy.Start(&p.state, &topology, numReservedCPUs); err != nil {
		return nil, err
	}

	if err := p.policy.Configure(&p.state); err != nil {
		return nil, err
	}

	return &api.ConfigureResponse{
		Resources: p.state.ResourceChanges(true),
		State:     p.state.StateChanges(),
	}, nil
}

// Relay AddContainer request.
func (p *cpuPlugin) AddContainer(ctx context.Context, req *api.AddContainerRequest) (*api.AddContainerResponse, error) {
	logInfo("AddContainer request")

	pod := CorePod(req.Pod)
	container := CoreContainer(req.Container)
	id := req.Id

	p.state.Reset()
	err := p.policy.AddContainer(&p.state, &pod, &container, id)
	if err != nil {
		return nil, err
	}

	return &api.AddContainerResponse{
		Hints:     p.state.ContainerChanges(),
		Resources: p.state.ResourceChanges(false),
		State:     p.state.StateChanges(),
	}, nil
}

// Relay RemoveContainer request.
func (p *cpuPlugin) RemoveContainer(ctx context.Context, req *api.RemoveContainerRequest) (*api.RemoveContainerResponse, error) {
	logInfo("RemoveContainer request")

	id := req.Id

	p.state.Reset()
	p.policy.RemoveContainer(&p.state, id)

	return &api.RemoveContainerResponse{
		Hints:     p.state.ContainerChanges(),
		Resources: p.state.ResourceChanges(false),
		State:     p.state.StateChanges(),
	}, nil
}
