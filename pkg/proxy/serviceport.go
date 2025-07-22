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

package proxy

import (
	"fmt"
	"net"
	"slices"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	apiservice "k8s.io/kubernetes/pkg/api/v1/service"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	netutils "k8s.io/utils/net"
)

// ServicePort is an interface which abstracts information about a service.
type ServicePort interface {
	// String returns service string.  An example format can be: `IP:Port/Protocol`.
	String() string // 返回服务字符串，格式为 `IP:Port/Protocol`
	// ClusterIP returns service cluster IP in net.IP format.
	ClusterIP() net.IP // 返回服务集群IP
	// Port returns service port if present. If return 0 means not present.
	Port() int // 返回服务端口
	// SessionAffinityType returns service session affinity type
	SessionAffinityType() v1.ServiceAffinity // 返回服务会话亲和类型
	// StickyMaxAgeSeconds returns service max connection age
	StickyMaxAgeSeconds() int // 返回服务最大连接年龄
	// ExternalIPs returns service ExternalIPs
	ExternalIPs() []net.IP // 返回服务外部IP列表
	// LoadBalancerVIPs returns service LoadBalancerIPs which are VIP mode
	LoadBalancerVIPs() []net.IP // 返回服务LoadBalancer IP列表
	// Protocol returns service protocol.
	Protocol() v1.Protocol // 返回服务协议
	// LoadBalancerSourceRanges returns service LoadBalancerSourceRanges if present empty array if not
	LoadBalancerSourceRanges() []*net.IPNet // 返回服务LoadBalancer源IP范围
	// HealthCheckNodePort returns service health check node port if present.  If return 0, it means not present.
	HealthCheckNodePort() int // 返回服务健康检查NodePort
	// NodePort returns a service Node port if present. If return 0, it means not present.
	NodePort() int // 返回服务NodePort
	// ExternalPolicyLocal returns if a service has only node local endpoints for external traffic.
	ExternalPolicyLocal() bool // 返回服务外部策略本地
	// InternalPolicyLocal returns if a service has only node local endpoints for internal traffic.
	InternalPolicyLocal() bool // 返回服务内部策略本地
	// ExternallyAccessible returns true if the service port is reachable via something
	// other than ClusterIP (NodePort/ExternalIP/LoadBalancer)
	ExternallyAccessible() bool // 返回服务是否可外部访问
	// UsesClusterEndpoints returns true if the service port ever sends traffic to
	// endpoints based on "Cluster" traffic policy
	UsesClusterEndpoints() bool // 返回服务是否使用集群端点
	// UsesLocalEndpoints returns true if the service port ever sends traffic to
	// endpoints based on "Local" traffic policy
	UsesLocalEndpoints() bool // 返回服务是否使用本地端点
}

// BaseServicePortInfo contains base information that defines a service.
// This could be used directly by proxier while processing services,
// or can be used for constructing a more specific ServiceInfo struct
// defined by the proxier if needed.
type BaseServicePortInfo struct {
	clusterIP                net.IP             // 服务集群IP
	port                     int                // 服务端口
	protocol                 v1.Protocol        // 服务协议
	nodePort                 int                // NodePort端口
	loadBalancerVIPs         []net.IP           // LoadBalancer VIP列表
	sessionAffinityType      v1.ServiceAffinity // 会话亲和类型
	stickyMaxAgeSeconds      int                // 会话保持最大年龄
	externalIPs              []net.IP           // 外部IP列表
	loadBalancerSourceRanges []*net.IPNet       // LoadBalancer源IP范围
	healthCheckNodePort      int                // 健康检查NodePort
	externalPolicyLocal      bool               // 外部策略本地
	internalPolicyLocal      bool               // 内部策略本地
}

var _ ServicePort = &BaseServicePortInfo{}

// String is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) String() string {
	return fmt.Sprintf("%s:%d/%s", bsvcPortInfo.clusterIP, bsvcPortInfo.port, bsvcPortInfo.protocol)
}

// ClusterIP is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) ClusterIP() net.IP {
	return bsvcPortInfo.clusterIP
}

// Port is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) Port() int {
	return bsvcPortInfo.port
}

// SessionAffinityType is part of the ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) SessionAffinityType() v1.ServiceAffinity {
	return bsvcPortInfo.sessionAffinityType
}

// StickyMaxAgeSeconds is part of the ServicePort interface
func (bsvcPortInfo *BaseServicePortInfo) StickyMaxAgeSeconds() int {
	return bsvcPortInfo.stickyMaxAgeSeconds
}

// Protocol is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) Protocol() v1.Protocol {
	return bsvcPortInfo.protocol
}

// LoadBalancerSourceRanges is part of ServicePort interface
func (bsvcPortInfo *BaseServicePortInfo) LoadBalancerSourceRanges() []*net.IPNet {
	return bsvcPortInfo.loadBalancerSourceRanges
}

// HealthCheckNodePort is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) HealthCheckNodePort() int {
	return bsvcPortInfo.healthCheckNodePort
}

