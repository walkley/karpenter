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

package state

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/config"
	"github.com/aws/karpenter/pkg/scheduling"
	podutils "github.com/aws/karpenter/pkg/utils/pod"
	"github.com/aws/karpenter/pkg/utils/resources"
)

// Cluster maintains cluster state that is often needed but expensive to compute.
type Cluster struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider

	// Pod Specific Tracking
	antiAffinityPods sync.Map // mapping of pod namespaced name to *v1.Pod of pods that have required anti affinities

	nominatedNodes *cache.Cache

	// Node Status & Pod -> Node Binding
	mu       sync.RWMutex
	nodes    map[string]*Node                // node name -> node
	bindings map[types.NamespacedName]string // pod namespaced named -> node name
}

func NewCluster(cfg config.Config, client client.Client, cp cloudprovider.CloudProvider) *Cluster {
	// The nominationPeriod is how long we consider a node as 'likely to be used' after a pending pod was
	// nominated for it. This time can very depending on the batching window size + time spent scheduling
	// so we try to adjust based off the window size.
	nominationPeriod := time.Duration(1.5*cfg.BatchMaxDuration().Seconds()) * time.Second
	if nominationPeriod < 10*time.Second {
		nominationPeriod = 10 * time.Second
	}

	c := &Cluster{
		kubeClient:     client,
		cloudProvider:  cp,
		nominatedNodes: cache.New(nominationPeriod, 10*time.Second),
		nodes:          map[string]*Node{},
		bindings:       map[types.NamespacedName]string{},
	}
	return c
}

// Node is a cached version of a node in the cluster that maintains state which is expensive to compute every time it's
// needed.  This currently contains node utilization across all the allocatable resources, but will soon be used to
// compute topology information.
type Node struct {
	Node *v1.Node
	// Capacity is the total resources on the node.
	Capacity v1.ResourceList
	// Allocatable is the total amount of resources on the node after os overhead.
	Allocatable v1.ResourceList
	// Available is allocatable minus anything allocated to pods.
	Available v1.ResourceList
	// Available is the total amount of resources that are available on the node.  This is the Allocatable minus the
	// resources requested by all pods bound to the node.
	// DaemonSetRequested is the total amount of resources that have been requested by daemon sets.  This allows users
	// of the Node to identify the remaining resources that we expect future daemonsets to consume.  This is already
	// included in the calculation for Available.
	DaemonSetRequested v1.ResourceList
	DaemonSetLimits    v1.ResourceList
	// HostPort usage of all pods that are bound to the node
	HostPortUsage *scheduling.HostPortUsage
	VolumeUsage   *scheduling.VolumeLimits
	VolumeLimits  scheduling.VolumeCount

	podRequests map[types.NamespacedName]v1.ResourceList
	podLimits   map[types.NamespacedName]v1.ResourceList

	// PodTotalRequests is the total resources on pods scheduled to this node
	PodTotalRequests v1.ResourceList
	// PodTotalLimits is the total resource limits scheduled to this node
	PodTotalLimits v1.ResourceList
}

// GetBoundPods returns the pods bound to this node
func (n *Node) GetBoundPods() sets.String {
	boundPods := sets.NewString()
	for pod := range n.podRequests {
		boundPods.Insert(pod.String())
	}
	return boundPods
}

// ForPodsWithAntiAffinity calls the supplied function once for each pod with required anti affinity terms that is
// currently bound to a node. The pod returned may not be up-to-date with respect to status, however since the
// anti-affinity terms can't be modified, they will be correct.
func (c *Cluster) ForPodsWithAntiAffinity(fn func(p *v1.Pod, n *v1.Node) bool) {
	c.antiAffinityPods.Range(func(key, value interface{}) bool {
		pod := value.(*v1.Pod)
		c.mu.RLock()
		defer c.mu.RUnlock()
		nodeName, ok := c.bindings[client.ObjectKeyFromObject(pod)]
		if !ok {
			return true
		}
		node, ok := c.nodes[nodeName]
		if !ok {
			// if we receive the node deletion event before the pod deletion event, this can happen
			return true
		}
		return fn(pod, node.Node)
	})
}

