//go:build linux
// +build linux

/*
Copyright 2014 The Kubernetes Authors.

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

// Package app does all of the work necessary to configure and run a
// Kubernetes app process.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"time"

	"github.com/google/cadvisor/machine"
	"github.com/google/cadvisor/utils/sysfs"

	v1 "k8s.io/api/core/v1"
	utilsysctl "k8s.io/component-helpers/node/util/sysctl"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/proxy"
	proxyconfigapi "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/kubernetes/pkg/proxy/iptables"
	"k8s.io/kubernetes/pkg/proxy/ipvs"
	utilipset "k8s.io/kubernetes/pkg/proxy/ipvs/ipset"
	utilipvs "k8s.io/kubernetes/pkg/proxy/ipvs/util"
	"k8s.io/kubernetes/pkg/proxy/nftables"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
)

// platformApplyDefaults is called after parsing command-line flags and/or reading the
// config file, to apply platform-specific default values to config.
func (o *Options) platformApplyDefaults(config *proxyconfigapi.KubeProxyConfiguration) {
	if config.Mode == "" {
		o.logger.Info("Using iptables proxy")
		config.Mode = proxyconfigapi.ProxyModeIPTables
	}

	if config.Mode == proxyconfigapi.ProxyModeNFTables && len(config.NodePortAddresses) == 0 {
		config.NodePortAddresses = []string{proxyconfigapi.NodePortAddressesPrimary}
	}

	if config.DetectLocalMode == "" {
		o.logger.V(4).Info("Defaulting detect-local-mode", "localModeClusterCIDR", string(proxyconfigapi.LocalModeClusterCIDR))
		config.DetectLocalMode = proxyconfigapi.LocalModeClusterCIDR
	}
	o.logger.V(2).Info("DetectLocalMode", "localMode", string(config.DetectLocalMode))
}

// platformSetup is called after setting up the ProxyServer, but before creating the
// Proxier. It should fill in any platform-specific fields and perform other
// platform-specific setup.
func (s *ProxyServer) platformSetup(ctx context.Context) error {
	ct := &realConntracker{}
	err := s.setupConntrack(ctx, ct)
	if err != nil {
		return err
	}

	return nil
}

// isIPTablesBased checks whether mode is based on iptables rather than nftables
func isIPTablesBased(mode proxyconfigapi.ProxyMode) bool {
	return mode == proxyconfigapi.ProxyModeIPTables || mode == proxyconfigapi.ProxyModeIPVS
}

// platformCheckSupported is called immediately before creating the Proxier, to check
// what IP families are supported (and whether the configuration is usable at all).
// 验证系统是否支持配置的代理模式:
// 	检查内核模块是否加载
// 	确定可用的 IP 协议族
// 	提供有意义的错误信息
func (s *ProxyServer) platformCheckSupported(ctx context.Context) (ipv4Supported, ipv6Supported, dualStackSupported bool, err error) {
	logger := klog.FromContext(ctx)

	if isIPTablesBased(s.Config.Mode) {
		// Check for the iptables and ip6tables binaries.
		var ipts map[v1.IPFamily]utiliptables.Interface
		ipts, err = utiliptables.NewDualStack()

		ipv4Supported = ipts[v1.IPv4Protocol] != nil
		ipv6Supported = ipts[v1.IPv6Protocol] != nil

		if !ipv4Supported && !ipv6Supported {
			err = fmt.Errorf("iptables is not available on this host : %w", err)
		} else if !ipv4Supported {
			logger.Info("No iptables support for family", "ipFamily", v1.IPv4Protocol, "error", err)
		} else if !ipv6Supported {
			logger.Info("No iptables support for family", "ipFamily", v1.IPv6Protocol, "error", err)
		}
	} else {
		// The nft CLI always supports both families.
		ipv4Supported, ipv6Supported = true, true
	}

	// Check if the OS has IPv6 enabled, by verifying if the IPv6 interfaces are available
	_, errIPv6 := os.Stat("/proc/net/if_inet6")
	if errIPv6 != nil {
		logger.Info("No kernel support for family", "ipFamily", v1.IPv6Protocol)
		ipv6Supported = false
	}

	// The Linux proxies can always support dual-stack if they can support both IPv4
	// and IPv6.
	dualStackSupported = ipv4Supported && ipv6Supported
	return
}

// createProxier creates the proxy.Provider
// 根据 KubeProxyConfiguration 选择并实例化真正执行转发规则的 Proxier 实现（iptables / IPVS / nftables）
func (s *ProxyServer) createProxier(ctx context.Context, config *proxyconfigapi.KubeProxyConfiguration, dualStack, initOnly bool) (proxy.Provider, error) {
	logger := klog.FromContext(ctx)
	var proxier proxy.Provider
	var err error

	// ​准备本地流量探测器根据 KubeProxyConfiguration 中的 LocalMode 配置，选择 LocalDetector
	// 生成一组 DetectLocal 策略（依据 ClusterCIDR、NodeCIDR 等），后面传给各 Proxier 用于判断某流量是否“本地 Pod”
	localDetectors := getLocalDetectors(logger, s.PrimaryIPFamily, config, s.podCIDRs)

	// 根据 config.Mode 选择 backend Proxier
	if config.Mode == proxyconfigapi.ProxyModeIPTables {
		// iptables 实现
		logger.Info("Using iptables Proxier")
		ipts, _ := utiliptables.NewDualStack()

		// 双栈模式
		if dualStack {
			// TODO this has side effects that should only happen when Run() is invoked.
			proxier, err = iptables.NewDualStackProxier(
				ctx,
				ipts,                                // iptables handle (v4+v6)
				utilsysctl.New(),                    // sysctl handle
				config.SyncPeriod.Duration,          // 同步周期
				config.MinSyncPeriod.Duration,       // 最小同步周期
				config.Linux.MasqueradeAll,          // 是否 masquerade，是否对所有出站流量 SNAT
				*config.IPTables.LocalhostNodePorts, // 是否允许 127.0.0.1 命中 NodePort
				int(*config.IPTables.MasqueradeBit), // SNAT mark 位
				localDetectors,                      // 本地流量探测策略
				s.NodeName,                          // 节点名称
				s.NodeIPs,                           // 节点 IP
				s.Recorder,                          // 事件记录器
				s.HealthzServer,                     // 健康检查服务器
				config.NodePortAddresses,            // NodePort 地址 监听 NodePort 地址列表，控制 NodePort 服务监听在哪些本地 IP 上
				initOnly,                            // 是否只初始化
			) // 一次创建两套 tables/链
		} else { // 单栈模式
			// Create a single-stack proxier if and only if the node does not support dual-stack (i.e, no iptables support).

			// TODO this has side effects that should only happen when Run() is invoked.
			proxier, err = iptables.NewProxier(
				ctx,
				s.PrimaryIPFamily,                   // IP 家庭
				ipts[s.PrimaryIPFamily],             // iptables handle
				utilsysctl.New(),                    // sysctl handle
				config.SyncPeriod.Duration,          // 同步周期
				config.MinSyncPeriod.Duration,       // 最小同步周期
				config.Linux.MasqueradeAll,          // 是否 masquerade，是否对所有出站流量 SNAT
				*config.IPTables.LocalhostNodePorts, // 是否允许 127.0.0.1 命中 NodePort
				int(*config.IPTables.MasqueradeBit), // SNAT mark 位
				localDetectors[s.PrimaryIPFamily],   // 本地流量探测策略
				s.NodeName,                          // 节点名称
				s.NodeIPs[s.PrimaryIPFamily],        // 节点 IP
				s.Recorder,                          // 事件记录器
				s.HealthzServer,                     // 健康检查服务器
				config.NodePortAddresses,            // NodePort 地址 监听 NodePort 地址列表
				initOnly,                            // 是否只初始化
			)
		}

		if err != nil {
			return nil, fmt.Errorf("unable to create proxier: %v", err)
		}
	} else if config.Mode == proxyconfigapi.ProxyModeIPVS {
		// IPVS 实现
		ipsetInterface := utilipset.New()
		ipvsInterface := utilipvs.New()
		if err := ipvs.CanUseIPVSProxier(ctx, ipvsInterface, ipsetInterface, config.IPVS.Scheduler); err != nil {
			return nil, fmt.Errorf("can't use the IPVS proxier: %v", err)
		}
		ipts, _ := utiliptables.NewDualStack()

		logger.Info("Using ipvs Proxier")
		if dualStack {
			proxier, err = ipvs.NewDualStackProxier(
				ctx,
				ipts,                                // iptables handle
				ipvsInterface,                       // ipvs handle
				ipsetInterface,                      // ipset handle
				utilsysctl.New(),                    // sysctl handle
				config.SyncPeriod.Duration,          // 同步周期
				config.MinSyncPeriod.Duration,       // 最小同步周期
				config.IPVS.ExcludeCIDRs,            // IPVS 排除 CIDR
				config.IPVS.StrictARP,               // IPVS 严格 ARP
				config.IPVS.TCPTimeout.Duration,     // IPVS TCP 超时
				config.IPVS.TCPFinTimeout.Duration,  // IPVS TCP FIN 超时
				config.IPVS.UDPTimeout.Duration,     // IPVS UDP 超时
				config.Linux.MasqueradeAll,          // 是否 masquerade，是否对所有出站流量 SNAT
				int(*config.IPTables.MasqueradeBit), // SNAT mark 位
				localDetectors,                      // 本地流量探测策略
				s.NodeName,                          // 节点名称
				s.NodeIPs,                           // 节点 IP
				s.Recorder,                          // 事件记录器
				s.HealthzServer,                     // 健康检查服务器
				config.IPVS.Scheduler,               // IPVS 调度器
				config.NodePortAddresses,            // NodePort 地址 监听 NodePort 地址列表
				initOnly,                            // 是否只初始化
			)
		} else {
			proxier, err = ipvs.NewProxier(
				ctx,
				s.PrimaryIPFamily,                   // IP 家庭
				ipts[s.PrimaryIPFamily],             // iptables handle
				ipvsInterface,                       // ipvs handle
				ipsetInterface,                      // ipset handle
				utilsysctl.New(),                    // sysctl handle
				config.SyncPeriod.Duration,          // 同步周期
				config.MinSyncPeriod.Duration,       // 最小同步周期
				config.IPVS.ExcludeCIDRs,            // IPVS 排除 CIDR
				config.IPVS.StrictARP,               // IPVS 严格 ARP
				config.IPVS.TCPTimeout.Duration,     // IPVS TCP 超时
				config.IPVS.TCPFinTimeout.Duration,  // IPVS TCP FIN 超时
				config.IPVS.UDPTimeout.Duration,     // IPVS UDP 超时
				config.Linux.MasqueradeAll,          // 是否 masquerade，是否对所有出站流量 SNAT
				int(*config.IPTables.MasqueradeBit), // SNAT mark 位
				localDetectors[s.PrimaryIPFamily],   // 本地流量探测策略
				s.NodeName,                          // 节点名称
				s.NodeIPs[s.PrimaryIPFamily],        // 节点 IP
				s.Recorder,                          // 事件记录器
				s.HealthzServer,                     // 健康检查服务器
				config.IPVS.Scheduler,               // IPVS 调度器
				config.NodePortAddresses,            // NodePort 地址 监听 NodePort 地址列表
				initOnly,                            // 是否只初始化
			)
		}
		if err != nil {
			return nil, fmt.Errorf("unable to create proxier: %v", err)
		}
	} else if config.Mode == proxyconfigapi.ProxyModeNFTables {
		// nftables 实现，1.26+ 实验
		logger.Info("Using nftables Proxier")

		// ​针对单栈 / 双栈分别构造
		if dualStack {
			// TODO this has side effects that should only happen when Run() is invoked.
			proxier, err = nftables.NewDualStackProxier(
				ctx,
				config.SyncPeriod.Duration,          // 同步周期
				config.MinSyncPeriod.Duration,       // 最小同步周期
				config.Linux.MasqueradeAll,          // 是否 masquerade，是否对所有出站流量 SNAT
				int(*config.NFTables.MasqueradeBit), // SNAT mark 位
				localDetectors,                      // 本地流量探测策略
				s.NodeName,                          // 节点名称
				s.NodeIPs,                           // 节点 IP
				s.Recorder,                          // 事件记录器
				s.HealthzServer,                     // 健康检查服务器
				config.NodePortAddresses,            // NodePort 地址 监听 NodePort 地址列表
				initOnly,                            // 是否只初始化
			)
		} else {
			// Create a single-stack proxier if and only if the node does not support dual-stack
			// TODO this has side effects that should only happen when Run() is invoked.
			proxier, err = nftables.NewProxier(
				ctx,
				s.PrimaryIPFamily,                   // IP 家庭
				config.SyncPeriod.Duration,          // 同步周期
				config.MinSyncPeriod.Duration,       // 最小同步周期
				config.Linux.MasqueradeAll,          // 是否 masquerade，是否对所有出站流量 SNAT
				int(*config.NFTables.MasqueradeBit), // SNAT mark 位
				localDetectors[s.PrimaryIPFamily],   // 本地流量探测策略
				s.NodeName,                          // 节点名称
				s.NodeIPs[s.PrimaryIPFamily],        // 节点 IP
				s.Recorder,                          // 事件记录器
				s.HealthzServer,                     // 健康检查服务器
				config.NodePortAddresses,            // NodePort 地址 监听 NodePort 地址列表
				initOnly,                            // 是否只初始化
			)
		}

		if err != nil {
			return nil, fmt.Errorf("unable to create proxier: %v", err)
		}
	}

	return proxier, nil
}

func (s *ProxyServer) setupConntrack(ctx context.Context, ct Conntracker) error {
	max, err := getConntrackMax(ctx, s.Config.Linux.Conntrack)
	if err != nil {
		return err
	}
	if max > 0 {
		err := ct.SetMax(ctx, max)
		if err != nil {
			if err != errReadOnlySysFS {
				return err
			}
			// errReadOnlySysFS is caused by a known docker issue (https://github.com/docker/docker/issues/24000),
			// the only remediation we know is to restart the docker daemon.
			// Here we'll send an node event with specific reason and message, the
			// administrator should decide whether and how to handle this issue,
			// whether to drain the node and restart docker.  Occurs in other container runtimes
			// as well.
			// TODO(random-liu): Remove this when the docker bug is fixed.
			const message = "CRI error: /sys is read-only: " +
				"cannot modify conntrack limits, problems may arise later (If running Docker, see docker issue #24000)"
			s.Recorder.Eventf(s.NodeRef, nil, v1.EventTypeWarning, err.Error(), "StartKubeProxy", message)
		}
	}

	if s.Config.Linux.Conntrack.TCPEstablishedTimeout != nil && s.Config.Linux.Conntrack.TCPEstablishedTimeout.Duration > 0 {
		timeout := int(s.Config.Linux.Conntrack.TCPEstablishedTimeout.Duration / time.Second)
		if err := ct.SetTCPEstablishedTimeout(ctx, timeout); err != nil {
			return err
		}
	}

	if s.Config.Linux.Conntrack.TCPCloseWaitTimeout != nil && s.Config.Linux.Conntrack.TCPCloseWaitTimeout.Duration > 0 {
		timeout := int(s.Config.Linux.Conntrack.TCPCloseWaitTimeout.Duration / time.Second)
		if err := ct.SetTCPCloseWaitTimeout(ctx, timeout); err != nil {
			return err
		}
	}

	if s.Config.Linux.Conntrack.TCPBeLiberal {
		if err := ct.SetTCPBeLiberal(ctx, 1); err != nil {
			return err
		}
	}

	if s.Config.Linux.Conntrack.UDPTimeout.Duration > 0 {
		timeout := int(s.Config.Linux.Conntrack.UDPTimeout.Duration / time.Second)
		if err := ct.SetUDPTimeout(ctx, timeout); err != nil {
			return err
		}
	}

	if s.Config.Linux.Conntrack.UDPStreamTimeout.Duration > 0 {
		timeout := int(s.Config.Linux.Conntrack.UDPStreamTimeout.Duration / time.Second)
		if err := ct.SetUDPStreamTimeout(ctx, timeout); err != nil {
			return err
		}
	}

	return nil
}

func getConntrackMax(ctx context.Context, config proxyconfigapi.KubeProxyConntrackConfiguration) (int, error) {
	logger := klog.FromContext(ctx)
	if config.MaxPerCore != nil && *config.MaxPerCore > 0 {
		floor := 0
		if config.Min != nil {
			floor = int(*config.Min)
		}
		scaled := int(*config.MaxPerCore) * detectNumCPU()
		if scaled > floor {
			logger.V(3).Info("GetConntrackMax: using scaled conntrack-max-per-core")
			return scaled, nil
		}
		logger.V(3).Info("GetConntrackMax: using conntrack-min")
		return floor, nil
	}
	return 0, nil
}

func detectNumCPU() int {
	// try get numCPU from /sys firstly due to a known issue (https://github.com/kubernetes/kubernetes/issues/99225)
	_, numCPU, err := machine.GetTopology(sysfs.NewRealSysFs())
	if err != nil || numCPU < 1 {
		return goruntime.NumCPU()
	}
	return numCPU
}

// getLocalDetectors 根据配置创建用于检测本地流量的检测器 LocalDetector，支持多种检测模式
func getLocalDetectors(logger klog.Logger, primaryIPFamily v1.IPFamily, config *proxyconfigapi.KubeProxyConfiguration, nodePodCIDRs []string) map[v1.IPFamily]proxyutil.LocalTrafficDetector {
	// 默认不检测本地流量
	localDetectors := map[v1.IPFamily]proxyutil.LocalTrafficDetector{
		v1.IPv4Protocol: proxyutil.NewNoOpLocalDetector(),
		v1.IPv6Protocol: proxyutil.NewNoOpLocalDetector(),
	}

	// 根据配置选择检测模式
	switch config.DetectLocalMode {
	// ClusterCIDR 模式，通过配置的 ClusterCIDR 检测本地流量
	case proxyconfigapi.LocalModeClusterCIDR:
		for family, cidrs := range proxyutil.MapCIDRsByIPFamily(config.DetectLocal.ClusterCIDRs) {
			localDetectors[family] = proxyutil.NewDetectLocalByCIDR(cidrs[0].String())
		}
		if !localDetectors[primaryIPFamily].IsImplemented() {
			logger.Info("Detect-local-mode set to ClusterCIDR, but no cluster CIDR specified for primary IP family", "ipFamily", primaryIPFamily, "clusterCIDRs", config.DetectLocal.ClusterCIDRs)
		}

	// NodeCIDR 模式，通过节点的 PodCIDR 检测本地流量
	case proxyconfigapi.LocalModeNodeCIDR:
		for family, cidrs := range proxyutil.MapCIDRsByIPFamily(nodePodCIDRs) {
			localDetectors[family] = proxyutil.NewDetectLocalByCIDR(cidrs[0].String())
		}
		if !localDetectors[primaryIPFamily].IsImplemented() {
			logger.Info("Detect-local-mode set to NodeCIDR, but no PodCIDR defined at node for primary IP family", "ipFamily", primaryIPFamily, "podCIDRs", nodePodCIDRs)
		}

	// BridgeInterface 模式，通过配置的 BridgeInterface 检测本地流量
	case proxyconfigapi.LocalModeBridgeInterface:
		localDetector := proxyutil.NewDetectLocalByBridgeInterface(config.DetectLocal.BridgeInterface)
		localDetectors[v1.IPv4Protocol] = localDetector
		localDetectors[v1.IPv6Protocol] = localDetector

	// InterfaceNamePrefix 模式，通过配置的 InterfaceNamePrefix 检测本地流量
	case proxyconfigapi.LocalModeInterfaceNamePrefix:
		localDetector := proxyutil.NewDetectLocalByInterfaceNamePrefix(config.DetectLocal.InterfaceNamePrefix)
		localDetectors[v1.IPv4Protocol] = localDetector
		localDetectors[v1.IPv6Protocol] = localDetector

	default:
		logger.Info("Defaulting to no-op detect-local")
	}

	return localDetectors
}

// platformCleanup removes stale kube-proxy rules that can be safely removed. If
// cleanupAndExit is true, it will attempt to remove rules from all known kube-proxy
// modes. If it is false, it will only remove rules that are definitely not in use by the
// currently-configured mode.
func platformCleanup(ctx context.Context, mode proxyconfigapi.ProxyMode, cleanupAndExit bool) error {
	var encounteredError bool

	// Clean up iptables and ipvs rules if switching to nftables, or if cleanupAndExit
	if !isIPTablesBased(mode) || cleanupAndExit {
		encounteredError = iptables.CleanupLeftovers(ctx) || encounteredError
		encounteredError = ipvs.CleanupLeftovers(ctx) || encounteredError
	}

	// Clean up nftables rules when switching to iptables or ipvs, or if cleanupAndExit
	if isIPTablesBased(mode) || cleanupAndExit {
		encounteredError = nftables.CleanupLeftovers(ctx) || encounteredError
	}

	if encounteredError {
		return errors.New("encountered an error while tearing down rules")
	}
	return nil
}
