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

package main

import "flag"
import "strings"
import "bytes"
import "fmt"
import "os/exec"
import "os"
import "io/ioutil"
import "net/http"

import "gopkg.in/yaml.v2"
import "crypto/rand"

/* The pool-tool application is designed to work together with
 * cpumanager pool policy. The application sets the kubelet
 * configuration to match with the loaded profile file on selected
 * nodes. The profiles are yaml files and look like this:
 *
 *      cpupools:
 *        - name: "reserved"
 *          cpus: "0-7,9"
 *        - name: "default"
 *          cpus: "8,10-12"
 *
 * Note that the file format is subject to change in future versions of
 * this tool. The above example would create two cpu pools, reserved and
 * default, and assign cpus 0-7 (inclusive range) and 9 to pool reserved
 * and cpus 8, 10, 11, and 12 to pool default.
 *
 * Usage:
 *	    pool-tool <--profile profile> [--nodes node1,node2,...] [--port 8001] [--address 127.0.0.1]
 *
 * In normal use port and address don't need to be changed, given that
 * the command is run on kubernetes master node.
 *
 * In order for the tool to work, Dynamic Kubelet Configuration must be
 * enabled on API server and kubelet. In 1.11 the feature should be on
 * by default, but 1.10 users will need to enable the feature gate
 * manually. For example, create a directory for the configuration:
 *
 *     # mkdir /var/lib/kubelet/config
 *
 * Then add
 *
 *     --feature-gates=DynamicKubeletConfig=true --dynamic-config-dir=/var/lib/kubelet/config
 *
 * to kubelet command lines and
 *
 *     --feature-gates=DynamicKubeletConfig=true
 *
 * to API server command line. Then run kube-proxy to enable pool-tool to
 * talk to the API server:
 *
 *     $ kubectl proxy -p 8001
 *
 * After this pool-tool will be able to set the kubelet configuration.
 */

// TODO items:
//   * Replace the kubectl calls with k8s go-client API calls
//   * Enable HT disabling support
//     * Ask the kubelet for CPU topology
//       * Use kubelet's secure configuration/stats port for this
//     * Find the HT sibling for a given core in a non-ht pool
//     * Take the sibling core offline
//   * Redo the configuration so that the kubelet command-line
//     configuration is compatible with pool-tool
//   * Add ability to request just a number of cores instead of
//     specifying the exact cores
//   * Add an option to control the cleaning up of old resources

type nodeList []string

func (n *nodeList) String() string {
	return fmt.Sprint(*n)
}

func (n *nodeList) Set(value string) error {
	for _, token := range strings.Split(value, ",") {
		*n = append(*n, token)
	}
	return nil
}

var nodes nodeList

type NodeSpecConfigSourceConfigMapRefYaml struct {
	Name string
}

type NodeSpecConfigSourceYaml struct {
	ConfigMapRef NodeSpecConfigSourceConfigMapRefYaml `yaml:"configMapRef,omitempty"`
}

type NodeSpecYaml struct {
	ConfigSource NodeSpecConfigSourceYaml `yaml:"configSource,omitempty"`
}

type NodeYaml struct {
	Spec NodeSpecYaml `yaml:"spec,omitempty"`
}

type ConfigMapYaml struct {
	Metadata struct {
		Uid  string
		Name string
	}
}

type CpuPool struct {
	Name string
	Cpus string
}

type Profile struct {
	Cpupools []CpuPool
}

func loadProfile(fileName string) *Profile {

	profile := Profile{}

	data, err := ioutil.ReadFile(fileName)
	if err != nil {
		return nil
	}

	err = yaml.Unmarshal([]byte(data), &profile)

	if err != nil {
		return nil
	}

	return &profile
}

func downloadNodeConfig(nodeName, address, port string) *[]byte {

	url := fmt.Sprintf("http://%s:%s/api/v1/nodes/%s/proxy/configz", address, port, nodeName)

	fmt.Printf("Download config from %s ...\n", url)

	resp, err := http.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	contents, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return nil
	}

	return &contents
}

