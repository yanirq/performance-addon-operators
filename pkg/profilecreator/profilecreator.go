/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2021 Red Hat, Inc.
 */

package profilecreator

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/jaypipes/ghw"
	"github.com/jaypipes/ghw/pkg/cpu"
	"github.com/jaypipes/ghw/pkg/option"
	"github.com/jaypipes/ghw/pkg/topology"
	log "github.com/sirupsen/logrus"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"

	machineconfigv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	v1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ClusterScopedResources defines the subpath, relative to the top-level must-gather directory.
	// A top-level must-gather directory is of the following format:
	// must-gather-dir/quay-io-openshift-kni-performance-addon-operator-must-gather-sha256-<Image SHA>
	// Here we find the cluster-scoped definitions saved by must-gather
	ClusterScopedResources = "cluster-scoped-resources"
	// CoreNodes defines the subpath, relative to ClusterScopedResources, on which we find node-specific data
	CoreNodes = "core/nodes"
	// MCPools defines the subpath, relative to ClusterScopedResources, on which we find the machine config pool definitions
	MCPools = "machineconfiguration.openshift.io/machineconfigpools"
	// YAMLSuffix is the extension of the yaml files saved by must-gather
	YAMLSuffix = ".yaml"
	// Nodes defines the subpath, relative to top-level must-gather directory, on which we find node-specific data
	Nodes = "nodes"
	// SysInfoFileName defines the name of the file where ghw snapshot is stored
	SysInfoFileName = "sysinfo.tgz"
)

func init() {
	log.SetOutput(os.Stderr)
}

// GetMatchedNodes returns the list of nodes that are targetted by a specified labelSelector
func GetMatchedNodes(nodes []*v1.Node, labelSelector *metav1.LabelSelector) ([]*v1.Node, error) {
	matchedNodes := make([]*v1.Node, 0)
	for _, node := range nodes {
		matches, _ := corev1.MatchNodeSelectorTerms(node, getNodeSelectorFromLabelSelector(labelSelector))
		if matches {
			matchedNodes = append(matchedNodes, node)
		}
	}
	return matchedNodes, nil
}

func getNodeSelectorFromLabelSelector(labelSelector *metav1.LabelSelector) *v1.NodeSelector {

	matchExpressions := make([]v1.NodeSelectorRequirement, 0)
	for key, value := range labelSelector.MatchLabels {
		matchExpressions = append(matchExpressions, v1.NodeSelectorRequirement{
			Key:      key,
			Operator: v1.NodeSelectorOpIn,
			Values:   []string{value},
		})
	}
	matchFields := make([]v1.NodeSelectorRequirement, 0)
	for _, labelSelectorRequirement := range labelSelector.MatchExpressions {
		matchExpressions = append(matchFields, v1.NodeSelectorRequirement{
			Key:      labelSelectorRequirement.Key,
			Operator: v1.NodeSelectorOperator(string(labelSelectorRequirement.Operator)),
			Values:   labelSelectorRequirement.Values,
		})
	}

	nodeSelectorTerms := []v1.NodeSelectorTerm{
		{
			MatchExpressions: matchExpressions,
			MatchFields:      matchFields,
		},
	}
	nodeSelector := &v1.NodeSelector{
		NodeSelectorTerms: nodeSelectorTerms,
	}

	return nodeSelector

}

func getMustGatherFullPaths(mustGatherPath string, suffix string) (string, error) {
	// The glob pattern below depends on the must gather image name. It is assumed here
	// that the image would have "performance-addon-operator-must-gather" substring in the name.
	paths, err := filepath.Glob(mustGatherPath + "/*performance-addon-operator-must-gather*/" + suffix)
	if err != nil {
		return "", fmt.Errorf("Error obtaining the path mustGatherPath:%s, suffix:%s %v", mustGatherPath, suffix, err)
	}
	if paths == nil {
		return "", fmt.Errorf("No match for the specified must gather directory path: %s and suffix: %s", mustGatherPath, suffix)

	}
	if len(paths) > 1 {
		log.Infof("Multiple matches for the specified must gather directory path: %s and suffix: %s", mustGatherPath, suffix)
		return "", fmt.Errorf("Multiple matches for the specified must gather directory path: %s and suffix: %s.\n Expected only one performance-addon-operator-must-gather* directory, please check the must-gather tarball", mustGatherPath, suffix)
	}
	// returning one possible path
	return paths[0], err
}

