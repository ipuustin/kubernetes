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

package main

import (
	"context"
	"log"
	"net"
	"os"
	"path"
	"sync"
	"time"

	"google.golang.org/grpc"

	cpumanagerapi "k8s.io/kubernetes/pkg/kubelet/apis/cpumanager/v1alpha"
	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
)

// Stub implementation for DevicePlugin.
type Stub struct {
	devs   []*pluginapi.Device
	socket string

	stop   chan interface{}
	wg     sync.WaitGroup
	update chan []*pluginapi.Device

	server *grpc.Server

	// allocFunc is used for handling allocation request
	allocFunc stubAllocFunc
}

// stubAllocFunc is the function called when receive an allocation request from Kubelet
type stubAllocFunc func(r *pluginapi.AllocateRequest, devs map[string]pluginapi.Device) (*pluginapi.AllocateResponse, error)

func defaultAllocFunc(r *pluginapi.AllocateRequest, devs map[string]pluginapi.Device) (*pluginapi.AllocateResponse, error) {
	var response pluginapi.AllocateResponse

	return &response, nil
}

// NewDevicePluginStub returns an initialized DevicePlugin Stub.
func NewDevicePluginStub(devs []*pluginapi.Device, socket string) *Stub {
	return &Stub{
		devs:   devs,
		socket: socket,

		stop:   make(chan interface{}),
		update: make(chan []*pluginapi.Device),

		allocFunc: defaultAllocFunc,
	}
}

// SetAllocFunc sets allocFunc of the device plugin
func (m *Stub) SetAllocFunc(f stubAllocFunc) {
	m.allocFunc = f
}

// Start starts the gRPC server of the device plugin. Can only
// be called once.
func (m *Stub) Start() error {
	err := m.cleanup()
	if err != nil {
		return err
	}

	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return err
	}

	m.wg.Add(1)
	m.server = grpc.NewServer([]grpc.ServerOption{}...)
	pluginapi.RegisterDevicePluginServer(m.server, m)
	cpumanagerapi.RegisterCpuManagerPluginServer(m.server, m)

	go func() {
		defer m.wg.Done()
		m.server.Serve(sock)
	}()
	log.Println("Starting to serve on", m.socket)

	return nil
}

// Stop stops the gRPC server. Can be called without a prior Start
// and more than once. Not safe to be called concurrently by different
// goroutines!
func (m *Stub) Stop() error {
	if m.server == nil {
		return nil
	}
	m.server.Stop()
	m.wg.Wait()
	m.server = nil
	close(m.stop) // This prevents re-starting the server.

	return m.cleanup()
}

// Register registers the device plugin for the given resourceName with Kubelet.
func (m *Stub) Register(kubeletEndpoint, resourceName string, preStartContainerFlag bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, kubeletEndpoint, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}))
	defer conn.Close()
	if err != nil {
		return err
	}
	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: resourceName,
		Options:      &pluginapi.DevicePluginOptions{PreStartRequired: preStartContainerFlag},
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}

// GetDevicePluginOptions returns DevicePluginOptions settings for the device plugin.
func (m *Stub) GetDevicePluginOptions(ctx context.Context, e *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// PreStartContainer resets the devices received
func (m *Stub) PreStartContainer(ctx context.Context, r *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	log.Printf("PreStartContainer, %+v", r)
	return &pluginapi.PreStartContainerResponse{}, nil
}

// ListAndWatch lists devices and update that list according to the Update call
func (m *Stub) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	log.Println("ListAndWatch")

	s.Send(&pluginapi.ListAndWatchResponse{Devices: m.devs})

	for {
		select {
		case <-m.stop:
			return nil
		case updated := <-m.update:
			s.Send(&pluginapi.ListAndWatchResponse{Devices: updated})
		}
	}
}

// Update allows the device plugin to send new devices through ListAndWatch
func (m *Stub) Update(devs []*pluginapi.Device) {
	m.update <- devs
}

// Allocate does a mock allocation
func (m *Stub) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	log.Printf("Allocate, %+v", r)

	devs := make(map[string]pluginapi.Device)

	for _, dev := range m.devs {
		devs[dev.ID] = *dev
	}

	return m.allocFunc(r, devs)
}

func (m *Stub) cleanup() error {
	if err := os.Remove(m.socket); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// CPU manager plugin specific functions

// AddContainer tells the plugin that the container has been added
func (m *Stub) AddContainer(ctx context.Context, r *cpumanagerapi.AddContainerRequest) (*cpumanagerapi.AddContainerResponse, error) {
	log.Printf("AddContainer, %+v", r)
	return &cpumanagerapi.AddContainerResponse{}, nil
}

// RemoveContainer tells the plugin that the container has been removed
func (m *Stub) RemoveContainer(ctx context.Context, r *cpumanagerapi.RemoveContainerRequest) (*cpumanagerapi.RemoveContainerResponse, error) {
	log.Printf("RemoveContainer, %+v", r)
	return &cpumanagerapi.RemoveContainerResponse{}, nil
}

// SetConfigData tells the plugin its configuration
func (m *Stub) SetConfigData(ctx context.Context, r *cpumanagerapi.ConfigDataRequest) (*cpumanagerapi.ConfigDataResponse, error) {
	log.Printf("SetConfigData, %+v", r)
	return &cpumanagerapi.ConfigDataResponse{}, nil
}

func main() {
	cpus := make([]*pluginapi.Device, 1)

	cpus[0] = &pluginapi.Device{
		ID:     "cpu-foobar-1",
		Health: "Healthy",
	}

	plugin := NewDevicePluginStub(cpus, "/var/lib/kubelet/device-plugins/device-plugin-server")

	plugin.Start()

	plugin.Register(pluginapi.KubeletSocket, "cpu-manager/cpu-plugin-stub", false)

	plugin.wg.Wait()
}
