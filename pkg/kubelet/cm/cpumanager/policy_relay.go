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
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"google.golang.org/grpc"

	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"

	api "k8s.io/kubernetes/pkg/kubelet/apis/cpuplugin/v1alpha"
	stub "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/stub"
)

const (
	// log message prefix
	logPrefix = "[cpumanager/plugin] "
	// PolicyRelay is the name of the relay policy
	PolicyRelay policyName = "plugin"
	// request-response timeout
	relayTimeout = 1 * time.Second
)

var _ Policy = &pluginRelay{}
var _ api.RegistrationServer = &pluginRelay{}

// Allow resource declarations without a namespace for these resources.
var whitelistedResources = map[string]bool{
	string(v1.ResourceCPU): true,
}

type pluginRelay struct {
	sync.Mutex                            // we're lockable
	topology        *topology.CPUTopology // discovered CPU topology
	numReservedCPUs int                   // reserved number of CPUs
	policyConfig    map[string]string     // opaque policy configuration
	expectedPolicy  string                // expected policy
	updateCapacity  UpdateCapacityFunc    // function to update resource capacity
	socketDir       string                // directory for server and plugin sockets
	serverAddr      string                // registration server socket path
	srv             *grpc.Server          // registration gRPC server
	activePolicy    string                // active policy, if any
	vendor          string                // policy plugin vendor (domain)
	vendorNamespace string                // vendor resource namespace
	clt             *grpc.ClientConn      // CPU plugin gRPC connection
	plugin          api.CpuPluginClient   // CPU plugin client stub
	state           state.State           // cached state for configure et al.
}

//
// CPU Manager policy interface.
//

// NewRelayPolicy creates a new policy for relaying to external CPU policy plugins.
func NewRelayPolicy(topology *topology.CPUTopology, numReservedCPUs int,
	plugin string, config map[string]string, updateCapacityFunc UpdateCapacityFunc) Policy {
	logInfo("creating external policy relay")

	r := pluginRelay{
		topology:        topology,
		numReservedCPUs: numReservedCPUs,
		policyConfig:    config,
		expectedPolicy:  plugin,
		serverAddr:      api.CpuManagerSocket,
		updateCapacity:  updateCapacityFunc,
	}

	r.socketDir, _ = filepath.Split(api.CpuManagerSocket)

	return &r
}

// Return the our well-known policy name.
func (r *pluginRelay) Name() string {
	return string(PolicyRelay)
}

// Start the plugin, creating our registration server.
func (r *pluginRelay) Start(s state.State) {
	logInfo("starting CPU policy plugin relay")

	r.checkState(s)
	r.startRegistrationServer()
}

// Allocate CPU and related resources for the given container.
func (r *pluginRelay) AddContainer(s state.State, pod *v1.Pod, container *v1.Container, containerID string) error {
	logInfo("AddContainer request...")

	if !r.hasCPUPlugin() {
		return nil
	}

	return r.addContainer(s, pod, container, containerID)
}

// Release CPU and related resources for the given container.
func (r *pluginRelay) RemoveContainer(s state.State, containerID string) error {
	logInfo("RemoveContainer request...")

	if !r.hasCPUPlugin() {
		return nil
	}

	return r.removeContainer(s, containerID)
}

// Check supplied state. We don't really check state, we just dig out any stored policy data.
func (r *pluginRelay) checkState(s state.State) {
	r.state = s
}

//
// CPU Plugin Registration Server
//

// Register the CPU policy plugin.
func (r *pluginRelay) Register(ctx context.Context, req *api.RegisterRequest) (*api.Empty, error) {
	logInfo("CPU plugin %s, api %s registering.", req.Name, req.Version)

	if req.Version != api.Version {
		logError("incorrect API version %s, expecting %s", req.Version, api.Version)
		return &api.Empty{}, fmt.Errorf("incorrect API version %s, expecting %s", req.Version, api.Version)
	}

	if r.expectedPolicy != "" && r.expectedPolicy != req.Name {
		logError("incorrect policy %s, expecting %s", req.Name, r.expectedPolicy)
	}

	if r.clt != nil {
		logError("CPU plugin already active, rejecting new one")
		return &api.Empty{}, fmt.Errorf("CPU plugin already active")
	}

	if err := r.startCPUPluginClient(req.Name, req.Vendor); err != nil {
		return &api.Empty{}, err
	}

	r.configure()

	return &api.Empty{}, nil
}

// Set up the Registration server.
func (r *pluginRelay) startRegistrationServer() {
	socketDir, _ := filepath.Split(r.serverAddr)
	os.MkdirAll(socketDir, 0755)

	if err := r.cleanupSockets(); err != nil {
		logPanic("failed to start registration server: %+v", err)
	}

	sock, err := net.Listen("unix", r.serverAddr)
	if err != nil {
		logPanic("failed to create socket %s: %+v", r.serverAddr, err)
	}

	r.srv = grpc.NewServer([]grpc.ServerOption{}...)

	api.RegisterRegistrationServer(r.srv, r)
	go func() {
		r.srv.Serve(sock)
	}()
}

