/*
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

package packing

import (
	"context"
	"math"
	"sort"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/awslabs/karpenter/pkg/cloudprovider"
	"github.com/awslabs/karpenter/pkg/utils/binpacking"
	"github.com/awslabs/karpenter/pkg/utils/resources"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
)

type Instance struct {
	// TODO replace w/ generic instance parameters
	ec2.InstanceTypeInfo
	Zones []string
}

type packingResult struct {
	packed   []*v1.Pod
	unpacked []*v1.Pod
}

type packer struct{}

// Packer helps pack the pods and calculates efficient placement on the instances.
type Packer interface {
	Pack(context.Context, []*v1.Pod, []*Instance, *cloudprovider.Constraints) []*Packing
}

// Packing contains a list of pods that can be placed on any of Instance type
// in the InstanceTypes
type Packing struct {
	Pods          []*v1.Pod
	InstanceTypes []*Instance
}

// NewPacker returns a Packer implementation
func NewPacker() Packer {
	return &packer{}
}

// Pack returns the node packings for the provided pods. It computes a set of viable
// instance types for each packing of pods. Instance variety enables the cloud provider
// to make better cost and availability decisions. The instance types returned are sorted by resources.
// Pods provided are all schedulable in the same zone as tightly as possible.
// It follows the First Fit Decreasing bin packing technique, reference-
// https://en.wikipedia.org/wiki/Bin_packing_problem#First_Fit_Decreasing_(FFD)
func (p *packer) Pack(ctx context.Context, pods []*v1.Pod, instanceTypes []*Instance, constraints *cloudprovider.Constraints) []*Packing {
	// Sort pods in decreasing order by the amount of CPU requested, if
	// CPU requested is equal compare memory requested.
	sort.Sort(sort.Reverse(binpacking.ByResourcesRequested{SortablePods: pods}))
	var packings []*Packing
	var packing *Packing
	remainingPods := pods
	nodeCapacities := p.getNodeCapacities(instanceTypes, constraints)
	for len(remainingPods) > 0 {
		packing, remainingPods = p.packWithLargestPod(remainingPods, nodeCapacities)
		// checked all instance type and found no packing option
		if len(packing.Pods) == 0 {
			zap.S().Warnf("Failed to find instance type for pod %s/%s ", remainingPods[0].Namespace, remainingPods[0].Name)
			remainingPods = remainingPods[1:]
			continue
		}
		packings = append(packings, packing)
		sortByResources(packing.InstanceTypes)
		instanceTypeNames := []string{}
		for _, it := range packing.InstanceTypes {
			instanceTypeNames = append(instanceTypeNames, *it.InstanceType)
		}
		zap.S().Debugf("Selected %d instance type options for %d pod(s) %v", len(packing.InstanceTypes), len(packing.Pods), instanceTypeNames)
	}
	return packings
}

func (*packer) getNodeCapacities(instanceTypes []*Instance, constraints *cloudprovider.Constraints) []*nodeCapacity {
	nodeCapacities := []*nodeCapacity{}
	for _, instanceType := range instanceTypes {
		nc := nodeCapacityFrom(instanceType)
		kubeletOverhead := binpacking.CalculateKubeletOverhead(nc.total)
		if ok := nc.reserve(resources.Merge(constraints.Overhead, kubeletOverhead)); !ok {
			zap.S().Infof("Excluding instance type %s because there are not enough resources for the kubelet overhead", nc.instanceType)
			continue
		}
		nodeCapacities = append(nodeCapacities, nc)
	}
	return nodeCapacities
}

// packWithLargestPod will try to pack max number of pods with largest pod in
// pods across all available node capacities. It returns Packing: max pod count
// that fit; with their node capacities and list of leftover pods
func (p *packer) packWithLargestPod(unpackedPods []*v1.Pod, nodeCapacities []*nodeCapacity) (*Packing, []*v1.Pod) {
	bestPackedPods := []*v1.Pod{}
	bestCapacities := []*nodeCapacity{}
	remainingPods := unpackedPods
	for _, nc := range nodeCapacities {
		// check how many pods we can fit with the available capacity
		result := p.packPodsForCapacity(nc, unpackedPods)
		if len(result.packed) == 0 {
			continue
		}
		// If the pods packed are the same as before, this instance type can be
		// considered as a backup option in case we get ICE
		if p.podsMatch(bestPackedPods, result.packed) {
			bestCapacities = append(bestCapacities, nc)
		} else if len(result.packed) > len(bestPackedPods) {
			// If pods packed are more than compared to what we got in last
			// iteration, consider using this instance type
			bestPackedPods = result.packed
			remainingPods = result.unpacked
			bestCapacities = []*nodeCapacity{nc}
		}
	}
	instanceTypes := []*Instance{}
	for _, capacity := range bestCapacities {
		instanceTypes = append(instanceTypes, capacity.instanceType)
	}
	return &Packing{Pods: bestPackedPods, InstanceTypes: instanceTypes}, remainingPods
}

func (*packer) packPodsForCapacity(capacity *nodeCapacity, pods []*v1.Pod) *packingResult {
	// start with the largest pod based on resources requested
	result := &packingResult{}
	for _, pod := range pods {
		if ok := capacity.reserveForPod(&pod.Spec); ok {
			result.packed = append(result.packed, pod)
			continue
		}
		// if largest pod can't be packed try next node capacity
		if len(result.packed) == 0 {
			result.unpacked = append(result.unpacked, pods...)
			return result
		}
		result.unpacked = append(result.unpacked, pod)
	}
	return result
}

func (*packer) podsMatch(first, second []*v1.Pod) bool {
	if len(first) != len(second) {
		return false
	}
	podkey := func(pod *v1.Pod) string {
		return pod.Namespace + "/" + pod.Name
	}
	podSeen := map[string]int{}
	for _, pod := range first {
		podSeen[podkey(pod)]++
	}
	for _, pod := range second {
		podSeen[podkey(pod)]--
	}
	for _, value := range podSeen {
		if value != 0 {
			return false
		}
	}
	return true
}

// sortByResources sorts instance type packings by vcpus and memory resources
func sortByResources(instances []*Instance) {
	sort.Slice(instances, func(i, j int) bool {
		// Euclidean distance from origin using vcpus and memory
		// sqrt(vcpus[i]^2 + memoryInGiB[i]^2) < sqrt(vcpus[j]^2 + memoryInGiB[j]^2)
		return math.Sqrt(
			math.Pow(2, float64(*instances[i].VCpuInfo.DefaultVCpus))+
				math.Pow(2, float64(*instances[i].MemoryInfo.SizeInMiB)/1024)) <
			math.Sqrt(
				math.Pow(2, float64(*instances[j].VCpuInfo.DefaultVCpus))+
					math.Pow(2, float64(*instances[j].MemoryInfo.SizeInMiB)/1024))
	})
}