// NodePort is part of the ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) NodePort() int {
	return bsvcPortInfo.nodePort
}

// ExternalIPs is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) ExternalIPs() []net.IP {
	return bsvcPortInfo.externalIPs
}

// LoadBalancerVIPs is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) LoadBalancerVIPs() []net.IP {
	return bsvcPortInfo.loadBalancerVIPs
}

// ExternalPolicyLocal is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) ExternalPolicyLocal() bool {
	return bsvcPortInfo.externalPolicyLocal
}

// InternalPolicyLocal is part of ServicePort interface
func (bsvcPortInfo *BaseServicePortInfo) InternalPolicyLocal() bool {
	return bsvcPortInfo.internalPolicyLocal
}

// ExternallyAccessible is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) ExternallyAccessible() bool {
	return bsvcPortInfo.nodePort != 0 || len(bsvcPortInfo.loadBalancerVIPs) != 0 || len(bsvcPortInfo.externalIPs) != 0
}

// UsesClusterEndpoints is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) UsesClusterEndpoints() bool {
	// The service port uses Cluster endpoints if the internal traffic policy is "Cluster",
	// or if it accepts external traffic at all. (Even if the external traffic policy is
	// "Local", we need Cluster endpoints to implement short circuiting.)
	return !bsvcPortInfo.internalPolicyLocal || bsvcPortInfo.ExternallyAccessible()
}

// UsesLocalEndpoints is part of ServicePort interface.
func (bsvcPortInfo *BaseServicePortInfo) UsesLocalEndpoints() bool {
	return bsvcPortInfo.internalPolicyLocal || (bsvcPortInfo.externalPolicyLocal && bsvcPortInfo.ExternallyAccessible())
}

func newBaseServiceInfo(service *v1.Service, ipFamily v1.IPFamily, port *v1.ServicePort) *BaseServicePortInfo {
	externalPolicyLocal := apiservice.ExternalPolicyLocal(service)
	internalPolicyLocal := apiservice.InternalPolicyLocal(service)

	var stickyMaxAgeSeconds int
	if service.Spec.SessionAffinity == v1.ServiceAffinityClientIP {
		// Kube-apiserver side guarantees SessionAffinityConfig won't be nil when session affinity type is ClientIP
		stickyMaxAgeSeconds = int(*service.Spec.SessionAffinityConfig.ClientIP.TimeoutSeconds)
	}

	clusterIP := proxyutil.GetClusterIPByFamily(ipFamily, service)
	info := &BaseServicePortInfo{
		clusterIP:           netutils.ParseIPSloppy(clusterIP),
		port:                int(port.Port),
		protocol:            port.Protocol,
		nodePort:            int(port.NodePort),
		sessionAffinityType: service.Spec.SessionAffinity,
		stickyMaxAgeSeconds: stickyMaxAgeSeconds,
		externalPolicyLocal: externalPolicyLocal,
		internalPolicyLocal: internalPolicyLocal,
	}

	// Filter ExternalIPs to correct IP family
	ipFamilyMap := proxyutil.MapIPsByIPFamily(service.Spec.ExternalIPs)
	info.externalIPs = ipFamilyMap[ipFamily]

	// Filter source ranges to correct IP family. Also deal with the fact that
	// LoadBalancerSourceRanges validation mistakenly allows whitespace padding
	loadBalancerSourceRanges := make([]string, len(service.Spec.LoadBalancerSourceRanges))
	for i, sourceRange := range service.Spec.LoadBalancerSourceRanges {
		loadBalancerSourceRanges[i] = strings.TrimSpace(sourceRange)
	}

	cidrFamilyMap := proxyutil.MapCIDRsByIPFamily(loadBalancerSourceRanges)
	cidrs := cidrFamilyMap[ipFamily]
	// zero-masked cidr means "allow any", which same as the empty loadBalancerSourceRanges.
	if slices.ContainsFunc(cidrs, proxyutil.IsZeroCIDR) {
		cidrs = []*net.IPNet{}
	}
	info.loadBalancerSourceRanges = cidrs

	// Filter Load Balancer Ingress IPs to correct IP family. While proxying load
	// balancers might choose to proxy connections from an LB IP of one family to a
	// service IP of another family, that's irrelevant to kube-proxy, which only
	// creates rules for VIP-style load balancers.
	for _, ing := range service.Status.LoadBalancer.Ingress {
		if ing.IP == "" || !proxyutil.IsVIPMode(ing) {
			continue
		}

		ip := netutils.ParseIPSloppy(ing.IP) // (already verified as an IP-address)
		if ingFamily := proxyutil.GetIPFamilyFromIP(ip); ingFamily == ipFamily {
			info.loadBalancerVIPs = append(info.loadBalancerVIPs, ip)
		}
	}

	if apiservice.NeedsHealthCheck(service) {
		p := service.Spec.HealthCheckNodePort
		if p == 0 {
			klog.ErrorS(nil, "Service has no healthcheck nodeport", "service", klog.KObj(service))
		} else {
			info.healthCheckNodePort = int(p)
		}
	}

	return info
}