// Clean up the CPU plugin socket directory.
func (r *pluginRelay) cleanupSockets() error {
	d, err := os.Open(r.socketDir)
	if err != nil {
		return err
	}
	defer d.Close()

	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}

	for _, name := range names {
		file := filepath.Join(r.socketDir, name)
		stat, err := os.Stat(file)
		if err != nil {
			continue
		}
		if stat.IsDir() {
			continue
		}
		err = os.RemoveAll(file)
		if err != nil {
			return err
		}
	}

	return nil
}

//
// CPU Plugin Client/Stub
//

func (r *pluginRelay) stubState() *api.State {
	out := &api.State{
		Assignments:   make(map[string]string),
		DefaultCPUSet: r.state.GetDefaultCPUSet().String(),
		PluginState:   r.state.GetPolicyData(),
	}

	for id, cset := range r.state.GetCPUAssignments() {
		out.Assignments[id] = cset.String()
	}

	return out
}

// Set up connection to the registered CPU plugin.
func (r *pluginRelay) startCPUPluginClient(policy string, vendor string) error {
	sockAddr := filepath.Join(api.CpuPluginPath, policy) + ".sock"
	conn, err := grpc.Dial(sockAddr, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)
	if err != nil {
		logError("failed to connect to CPU plugin %s (%s): %+v", policy, sockAddr, err)
		return err
	}

	r.Lock()
	r.vendor = vendor
	r.vendorNamespace = vendor + "/"
	r.clt = conn
	r.plugin = api.NewCpuPluginClient(conn)
	r.activePolicy = policy
	r.Unlock()

	return nil
}

// Tear down the connection to the registered CPU plugin.
func (r *pluginRelay) stopCPUPluginClient() {
	r.Lock()
	defer r.Unlock()

	if r.clt != nil {
		r.clt.Close()
		r.clt = nil
		r.activePolicy = ""
	}
}

func (r *pluginRelay) hasCPUPlugin() bool {
	r.Lock()
	defer r.Unlock()
	return r.clt != nil
}

// Configure the CPU plugin.
func (r *pluginRelay) configure() error {
	ctx, cancel := context.WithTimeout(context.Background(), relayTimeout)
	defer cancel()

	rpl, err := r.plugin.Configure(ctx, &api.ConfigureRequest{
		Topology:        stub.StubCPUTopology(*r.topology),
		NumReservedCPUs: int32(r.numReservedCPUs),
		Config:          r.policyConfig,
		State:           r.stubState(),
	})

	logInfo("got Configure response %v", *rpl)

	r.applyStateChanges(r.state, rpl.State)

	if err := r.updatePluginResources(rpl.Resources); err != nil {
		return err
	}

	return err
}

// Add a container to the CPU plugin.
func (r *pluginRelay) addContainer(s state.State, pod *v1.Pod, container *v1.Container, containerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), relayTimeout)
	defer cancel()

	rpl, err := r.plugin.AddContainer(ctx, &api.AddContainerRequest{
		Id:        containerID,
		Pod:       stub.StubPod(*pod),
		Container: stub.StubContainer(*container),
	})

	if err != nil {
		return err
	}

	logInfo("got AddContainer response %v", *rpl)

	if err := r.applyContainerHints(s, rpl.Hints); err != nil {
		return err
	}

	if err := r.updatePluginResources(rpl.Resources); err != nil {
		return err
	}

	r.applyStateChanges(s, rpl.State)

	return nil
}

// Remove a container from the CPU plugin.
func (r *pluginRelay) removeContainer(s state.State, containerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), relayTimeout)
	defer cancel()

	rpl, err := r.plugin.RemoveContainer(ctx, &api.RemoveContainerRequest{
		Id: containerID,
	})

	if err != nil {
		return err
	}

	if err := r.applyContainerHints(s, rpl.Hints); err != nil {
		return err
	}

	if err := r.updatePluginResources(rpl.Resources); err != nil {
		return err
	}

	r.applyStateChanges(s, rpl.State)

	return nil
}

//
// CPU Plugin Policy Decision/Hint Handling
//

// Update container resource allocation.
func (r *pluginRelay) applyContainerHints(s state.State, hints map[string]*api.ContainerHint) error {
	if hints == nil {
		return nil
	}

	for _, h := range hints {
		if h.Id == "delete" {
			s.Delete(h.Id)
		} else {
			cset, err := cpuset.Parse(h.Cpuset)
			if err != nil {
				return err
			}
			s.SetCPUSet(h.Id, cset)
		}
	}

	return nil
}

// Update resources declared by the plugin.
func (r *pluginRelay) updatePluginResources(resources map[string]*api.Quantity) error {
	cap := v1.ResourceList{}

	for name, qty := range resources {
		if !strings.HasPrefix(name, r.vendorNamespace) {
			if !whitelistedResources[name] {
				name = r.vendorNamespace + name
			}
		}
		res := v1.ResourceName(name)
		cap[res] = stub.CoreQuantity(qty)
	}

	r.updateCapacity(cap)

	return nil
}

// Update state changes made by the plugin.
func (r *pluginRelay) applyStateChanges(s state.State, as *api.State) error {
	if as != nil {
		s.SetDefaultCPUSet(cpuset.MustParse(as.DefaultCPUSet))
		s.SetPolicyData(as.PluginState)
	}

	return nil
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

func logPanic(format string, args ...interface{}) {
	logFatal(format, args...)
	panic(logFormat(format, args...))
}