// ForEachNode calls the supplied function once per node object that is being tracked. It is not safe to store the
// state.Node object, it should be only accessed from within the function provided to this method.
func (c *Cluster) ForEachNode(f func(n *Node) bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var nodes []*Node
	for _, node := range c.nodes {
		nodes = append(nodes, node)
	}
	// sort nodes by creation time so we provide a consistent ordering
	sort.Slice(nodes, func(a, b int) bool {
		if nodes[a].Node.CreationTimestamp != nodes[b].Node.CreationTimestamp {
			return nodes[a].Node.CreationTimestamp.Time.Before(nodes[b].Node.CreationTimestamp.Time)
		}
		// sometimes we get nodes created in the same second, so sort again by node UID to provide a consistent ordering
		return nodes[a].Node.UID < nodes[b].Node.UID
	})

	for _, node := range nodes {
		if !f(node) {
			return
		}
	}
}

// IsNodeNominated returns true if the given node was expected to have a pod bound to it during a recent scheduling
// batch
func (c *Cluster) IsNodeNominated(nodeName string) bool {
	_, exists := c.nominatedNodes.Get(nodeName)
	return exists
}

// NominateNodeForPod records that a node was the target of a pending pod during a scheduling batch
func (c *Cluster) NominateNodeForPod(nodeName string) {
	c.nominatedNodes.SetDefault(nodeName, nil)
}

// newNode always returns a node, even if some portion of the update has failed
func (c *Cluster) newNode(ctx context.Context, node *v1.Node) (*Node, error) {
	n := &Node{
		Node:          node,
		Capacity:      v1.ResourceList{},
		Allocatable:   v1.ResourceList{},
		Available:     v1.ResourceList{},
		HostPortUsage: scheduling.NewHostPortUsage(),
		VolumeUsage:   scheduling.NewVolumeLimits(c.kubeClient),
		VolumeLimits:  scheduling.VolumeCount{},
		podRequests:   map[types.NamespacedName]v1.ResourceList{},
		podLimits:     map[types.NamespacedName]v1.ResourceList{},
	}
	if err := multierr.Combine(
		c.populateCapacity(ctx, node, n),
		c.populateVolumeLimits(ctx, node, n),
		c.populateResourceRequests(ctx, node, n),
	); err != nil {
		return nil, err
	}
	return n, nil
}

// nolint:gocyclo
func (c *Cluster) populateCapacity(ctx context.Context, node *v1.Node, n *Node) error {
	// Use node's values if initialized
	if node.Labels[v1alpha5.LabelNodeInitialized] == "true" {
		n.Allocatable = node.Status.Allocatable
		n.Capacity = node.Status.Capacity
		return nil
	}
	// Fallback to instance type capacity otherwise
	provisioner := &v1alpha5.Provisioner{}
	// In flight nodes not owned by karpenter are not included in calculations
	if _, ok := node.Labels[v1alpha5.ProvisionerNameLabelKey]; !ok {
		return nil
	}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: node.Labels[v1alpha5.ProvisionerNameLabelKey]}, provisioner); err != nil {
		if errors.IsNotFound(err) {
			// Nodes that are not owned by an existing provisioner are not included in calculations
			return nil
		}
		return fmt.Errorf("getting provisioner, %w", err)
	}
	instanceTypes, err := c.cloudProvider.GetInstanceTypes(ctx, provisioner)
	if err != nil {
		return err
	}
	instanceType, ok := lo.Find(instanceTypes, func(it cloudprovider.InstanceType) bool { return it.Name() == node.Labels[v1.LabelInstanceTypeStable] })
	if !ok {
		return fmt.Errorf("instance type '%s' not found", node.Labels[v1.LabelInstanceTypeStable])
	}
	n.Capacity = instanceType.Resources()

	for k, v := range node.Status.Allocatable {
		n.Allocatable[k] = v
	}
	for resourceName, quantity := range instanceType.Resources() {
		// kubelet will zero out both the capacity and allocatable for an extended resource on startup
		if resources.IsZero(node.Status.Capacity[resourceName]) &&
			resources.IsZero(node.Status.Allocatable[resourceName]) &&
			!quantity.IsZero() {
			n.Allocatable[resourceName] = quantity
		}
	}
	return nil
}

