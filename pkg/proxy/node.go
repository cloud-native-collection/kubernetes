/*
Copyright 2022 The Kubernetes Authors.

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

package proxy

import (
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	v1informers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnode "k8s.io/kubernetes/pkg/util/node"
)

// NodeManager handles the life cycle of kube-proxy based on the NodeIPs and PodCIDRs handles
// node watch events and crashes kube-proxy if there are any changes in NodeIPs or PodCIDRs.
// Note: It only crashes on change on PodCIDR when watchPodCIDRs is set to true.
// NodeManager 处理 kube-proxy 的生命周期，基于 NodeIPs 和 PodCIDRs 处理
// 节点事件并崩溃 kube-proxy 如果 NodeIPs 或 PodCIDRs 发生变化。
// 注意：当 watchPodCIDRs 设置为 true 时，仅在 PodCIDR 发生变化时崩溃。
type NodeManager struct {
	// 节点 Informer，用于监听节点变化
	nodeInformer v1informers.NodeInformer
	// 节点 Listers，用于获取节点信息
	nodeLister corelisters.NodeLister
	// 退出函数，用于在节点变化时退出 kube-proxy
	exitFunc func(exitCode int)
	// 是否监控 PodCIDRs，用于在 PodCIDRs 发生变化时退出 kube-proxy,确保kube-proxy在节点网络配置变化时能够重新加载配置
	watchPodCIDRs bool

	// These are constant after construct time
	// 这些字段在构造后是常量
	nodeIPs  []net.IP
	podCIDRs []string

	mu sync.Mutex
	// 缓存的节点对象，用于获取节点信息
	node *v1.Node
}

// NewNodeManager initializes node informer that selects for the given node, waits for cache sync
// and returns NodeManager after waiting some amount of time for the node object to exist
// and have NodeIPs (and PodCIDRs if watchPodCIDRs is true). Note: for backward compatibility,
// NewNodeManager doesn't return any error if it failed to retrieve NodeIPs and watchPodCIDRs
// is false.
func NewNodeManager(ctx context.Context, client clientset.Interface,
	resyncInterval time.Duration, nodeName string, watchPodCIDRs bool,
) (*NodeManager, error) {
	return newNodeManager(ctx, client, resyncInterval, nodeName, watchPodCIDRs, os.Exit, time.Second, 30*time.Second, 5*time.Minute)
}

// newNodeManager implements NewNodeManager with configurable exit function, poll interval and timeouts.
func newNodeManager(ctx context.Context, client clientset.Interface, resyncInterval time.Duration,
	nodeName string, watchPodCIDRs bool, exitFunc func(int),
	pollInterval, nodeIPsTimeout, podCIDRsTimeout time.Duration,
) (*NodeManager, error) {
	// make an informer that selects for the given node
	// 为给定的节点创建一个 Informer,字段选择器，只关注指定节点名的Node对象
	thisNodeInformerFactory := informers.NewSharedInformerFactoryWithOptions(client, resyncInterval,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("metadata.name", nodeName).String()
		}))
	// 获取Node Informer
	nodeInformer := thisNodeInformerFactory.Core().V1().Nodes()
	// 获取Node Lister
	nodeLister := nodeInformer.Lister()

	// initialize the informer and wait for cache sync
	// 启动Informer并等待缓存同步
	thisNodeInformerFactory.Start(wait.NeverStop)
	if !cache.WaitForNamedCacheSync("node informer cache", ctx.Done(), nodeInformer.Informer().HasSynced) {
		return nil, fmt.Errorf("can not sync node informer")
	}

	// 获取Node信息
	node, nodeIPs, podCIDRs := getNodeInfo(nodeLister, nodeName)

	// 检查NodeIPs是否为空
	if len(nodeIPs) == 0 {
		// wait for the node object to exist and have NodeIPs.
		// 等待Node对象存在并有NodeIPs
		ctx, cancel := context.WithTimeout(ctx, nodeIPsTimeout)
		defer cancel()
		_ = wait.PollUntilContextCancel(ctx, pollInterval, false, func(context.Context) (bool, error) {
			node, nodeIPs, podCIDRs = getNodeInfo(nodeLister, nodeName)
			return len(nodeIPs) != 0, nil
		})
	}

	// 检查PodCIDRs是否为空
	if watchPodCIDRs && len(podCIDRs) == 0 {
		// wait some additional time for the PodCIDRs.
		ctx, cancel := context.WithTimeout(ctx, podCIDRsTimeout)
		defer cancel()
		// 等待PodCIDRs分配
		_ = wait.PollUntilContextCancel(ctx, pollInterval, false, func(context.Context) (bool, error) {
			node, nodeIPs, podCIDRs = getNodeInfo(nodeLister, nodeName)
			return len(podCIDRs) != 0, nil
		})

		if len(podCIDRs) == 0 {
			if node == nil {
				return nil, fmt.Errorf("timeout waiting for node %q to exist", nodeName)
			} else {
				return nil, fmt.Errorf("timeout waiting for PodCIDR allocation on node %q", nodeName)
			}
		}
	}

	// For backward-compatibility, we keep going even if we didn't find a node (in
	// non-watchPodCIDRs mode) or it didn't have IPs.
	// 向后兼容，即使我们没有找到节点（在非watchPodCIDRs模式）或它没有IP，我们也会继续
	if node == nil {
		klog.FromContext(ctx).Error(nil, "Timed out waiting for node %q to exist", nodeName)
	} else if len(nodeIPs) == 0 {
		klog.FromContext(ctx).Error(nil, "Timed out waiting for node %q to be assigned IPs", nodeName)
	}

	// 返回NodeManager
	return &NodeManager{
		nodeInformer:  nodeInformer,
		nodeLister:    nodeLister,
		exitFunc:      exitFunc,
		watchPodCIDRs: watchPodCIDRs,

		node:     node,
		nodeIPs:  nodeIPs,
		podCIDRs: podCIDRs,
	}, nil
}

func getNodeInfo(nodeLister corelisters.NodeLister, nodeName string) (*v1.Node, []net.IP, []string) {
	node, _ := nodeLister.Get(nodeName)
	if node == nil {
		return nil, nil, nil
	}
	nodeIPs, _ := utilnode.GetNodeHostIPs(node)
	return node, nodeIPs, node.Spec.PodCIDRs
}

// NodeIPs returns the NodeIPs polled in NewNodeManager(). (This may be empty if
// NewNodeManager timed out without getting any IPs.)
func (n *NodeManager) NodeIPs() []net.IP {
	return n.nodeIPs
}

// PodCIDRs returns the PodCIDRs polled in NewNodeManager().
func (n *NodeManager) PodCIDRs() []string {
	return n.podCIDRs
}

// Node returns a copy of the latest node object, or nil if the Node has not yet been seen.
func (n *NodeManager) Node() *v1.Node {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.node == nil {
		return nil
	}
	return n.node.DeepCopy()
}

// NodeInformer returns the NodeInformer.
// // OnNodeChange 处理节点的创建和更新事件
func (n *NodeManager) NodeInformer() v1informers.NodeInformer {
	return n.nodeInformer
}

// OnNodeChange is a handler for Node creation and update.
// OnNodeChange 处理节点的创建和更新事件
func (n *NodeManager) OnNodeChange(node *v1.Node) {
	// update the node object
	// 更新节点对象
	n.mu.Lock()
	n.node = node
	n.mu.Unlock()

	// We exit whenever there is a change in PodCIDRs detected initially, and PodCIDRs received
	// on node watch event if the node manager is configured with watchPodCIDRs.
	// 如果配置了 watchPodCIDRs，当检测到 PodCIDRs 变化时退出
	if n.watchPodCIDRs {
		if !reflect.DeepEqual(n.podCIDRs, node.Spec.PodCIDRs) {
			klog.InfoS("PodCIDRs changed for the node",
				"node", klog.KObj(node), "newPodCIDRs", node.Spec.PodCIDRs, "oldPodCIDRs", n.podCIDRs)
			klog.Flush()
			// 退出
			n.exitFunc(1)
		}
	}

	// 获取节点的所有 IP 地址
	nodeIPs, _ := utilnode.GetNodeHostIPs(node)

	// We exit whenever there is a change in NodeIPs detected initially, and NodeIPs received
	// on node watch event.
	// 当检测到节点 IP 变化时退出
	if !reflect.DeepEqual(n.nodeIPs, nodeIPs) {
		klog.InfoS("NodeIPs changed for the node",
			"node", klog.KObj(node), "newNodeIPs", nodeIPs, "oldNodeIPs", n.nodeIPs)
		// FIXME: exit
		// klog.Flush()
		// n.exitFunc(1)
	}
}

// OnNodeDelete is a handler for Node deletes.
func (n *NodeManager) OnNodeDelete(node *v1.Node) {
	klog.InfoS("Node is being deleted", "node", klog.KObj(node))
	// FIXME: exit
	// klog.Flush()
	// n.exitFunc(1)
}

// OnNodeSynced is called after the cache is synced and all pre-existing Nodes have been reported
func (n *NodeManager) OnNodeSynced() {}