func parseNodeConfig(fileName string) *Profile {

	profile := Profile{}

	data, err := ioutil.ReadFile(fileName)
	if err != nil {
		return nil
	}

	err = yaml.Unmarshal([]byte(data), &profile)

	if err != nil {
		return nil
	}

	return &profile
}

func runKubeCtl(cmdLine string) (error, bytes.Buffer, bytes.Buffer) {

	var outBuf, errBuf bytes.Buffer

	cmdTokens := strings.Split(cmdLine, " ")

	cmd := exec.Command("kubectl", cmdTokens...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()

	return err, outBuf, errBuf
}
func main() {

	var profile string
	var address string
	var port string

	// Usage:
	//    pool-tool <--profile profile> [--nodes node1,node2,...] [--port 8001] [--addrss 127.0.0.1]

	flag.StringVar(&profile, "profile", "", "Profile file name to apply.")
	flag.StringVar(&address, "address", "127.0.0.1", "API server address.")
	flag.StringVar(&port, "port", "8001", "API server port.")
	flag.Var(&nodes, "nodes", "Comma-sepatared list of node names.")

	flag.Parse()

	fmt.Printf("profile '%s', nodes '%s'.\n", profile, nodes.String())

	// Load the profile file.
	profileConfig := loadProfile(profile)

	if profileConfig == nil {
		panic("Error loading profile file")
	}

	fmt.Printf("Profile:\n\t%v\n", *profileConfig)

	for _, node := range nodes {

		// Download node configuration
		nodeConfigYaml := downloadNodeConfig(node, address, port)
		if nodeConfigYaml == nil {
			panic("Error downloading config for node " + node + ".")
		}

		nodeConfig := make(map[interface{}]interface{})
		err := yaml.Unmarshal([]byte(*nodeConfigYaml), &nodeConfig)
		if err != nil {
			panic("Error parsing node configuration.")
		}

		// Change the CPU pool information from profile configuration to
		// a form that is understood by node configuration

		poolConfig := make(map[string]string)
		for _, cpuPool := range profileConfig.Cpupools {
			cpuList := cpuPool.Cpus

			poolConfig[cpuPool.Name] = cpuList
		}

		fmt.Printf("PoolConfig:\n\t%v\n", poolConfig)

		// Add/change the CPUPools data in the node configuration

		nodeConfig["CPUPools"] = poolConfig

		// Add missing "kind" and "apiVersion" fields

		nodeConfig["kind"] = "KubeletConfiguration"
		nodeConfig["apiVersion"] = "kubelet.config.k8s.io/v1beta1"

		// FIXME: Add a read only port. This is needed because it
		// appears that not all configuration properties get propagated
		// to the map? This should be removed anyway, because read only
		// port support is dropped in future kubernetes versions.
		nodeConfig["readOnlyPort"] = 10255

		// Convert the configuration back to yaml

		data, err := yaml.Marshal(&nodeConfig)
		if err != nil {
			panic("Error marshaling node configuration data to yaml.")
		}

		file, err := ioutil.TempFile("", "pool-tool-node-config")
		if err != nil {
			panic("Error creating temporary file for node configuration.")
		}

		defer func() {
			file.Close()
			os.Remove(file.Name())
		}()

		for n := 0; n < len(data); {
			wrote, err := file.Write(data[n:])
			if err != nil {
				panic("Error writing temporary file for node configuration.")
			}
			n += wrote
		}

		// Create short random string to add to the end of the ConfigMap
		// name. This guarantees that we can create ConfigMaps which
		// have the same content with different names, since kubectl's
		// --append-hash option just appends the hash of the ConfigMap's
		// contents to the name.

		r := make([]byte, 16)
		rand.Read(r)
		postfix := fmt.Sprintf("%x", r)

		// Call kubectl to create and push ConfigMap based on the config

		err, outBuf, errBuf := runKubeCtl("-n kube-system create configmap cpu-pool-node-config-" + postfix + " -o yaml --from-file=kubelet=" + file.Name())

		if err != nil {
			fmt.Println(err)
			panic("Error creating ConfigMap: " + errBuf.String())
		}

		// Parse the ConfigMap information from the result

		configMapYaml := ConfigMapYaml{}
		err = yaml.Unmarshal(outBuf.Bytes(), &configMapYaml)
		if err != nil {
			panic("Error parsing ConfigMap.")
		}

		configMapName := configMapYaml.Metadata.Name
		configMapUid := configMapYaml.Metadata.Uid

		fmt.Printf("ConfigMap name: '%s', uid: '%s'\n", configMapName, configMapUid)

		// Add the node permissions to read the ConfigMap

		err, outBuf, errBuf = runKubeCtl("-n kube-system create role --resource=configmap " + configMapName + "-reader --verb=get --resource-name=" + configMapName + " -o name")

		if err != nil {
			fmt.Println(err)
			panic("Error creating role: " + errBuf.String())
		}

		roleName := strings.TrimSpace(outBuf.String())

		err, outBuf, errBuf = runKubeCtl("-n kube-system create rolebinding " + configMapName + "-reader --role=" + configMapName + "-reader --user=system:node:" + node + " -o name")

		if err != nil {
			fmt.Println(err)
			panic("Error creating rolebinding: " + errBuf.String())
		}

		roleBindingName := strings.TrimSpace(outBuf.String())

		// Remove old ConfigMap

		var oldConfigMap string

		err, outBuf, errBuf = runKubeCtl("get node " + node + " -o yaml")
		if err != nil {
			fmt.Println(err)
			panic("Error getting current node configuration: " + errBuf.String())
		}
		nodeYaml := NodeYaml{}
		err = yaml.Unmarshal(outBuf.Bytes(), &nodeYaml)
		if err != nil {
			panic("failed to parse node yaml: '" + outBuf.String() + "'")

		}

		oldConfigMap = nodeYaml.Spec.ConfigSource.ConfigMapRef.Name

		// Create a configuration snippet

		snippet := fmt.Sprintf("{\"spec\":{\"configSource\":{\"configMapRef\":{\"name\":\"%s\",\"namespace\":\"kube-system\",\"uid\":\"%s\"}}}}", configMapName, configMapUid)

		// Patch the Node object with the configuration

		err, outBuf, errBuf = runKubeCtl("patch node " + node + " -p " + snippet)

		if err != nil {
			fmt.Println(err)
			panic("Error patching node:\n\t" + errBuf.String() + "\n\t" + outBuf.String() + "\n\t" + snippet)
		}

		fmt.Println("Patched kubelet on node '" + node + "' to use the CPU profile '" + profile + "'.")
		fmt.Println("ConfigMap name: '" + configMapName + "', Role name: '" + roleName + "', RoleBinding name: '" + roleBindingName + "'")

		// Remove the read rights (RoleBinding) from the Node to the old
		// ConfigMap. Also remove the Role and the ConfigMap.  Should we
		// add a label to the map to indicate that it was created by
		// pool-tool?

		if oldConfigMap != "" {
			err, _, errBuf = runKubeCtl("-n kube-system delete rolebinding " + oldConfigMap + "-reader")
			if err != nil {
				fmt.Println(err)
				panic("Error deleting old RoleBinding: " + errBuf.String())
			}
			err, _, errBuf = runKubeCtl("-n kube-system delete role " + oldConfigMap + "-reader")
			if err != nil {
				fmt.Println(err)
				panic("Error deleting old Role: " + errBuf.String())
			}
			err, _, errBuf = runKubeCtl("-n kube-system delete configmap " + oldConfigMap)
			if err != nil {
				fmt.Println(err)
				panic("Error deleting old ConfigMap: " + errBuf.String())
			}
		}
	}
}