func (c *Cluster) populateResourceRequests(ctx context.Context, node *v1.Node, n *Node) error {
	var pods v1.PodList
	if err := c.kubeClient.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return fmt.Errorf("listing pods, %w", err)
	}
	var requested []v1.ResourceList
	var limits []v1.ResourceList
	var daemonsetRequested []v1.ResourceList
	var daemonsetLimits []v1.ResourceList
	for i := range pods.Items {
		pod := &pods.Items[i]
		requests := resources.RequestsForPods(pod)
		podLimits := resources.LimitsForPods(pod)
		podKey := client.ObjectKeyFromObject(pod)

		n.podRequests[podKey] = requests
		n.podLimits[podKey] = podLimits
		c.bindings[podKey] = n.Node.Name
		if podutils.IsOwnedByDaemonSet(pod) {
			daemonsetRequested = append(daemonsetRequested, requests)
			daemonsetLimits = append(daemonsetLimits, podLimits)
		}
		requested = append(requested, requests)
		limits = append(limits, podLimits)
		n.HostPortUsage.Add(ctx, pod)
		n.VolumeUsage.Add(ctx, pod)
	}

	n.DaemonSetRequested = resources.Merge(daemonsetRequested...)
	n.DaemonSetLimits = resources.Merge(daemonsetLimits...)
	n.PodTotalRequests = resources.Merge(requested...)
	n.PodTotalLimits = resources.Merge(limits...)
	n.Available = resources.Subtract(n.Allocatable, resources.Merge(requested...))
	return nil
}

func (c *Cluster) populateVolumeLimits(ctx context.Context, node *v1.Node, n *Node) error {
	var csiNode storagev1.CSINode
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: node.Name}, &csiNode); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("getting CSINode to determine volume limit for %s, %w", node.Name, err)
	}

	for _, driver := range csiNode.Spec.Drivers {
		if driver.Allocatable == nil {
			continue
		}
		n.VolumeLimits[driver.Name] = int(aws.Int32Value(driver.Allocatable.Count))
	}
	return nil
}

func (c *Cluster) deleteNode(nodeName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.nodes, nodeName)
}

// updateNode is called for every node reconciliation
func (c *Cluster) updateNode(ctx context.Context, node *v1.Node) error {
	// perform node lookup before we lock so that the slower operation can occur in parallel
	n, err := c.newNode(ctx, node)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		// ensure that the out of date node is forgotten
		delete(c.nodes, node.Name)
		return err
	}
	c.nodes[node.Name] = n
	return nil
}

// deletePod is called when the pod has been deleted
func (c *Cluster) deletePod(podKey types.NamespacedName) {
	c.antiAffinityPods.Delete(podKey)
	c.updateNodeUsageFromPodDeletion(podKey)
}

func (c *Cluster) updateNodeUsageFromPodDeletion(podKey types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()

	nodeName, bindingKnown := c.bindings[podKey]
	if !bindingKnown {
		// we didn't think the pod was bound, so we weren't tracking it and don't need to do anything
		return
	}

	delete(c.bindings, podKey)
	n, ok := c.nodes[nodeName]
	if !ok {
		// we weren't tracking the node yet, so nothing to do
		return
	}
	// pod has been deleted so our available capacity increases by the resources that had been
	// requested by the pod
	n.Available = resources.Merge(n.Available, n.podRequests[podKey])
	n.PodTotalRequests = resources.Subtract(n.PodTotalRequests, n.podRequests[podKey])
	n.PodTotalLimits = resources.Subtract(n.PodTotalLimits, n.podLimits[podKey])
	delete(n.podRequests, podKey)
	delete(n.podLimits, podKey)
	n.HostPortUsage.DeletePod(podKey)
	n.VolumeUsage.DeletePod(podKey)

	// We can't easily track the changes to the DaemonsetRequested here as we no longer have the pod.  We could keep up
	// with this separately, but if a daemonset pod is being deleted, it usually means the node is going down.  In the
	// worst case we will resync to correct this.
}

// updatePod is called every time the pod is reconciled
func (c *Cluster) updatePod(ctx context.Context, pod *v1.Pod) error {
	err := c.updateNodeUsageFromPod(ctx, pod)
	c.updatePodAntiAffinities(pod)
	return err
}

func (c *Cluster) updatePodAntiAffinities(pod *v1.Pod) {
	// We intentionally don't track inverse anti-affinity preferences. We're not
	// required to enforce them so it just adds complexity for very little
	// value. The problem with them comes from the relaxation process, the pod
	// we are relaxing is not the pod with the anti-affinity term.
	if podKey := client.ObjectKeyFromObject(pod); podutils.HasRequiredPodAntiAffinity(pod) {
		c.antiAffinityPods.Store(podKey, pod)
	} else {
		c.antiAffinityPods.Delete(podKey)
	}
}