func getNode(mustGatherDirPath, nodeName string) (*v1.Node, error) {
	var node v1.Node
	nodePathSuffix := path.Join(ClusterScopedResources, CoreNodes, nodeName)
	path, err := getMustGatherFullPaths(mustGatherDirPath, nodePathSuffix)
	if err != nil {
		return nil, fmt.Errorf("Error obtaining MachineConfigPool %s: %v", nodeName, err)
	}

	src, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Error opening %q: %v", path, err)
	}
	defer src.Close()

	dec := k8syaml.NewYAMLOrJSONDecoder(src, 1024)
	if err := dec.Decode(&node); err != nil {
		return nil, fmt.Errorf("Error opening %q: %v", path, err)
	}
	return &node, nil
}

// GetNodeList returns the list of nodes using the Node YAMLs stored in Must Gather
func GetNodeList(mustGatherDirPath string) ([]*v1.Node, error) {
	machines := make([]*v1.Node, 0)

	nodePathSuffix := path.Join(ClusterScopedResources, CoreNodes)
	nodePath, err := getMustGatherFullPaths(mustGatherDirPath, nodePathSuffix)
	if err != nil {
		return nil, fmt.Errorf("Error obtaining Nodes: %v", err)
	}
	if nodePath == "" {
		return nil, fmt.Errorf("Error obtaining Nodes: %v", err)
	}

	nodes, err := ioutil.ReadDir(nodePath)
	if err != nil {
		return nil, fmt.Errorf("failed to list mustGatherPath directories: %v", err)
	}
	for _, node := range nodes {
		nodeName := node.Name()
		node, err := getNode(mustGatherDirPath, nodeName)
		if err != nil {
			return nil, fmt.Errorf("Error obtaining Nodes %s: %v", nodeName, err)
		}
		machines = append(machines, node)
	}
	return machines, nil
}

// GetMCP returns an MCP object corresponding to a specified MCP Name
func GetMCP(mustGatherDirPath, mcpName string) (*machineconfigv1.MachineConfigPool, error) {
	var mcp machineconfigv1.MachineConfigPool

	mcpPathSuffix := path.Join(ClusterScopedResources, MCPools, mcpName+YAMLSuffix)
	mcpPath, err := getMustGatherFullPaths(mustGatherDirPath, mcpPathSuffix)
	if err != nil {
		return nil, fmt.Errorf("Error obtaining MachineConfigPool %s: %v", mcpName, err)
	}
	if mcpPath == "" {
		return nil, fmt.Errorf("Error obtaining MachineConfigPool, mcp:%s does not exist: %v", mcpName, err)
	}

	src, err := os.Open(mcpPath)
	if err != nil {
		return nil, fmt.Errorf("Error opening %q: %v", mcpPath, err)
	}
	defer src.Close()
	dec := k8syaml.NewYAMLOrJSONDecoder(src, 1024)
	if err := dec.Decode(&mcp); err != nil {
		return nil, fmt.Errorf("Error opening %q: %v", mcpPath, err)
	}
	return &mcp, nil
}

// NewGHWHandler is a handler to use ghw options corresponding to a node
func NewGHWHandler(mustGatherDirPath string, node *v1.Node) (*GHWHandler, error) {
	nodeName := node.GetName()
	nodePathSuffix := path.Join(Nodes)
	nodepath, err := getMustGatherFullPaths(mustGatherDirPath, nodePathSuffix)
	if err != nil {
		return nil, fmt.Errorf("Error obtaining the node path %s: %v", nodeName, err)
	}
	_, err = os.Stat(path.Join(nodepath, nodeName, SysInfoFileName))
	if err != nil {
		return nil, fmt.Errorf("Error obtaining the path: %s for node %s: %v", nodeName, nodepath, err)
	}
	options := ghw.WithSnapshot(ghw.SnapshotOptions{
		Path: path.Join(nodepath, nodeName, SysInfoFileName),
	})
	ghwHandler := &GHWHandler{snapShotOptions: options}
	return ghwHandler, nil
}

// GHWHandler is a wrapper around ghw to get the API object
type GHWHandler struct {
	snapShotOptions *option.Option
}

// CPU returns a CPUInfo struct that contains information about the CPUs on the host system
func (ghwHandler GHWHandler) CPU() (*cpu.Info, error) {
	return ghw.CPU(ghwHandler.snapShotOptions)
}