// updateNodeUsageFromPod is called every time a reconcile event occurs for the pod. If the pods binding has changed
// (unbound to bound), we need to update the resource requests on the node.
func (c *Cluster) updateNodeUsageFromPod(ctx context.Context, pod *v1.Pod) error {
	// nothing to do if the pod isn't bound, checking early allows avoiding unnecessary locking
	if pod.Spec.NodeName == "" {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	podKey := client.ObjectKeyFromObject(pod)
	oldNodeName, bindingKnown := c.bindings[podKey]
	if bindingKnown {
		if oldNodeName == pod.Spec.NodeName {
			// we are already tracking the pod binding, so nothing to update
			return nil
		}
		// the pod has switched nodes, this can occur if a pod name was re-used and it was deleted/re-created rapidly,
		// binding to a different node the second time
		n, ok := c.nodes[oldNodeName]
		if ok {
			// we were tracking the old node, so we need to reduce its capacity by the amount of the pod that has
			// left it
			delete(c.bindings, podKey)
			n.Available = resources.Merge(n.Available, n.podRequests[podKey])
			n.PodTotalRequests = resources.Subtract(n.PodTotalRequests, n.podRequests[podKey])
			n.PodTotalLimits = resources.Subtract(n.PodTotalLimits, n.podLimits[podKey])
			n.HostPortUsage.DeletePod(podKey)
			delete(n.podRequests, podKey)
			delete(n.podLimits, podKey)
		}
	}

	// we have noticed that the pod is bound to a node and didn't know about the binding before
	n, ok := c.nodes[pod.Spec.NodeName]
	if !ok {
		var node v1.Node
		if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, &node); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("getting node, %w", err)
		}

		var err error
		// node didn't exist, but creating it will pick up this newly bound pod as well
		n, err = c.newNode(ctx, &node)
		if err != nil {
			// no need to delete c.nodes[node.Name] as it wasn't stored previously
			return err
		}
		c.nodes[node.Name] = n
		return nil
	}

	// sum the newly bound pod's requests and limits into the existing node and record the binding
	podRequests := resources.RequestsForPods(pod)
	podLimits := resources.LimitsForPods(pod)
	// our available capacity goes down by the amount that the pod had requested
	n.Available = resources.Subtract(n.Available, podRequests)
	n.PodTotalRequests = resources.Merge(n.PodTotalRequests, podRequests)
	n.PodTotalLimits = resources.Merge(n.PodTotalLimits, podLimits)
	// if it's a daemonset, we track what it has requested separately
	if podutils.IsOwnedByDaemonSet(pod) {
		n.DaemonSetRequested = resources.Merge(n.DaemonSetRequested, podRequests)
		n.DaemonSetLimits = resources.Merge(n.DaemonSetRequested, podLimits)
	}
	n.HostPortUsage.Add(ctx, pod)
	n.VolumeUsage.Add(ctx, pod)
	n.podRequests[podKey] = podRequests
	n.podLimits[podKey] = podLimits
	c.bindings[podKey] = n.Node.Name
	return nil
}

// clusterStateSynchronized ensures that our cluster state is aware of at least all of the nodes that our list cache has.
// Since we launch nodes in parallel, we can create many node objects which may not all be reconciled by the cluster
// state before we start trying to schedule again.  In this case, we would over-provision as we weren't aware of the
// inflight nodes.
func (c *Cluster) Synchronized(ctx context.Context) error {
	// collect the nodes known by the kube API server
	var nodes v1.NodeList
	if err := c.kubeClient.List(ctx, &nodes); err != nil {
		return err
	}
	unknownNodes := sets.NewString()
	for _, n := range nodes.Items {
		unknownNodes.Insert(n.Name)
	}
	// delete any that cluster state already knows about
	c.ForEachNode(func(n *Node) bool {
		unknownNodes.Delete(n.Node.Name)
		return true
	})
	// and we're left with nodes which exist, but haven't reconciled with cluster state yet
	if len(unknownNodes) != 0 {
		return fmt.Errorf("%d/%d nodes not yet synchronized", unknownNodes.Len(), len(nodes.Items))
	}
	return nil
}