// SortedTopology returns a TopologyInfo struct that contains information about the Topology sorted by numa ids and cpu ids on the host system
func (ghwHandler GHWHandler) SortedTopology() (*topology.Info, error) {
	topologyInfo, err := ghw.Topology(ghwHandler.snapShotOptions)
	if err != nil {
		return nil, fmt.Errorf("Error obtaining Topology Info from GHW snapshot: %v", err)
	}
	sort.Slice(topologyInfo.Nodes, func(x, y int) bool {
		return topologyInfo.Nodes[x].ID < topologyInfo.Nodes[y].ID
	})
	for _, node := range topologyInfo.Nodes {
		for _, core := range node.Cores {
			sort.Slice(core.LogicalProcessors, func(x, y int) bool {
				return core.LogicalProcessors[x] < core.LogicalProcessors[y]
			})
		}
		sort.Slice(node.Cores, func(i, j int) bool {
			return node.Cores[i].LogicalProcessors[0] < node.Cores[j].LogicalProcessors[0]
		})
	}
	return topologyInfo, nil
}

// GetReservedAndIsolatedCPUs returns Reserved and Isolated CPUs
func (ghwHandler GHWHandler) GetReservedAndIsolatedCPUs(reservedCPUCount int, splitReservedCPUsAcrossNUMA bool) (string, string, error) {
	if reservedCPUCount < 0 {
		return "", "", fmt.Errorf("Specified eservered CPU count is negative, please specify it correctly")
	}
	topologyInfo, err := ghwHandler.SortedTopology()
	if err != nil {
		return "", "", fmt.Errorf("Error obtaining Topology Info from GHW snapshot: %v", err)
	}

	totalCPUSet := cpuset.NewBuilder()
	reservedCPUSet := cpuset.NewBuilder()
	var numaNodeNum int
	if splitReservedCPUsAcrossNUMA {
		numaNodeNum = len(topologyInfo.Nodes)
	} else {
		numaNodeNum = 1
	}

	var max = 0
	reservedPerNuma := reservedCPUCount / numaNodeNum
	remainder := reservedCPUCount % numaNodeNum
	if remainder != 0 {
		log.Warnf("The reserved CPUs cannot be split equally across NUMA Nodes")
	}
	htEnabled, err := ghwHandler.isHyperthreadingEnabled()
	if err != nil {
		return "", "", fmt.Errorf("Error determining if Hyperthreading is enabled or not: %v", err)
	}

	//TODO: Make the allocation logic below more readable by using separate helper functions, one per allocation strategy
	// (splitReservedCPUsAcrossNUMA=false/true -> two strategies) each one with its clear and nice allocation loop
	for numaID, node := range topologyInfo.Nodes {
		if splitReservedCPUsAcrossNUMA {
			if remainder != 0 {
				max = (numaID+1)*reservedPerNuma + (numaNodeNum - remainder)
				remainder--
			} else {
				max = max + reservedPerNuma
			}
		} else {
			max = reservedCPUCount
		}
		if max%2 != 0 && htEnabled {
			return "", "", fmt.Errorf("Can't allocatable odd number of CPUs from a NUMA Node")
		}
		for _, processorCores := range node.Cores {
			for _, core := range processorCores.LogicalProcessors {
				totalCPUSet.Add(core)
				if reservedCPUSet.Result().Size() < max {
					reservedCPUSet.Add(core)
				}
			}
		}
	}
	isolatedCPUSet := totalCPUSet.Result().Difference(reservedCPUSet.Result())
	log.Infof("reservedCPUs: %v len(reservedCPUs): %d\n isolatedCPUs: %v len(isolatedCPUs): %d\n", reservedCPUSet.Result().String(), reservedCPUSet.Result().Size(), isolatedCPUSet.String(), isolatedCPUSet.Size())
	return reservedCPUSet.Result().String(), isolatedCPUSet.String(), nil

}

// isHyperthreadingEnabled checks if hyperthreading is enabled on the system or not
func (ghwHandler GHWHandler) isHyperthreadingEnabled() (bool, error) {
	cpuInfo, err := ghwHandler.CPU()
	if err != nil {
		return false, fmt.Errorf("Error obtaining CPU Info from GHW snapshot: %v", err)
	}
	// Since there is no way to disable flags per-processor (not system wide) we check the flags of the first available processor.
	// A following implementation will leverage the /sys/devices/system/cpu/smt/active file which is the "standard" way to query HT.
	return contains(cpuInfo.Processors[0].Capabilities, "ht"), nil
}

// contains checks if a string is present in a slice
func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}
	return false
}
