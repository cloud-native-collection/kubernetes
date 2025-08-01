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
	goflag "flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/mux"
	"k8s.io/apiserver/pkg/server/routes"
	"k8s.io/apiserver/pkg/util/compatibility"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/events"
	cliflag "k8s.io/component-base/cli/flag"
	componentbaseconfig "k8s.io/component-base/config"
	"k8s.io/component-base/configz"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/metrics"
	metricsfeatures "k8s.io/component-base/metrics/features"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/prometheus/slis"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	zpagesfeatures "k8s.io/component-base/zpages/features"
	"k8s.io/component-base/zpages/flagz"
	"k8s.io/component-base/zpages/statusz"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/klog/v2"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/proxy/apis"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	proxyconfigscheme "k8s.io/kubernetes/pkg/proxy/apis/config/scheme"
	"k8s.io/kubernetes/pkg/proxy/config"
	"k8s.io/kubernetes/pkg/proxy/healthcheck"
	proxymetrics "k8s.io/kubernetes/pkg/proxy/metrics"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/util/oom"
	netutils "k8s.io/utils/net"
)

const (
	// kubeProxy defines variable used internally when referring to kube-proxy component
	kubeProxy = "kube-proxy"
)

func init() {
	utilruntime.Must(metricsfeatures.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

// proxyRun defines the interface to run a specified ProxyServer
type proxyRun interface {
	Run(ctx context.Context) error
}

// NewProxyCommand creates a *cobra.Command object with default parameters
func NewProxyCommand() *cobra.Command {
	opts := NewOptions()

	cmd := &cobra.Command{
		Use: kubeProxy,
		Long: `The Kubernetes network proxy runs on each node. This
reflects services as defined in the Kubernetes API on each node and can do simple
TCP, UDP, and SCTP stream forwarding or round robin TCP, UDP, and SCTP forwarding across a set of backends.
Service cluster IPs and ports are currently found through Docker-links-compatible
environment variables specifying ports opened by the service proxy. There is an optional
addon that provides cluster DNS for these cluster IPs. The user must create a service
with the apiserver API to configure the proxy.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()

			if err := initForOS(opts.config.Windows.RunAsService); err != nil {
				return fmt.Errorf("failed os init: %w", err)
			}

			if err := opts.Complete(cmd.Flags()); err != nil {
				return fmt.Errorf("failed complete: %w", err)
			}

			logs.InitLogs()
			if err := logsapi.ValidateAndApplyAsField(&opts.config.Logging, utilfeature.DefaultFeatureGate, field.NewPath("logging")); err != nil {
				return fmt.Errorf("initialize logging: %w", err)
			}

			cliflag.PrintFlags(cmd.Flags())

			if err := opts.Validate(); err != nil {
				return fmt.Errorf("failed validate: %w", err)
			}
			// add feature enablement metrics
			utilfeature.DefaultMutableFeatureGate.AddMetrics()
			// 启动代理服务器
			if err := opts.Run(context.Background()); err != nil {
				opts.logger.Error(err, "Error running ProxyServer")
				return err
			}

			return nil
		},
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}

	fs := cmd.Flags()
	opts.AddFlags(fs)
	fs.AddGoFlagSet(goflag.CommandLine) // for --boot-id-file and --machine-id-file

	_ = cmd.MarkFlagFilename("config", "yaml", "yml", "json")

	return cmd
}

// ProxyServer represents all the parameters required to start the Kubernetes proxy server. All
// fields are required.
type ProxyServer struct {
	// 配置和客户端
	Config *kubeproxyconfig.KubeProxyConfiguration

	// Kubernetes 客户端
	Client clientset.Interface
	// 事件广播器
	Broadcaster events.EventBroadcaster
	// 事件记录器
	Recorder events.EventRecorder
	// 节点引用
	NodeRef *v1.ObjectReference
	// 健康检查服务器
	HealthzServer *healthcheck.ProxyHealthServer
	// 节点名称
	NodeName string
	// 主 IP 家族
	PrimaryIPFamily v1.IPFamily
	// 节点 IP 地址
	NodeIPs map[v1.IPFamily]net.IP
	flagz   flagz.Reader

	// 节点 Pod CIDR：仅用于 LocalModeNodeCIDR 模式的 Pod CIDR 列表
	podCIDRs []string // only used for LocalModeNodeCIDR
	// 节点管理器
	NodeManager *proxy.NodeManager

	// Proxier 是 Proxier 的接口，
	Proxier proxy.Provider
}

// newProxyServer creates a ProxyServer based on the given config
// 初始化 ProxyServer 实例
func newProxyServer(ctx context.Context, config *kubeproxyconfig.KubeProxyConfiguration, master string, initOnly bool, flagzReader flagz.Reader) (*ProxyServer, error) {
	logger := klog.FromContext(ctx)

	s := &ProxyServer{
		Config: config,
		flagz:  flagzReader,
	}

	// 1. 注册配置,  用于 /configz HTTP 端点即可返回当前 kube-proxy 配置，
	cz, err := configz.New(kubeproxyconfig.GroupName)
	if err != nil {
		return nil, fmt.Errorf("unable to register configz: %s", err)
	}
	cz.Set(config)

	// 2. 设置隐藏指标
	if len(config.ShowHiddenMetricsForVersion) > 0 {
		metrics.SetShowHidden()
	}

	// 3. 获取节点名称
	s.NodeName, err = nodeutil.GetHostname(config.HostnameOverride)
	if err != nil {
		return nil, err
	}

	// 4. 创建 Kubernetes 客户端
	s.Client, err = createClient(ctx, config.ClientConnection, master)
	if err != nil {
		return nil, err
	}

	// 5. 创建节点管理器，kube-proxy 自己的 informer 管理器，仅 watch 本节点 对象
	// NodeManager makes an informer that selects for the node where this kube-proxy is running
	s.NodeManager, err = proxy.NewNodeManager(ctx, s.Client, s.Config.ConfigSyncPeriod.Duration,
		s.NodeName, s.Config.DetectLocalMode == kubeproxyconfig.LocalModeNodeCIDR)
	if err != nil {
		return nil, err
	}

	// 6. 获取节点 IP节点网络信息并确定主 IPFamily
	rawNodeIPs := s.NodeManager.NodeIPs()
	if len(rawNodeIPs) > 0 {
		logger.Info("Successfully retrieved NodeIPs", "NodeIPs", rawNodeIPs)
	}
	//--bind-address > NodeIPs > 默认环回地址。确定单/双栈及主 IP 家族。
	s.PrimaryIPFamily, s.NodeIPs = detectNodeIPs(ctx, rawNodeIPs, config.BindAddress)
	s.podCIDRs = s.NodeManager.PodCIDRs() // 获取节点 Pod CIDR

	// 7. 如果 NodePortAddresses 设置为 NodePortAddressesPrimary，就自动设置为本节点 IP
	if len(config.NodePortAddresses) == 1 && config.NodePortAddresses[0] == kubeproxyconfig.NodePortAddressesPrimary {
		var nodePortAddresses []string
		if nodeIP := s.NodeIPs[v1.IPv4Protocol]; nodeIP != nil && !nodeIP.IsLoopback() {
			nodePortAddresses = append(nodePortAddresses, fmt.Sprintf("%s/32", nodeIP.String()))
		}
		if nodeIP := s.NodeIPs[v1.IPv6Protocol]; nodeIP != nil && !nodeIP.IsLoopback() {
			nodePortAddresses = append(nodePortAddresses, fmt.Sprintf("%s/128", nodeIP.String()))
		}
		config.NodePortAddresses = nodePortAddresses
	}

	// 8. 初始化事件系统，创建事件广播器，后续调用 s.Recorder.Eventf(...) 可把事件写到 API Serve
	s.Broadcaster = events.NewBroadcaster(&events.EventSinkImpl{Interface: s.Client.EventsV1()})
	s.Recorder = s.Broadcaster.NewRecorder(proxyconfigscheme.Scheme, kubeProxy)

	// 9. 创建节点引用，用于事件中填充 involvedObject 字段，标明事件与哪个 Node 相关
	s.NodeRef = &v1.ObjectReference{
		Kind:      "Node",
		Name:      s.NodeName,
		UID:       types.UID(s.NodeName),
		Namespace: "",
	}

	// 10. 创建健康检查服务器，--healthz-bind-address 设置，就启动 /healthz 端点，间隔 2×SyncPeriod 对自身做健康探针。
	if len(config.HealthzBindAddress) > 0 {
		s.HealthzServer = healthcheck.NewProxyHealthServer(config.HealthzBindAddress, 2*config.SyncPeriod.Duration, s.NodeManager)
	}

	// 11. 初始化平台依赖，如 conntrack
	err = s.platformSetup(ctx)
	if err != nil {
		return nil, err
	}

	// 12. 检查配置是否合理
	err = checkBadConfig(s)
	if err != nil {
		logger.Error(err, "Kube-proxy configuration may be incomplete or incorrect")
	}

	// 13. 检查 IP 家族是否支持，探测内核是否支持 iptables/ipvs/dual-stack
	ipv4Supported, ipv6Supported, dualStackSupported, err := s.platformCheckSupported(ctx)
	if err != nil {
		return nil, err
	} else if (s.PrimaryIPFamily == v1.IPv4Protocol && !ipv4Supported) || (s.PrimaryIPFamily == v1.IPv6Protocol && !ipv6Supported) {
		return nil, fmt.Errorf("no support for primary IP family %q", s.PrimaryIPFamily)
	} else if dualStackSupported {
		logger.Info("kube-proxy running in dual-stack mode", "primary ipFamily", s.PrimaryIPFamily)
	} else {
		logger.Info("kube-proxy running in single-stack mode", "ipFamily", s.PrimaryIPFamily)
	}

	// 14. 检查 IP 配置是否合理，IP 家族相关的细粒度校验，如 IPVS 模式下，
	err, fatal := checkBadIPConfig(s, dualStackSupported)
	if err != nil {
		if fatal {
			return nil, fmt.Errorf("kube-proxy configuration is incorrect: %w", err)
		}
		logger.Error(err, "Kube-proxy configuration may be incomplete or incorrect")
	}

	// 15. 创建 Proxier，Proxier 是 Proxier 的接口，
	s.Proxier, err = s.createProxier(ctx, config, dualStackSupported, initOnly)
	if err != nil {
		return nil, err
	}

	// 16. 完成全部依赖注入和环境检测，返回 ProxyServer，随后在 Run()中启动事件循环、metrics、healthz 等 goroutine 即可
	return s, nil
}

// checkBadConfig checks for bad/deprecated configuation
func checkBadConfig(s *ProxyServer) error {
	var errors []error

	// At this point we haven't seen any actual Services or EndpointSlices, so we
	// don't really know if the cluster is expected to be single- or dual-stack. But
	// we can at least take note of whether there is any explicitly-dual-stack
	// configuration.
	anyDualStackConfig := false
	for _, config := range [][]string{s.Config.DetectLocal.ClusterCIDRs, s.Config.NodePortAddresses, s.Config.IPVS.ExcludeCIDRs, s.podCIDRs} {
		if dual, _ := netutils.IsDualStackCIDRStrings(config); dual {
			anyDualStackConfig = true
			break
		}
	}

	// Warn if NodePortAddresses does not limit connections on all IP families that
	// seem to be in use.
	cidrsByFamily := proxyutil.MapCIDRsByIPFamily(s.Config.NodePortAddresses)
	if len(s.Config.NodePortAddresses) == 0 {
		errors = append(errors, fmt.Errorf("nodePortAddresses is unset; NodePort connections will be accepted on all local IPs. Consider using `--nodeport-addresses primary`"))
	} else if anyDualStackConfig && len(cidrsByFamily[s.PrimaryIPFamily]) == len(s.Config.NodePortAddresses) {
		errors = append(errors, fmt.Errorf("cluster appears to be dual-stack but nodePortAddresses contains only %s addresses; NodePort connections will be accepted on all local %s IPs", s.PrimaryIPFamily, proxyutil.OtherIPFamily(s.PrimaryIPFamily)))
	} else if len(cidrsByFamily[s.PrimaryIPFamily]) == 0 {
		errors = append(errors, fmt.Errorf("cluster appears to be %s-primary but nodePortAddresses contains only %s addresses; NodePort connections will be accepted on all local %s IPs", s.PrimaryIPFamily, proxyutil.OtherIPFamily(s.PrimaryIPFamily), s.PrimaryIPFamily))
	}

	return utilerrors.NewAggregate(errors)
}

// checkBadIPConfig checks for bad configuration relative to s.PrimaryIPFamily.
// Historically, we did not check most of the config options, so we cannot retroactively
// make IP family mismatches in those options be fatal. When we add new options to check
// here, we should make problems with those options be fatal.
func checkBadIPConfig(s *ProxyServer, dualStackSupported bool) (err error, fatal bool) {
	var errors []error
	var badFamily netutils.IPFamily

	if s.PrimaryIPFamily == v1.IPv4Protocol {
		badFamily = netutils.IPv6
	} else {
		badFamily = netutils.IPv4
	}

	var clusterType string
	if dualStackSupported {
		clusterType = fmt.Sprintf("%s-primary", s.PrimaryIPFamily)
	} else {
		clusterType = fmt.Sprintf("%s-only", s.PrimaryIPFamily)
	}

	if badCIDRs(s.Config.DetectLocal.ClusterCIDRs, badFamily) {
		errors = append(errors, fmt.Errorf("cluster is %s but clusterCIDRs contains only IPv%s addresses", clusterType, badFamily))
		if s.Config.DetectLocalMode == kubeproxyconfig.LocalModeClusterCIDR && !dualStackSupported {
			// This has always been a fatal error
			fatal = true
		}
	}

	if badCIDRs(s.podCIDRs, badFamily) {
		errors = append(errors, fmt.Errorf("cluster is %s but node.spec.podCIDRs contains only IPv%s addresses", clusterType, badFamily))
		if s.Config.DetectLocalMode == kubeproxyconfig.LocalModeNodeCIDR {
			// This has always been a fatal error
			fatal = true
		}
	}

	if netutils.IPFamilyOfString(s.Config.Winkernel.SourceVip) == badFamily {
		errors = append(errors, fmt.Errorf("cluster is %s but winkernel.sourceVip is IPv%s", clusterType, badFamily))
	}

	// In some cases, wrong-IP-family is only a problem when the secondary IP family
	// isn't present at all.
	if !dualStackSupported {
		if badCIDRs(s.Config.IPVS.ExcludeCIDRs, badFamily) {
			errors = append(errors, fmt.Errorf("cluster is %s but ipvs.excludeCIDRs contains only IPv%s addresses", clusterType, badFamily))
		}

		if badBindAddress(s.Config.HealthzBindAddress, badFamily) {
			errors = append(errors, fmt.Errorf("cluster is %s but healthzBindAddress is IPv%s", clusterType, badFamily))
		}
		if badBindAddress(s.Config.MetricsBindAddress, badFamily) {
			errors = append(errors, fmt.Errorf("cluster is %s but metricsBindAddress is IPv%s", clusterType, badFamily))
		}
	}

	// Note that s.Config.NodePortAddresses gets checked as part of checkBadConfig()
	// so it doesn't need to be checked here.

	return utilerrors.NewAggregate(errors), fatal
}

// badCIDRs returns true if cidrs is a non-empty list of CIDRs, all of wrongFamily.
func badCIDRs(cidrs []string, wrongFamily netutils.IPFamily) bool {
	if len(cidrs) == 0 {
		return false
	}
	for _, cidr := range cidrs {
		if netutils.IPFamilyOfCIDRString(cidr) != wrongFamily {
			return false
		}
	}
	return true
}

// badBindAddress returns true if bindAddress is an "IP:port" string where IP is a
// non-zero IP of wrongFamily.
func badBindAddress(bindAddress string, wrongFamily netutils.IPFamily) bool {
	if host, _, _ := net.SplitHostPort(bindAddress); host != "" {
		ip := netutils.ParseIPSloppy(host)
		if ip != nil && netutils.IPFamilyOf(ip) == wrongFamily && !ip.IsUnspecified() {
			return true
		}
	}
	return false
}

// createClient creates a kube client from the given config and masterOverride.
// TODO remove masterOverride when CLI flags are removed.
// createClient 根据给定的配置和 masterOverride 创建 Kubernetes 客户端
func createClient(ctx context.Context, config componentbaseconfig.ClientConnectionConfiguration, masterOverride string) (clientset.Interface, error) {
	logger := klog.FromContext(ctx)
	var kubeConfig *rest.Config
	var err error

	// 检查是否指定了 kubeconfig 或 master URL
	if len(config.Kubeconfig) == 0 && len(masterOverride) == 0 {
		logger.Info("Neither kubeconfig file nor master URL was specified, falling back to in-cluster config")
		kubeConfig, err = rest.InClusterConfig()
	} else {
		// This creates a client, first loading any specified kubeconfig
		// file, and then overriding the Master flag, if non-empty.
		// 创建客户端配置，优先使用 kubeconfig 文件，然后覆盖 master URL（如果指定）
		kubeConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: config.Kubeconfig},
			&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: masterOverride}}).ClientConfig()
	}
	if err != nil {
		return nil, err
	}

	// 设置客户端配置
	kubeConfig.AcceptContentTypes = config.AcceptContentTypes
	kubeConfig.ContentType = config.ContentType
	kubeConfig.QPS = config.QPS
	kubeConfig.Burst = int(config.Burst)

	// 创建 Kubernetes 客户端
	client, err := clientset.NewForConfig(kubeConfig)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func serveHealthz(ctx context.Context, hz *healthcheck.ProxyHealthServer, errCh chan error) {
	logger := klog.FromContext(ctx)
	if hz == nil {
		return
	}

	fn := func() {
		err := hz.Run(ctx)
		if err != nil {
			logger.Error(err, "Healthz server failed")
			if errCh != nil {
				errCh <- fmt.Errorf("healthz server failed: %w", err)
				// if in hardfail mode, never retry again
				blockCh := make(chan error)
				<-blockCh
			}
		} else {
			logger.Error(nil, "Healthz server returned without error")
		}
	}
	go wait.Until(fn, 5*time.Second, ctx.Done())
}

func serveMetrics(ctx context.Context, bindAddress string, proxyMode kubeproxyconfig.ProxyMode, enableProfiling bool, flagzReader flagz.Reader, errCh chan error) {
	if len(bindAddress) == 0 {
		return
	}

	proxyMux := mux.NewPathRecorderMux(kubeProxy)
	healthz.InstallHandler(proxyMux)
	slis.SLIMetricsWithReset{}.Install(proxyMux)

	proxyMux.HandleFunc("/proxyMode", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fmt.Fprintf(w, "%s", proxyMode)
	})

	proxyMux.Handle("/metrics", legacyregistry.Handler())

	if enableProfiling {
		routes.Profiling{}.Install(proxyMux)
		routes.DebugFlags{}.Install(proxyMux, "v", routes.StringFlagPutHandler(logs.GlogSetter))
	}

	configz.InstallHandler(proxyMux)

	if flagzReader != nil {
		flagz.Install(proxyMux, "kube-proxy", flagzReader)
	}

	if utilfeature.DefaultFeatureGate.Enabled(zpagesfeatures.ComponentStatusz) {
		statusz.Install(proxyMux, kubeProxy, statusz.NewRegistry(compatibility.DefaultBuildEffectiveVersion()))
	}

	fn := func() {
		var err error
		defer func() {
			if err != nil {
				err = fmt.Errorf("starting metrics server failed: %w", err)
				utilruntime.HandleError(err)
				if errCh != nil {
					errCh <- err
					// if in hardfail mode, never retry again
					blockCh := make(chan error)
					<-blockCh
				}
			}
		}()

		listener, err := netutils.MultiListen(ctx, "tcp", bindAddress)
		if err != nil {
			return
		}

		server := &http.Server{Handler: proxyMux}
		err = server.Serve(listener)
		if err != nil {
			return
		}

	}
	go wait.Until(fn, 5*time.Second, wait.NeverStop)
}

// Run runs the specified ProxyServer.  This should never exit (unless CleanupAndExit is set).
// TODO: At the moment, Run() cannot return a nil error, otherwise it's caller will never exit. Update callers of Run to handle nil errors.
// 启动 kube-proxy 的所有核心功能
func (s *ProxyServer) Run(ctx context.Context) error {
	logger := klog.FromContext(ctx)
	// To help debugging, immediately log version
	logger.Info("Version info", "version", version.Get())

	logger.Info("Golang settings", "GOGC", os.Getenv("GOGC"), "GOMAXPROCS", os.Getenv("GOMAXPROCS"), "GOTRACEBACK", os.Getenv("GOTRACEBACK"))

	// 1. 注册监控指标
	proxymetrics.RegisterMetrics(s.Config.Mode)

	// TODO(vmarmol): Use container config for this.
	var oomAdjuster *oom.OOMAdjuster
	if s.Config.Linux.OOMScoreAdj != nil {
		oomAdjuster = oom.NewOOMAdjuster()
		if err := oomAdjuster.ApplyOOMScoreAdj(0, int(*s.Config.Linux.OOMScoreAdj)); err != nil {
			logger.V(2).Info("Failed to apply OOMScore", "err", err)
		}
	}

	if s.Broadcaster != nil {
		stopCh := make(chan struct{})
		s.Broadcaster.StartRecordingToSink(stopCh)
	}

	// TODO(thockin): make it possible for healthz and metrics to be on the same port.

	var healthzErrCh, metricsErrCh chan error
	if s.Config.BindAddressHardFail {
		healthzErrCh = make(chan error)
		metricsErrCh = make(chan error)
	}

	// Start up a healthz server if requested
	// 2. 启动健康检查服务器
	serveHealthz(ctx, s.HealthzServer, healthzErrCh)

	// Start up a metrics server if requested
	// 3. 启动指标服务器
	serveMetrics(ctx, s.Config.MetricsBindAddress, s.Config.Mode, s.Config.EnableProfiling, s.flagz, metricsErrCh)

	// 4. 构造 LabelSelector 过滤条件，用于稍后 watch Service / EndpointSlice 时排除“不需要 kube-proxy 代理”的 Service
	noProxyName, err := labels.NewRequirement(
		apis.LabelServiceProxyName, // label 键：选择没有 apis.LabelServiceProxyName 标签的 Service，当 Service 设置了这个标签且值不为空时，表示它希望由 自定义的 service-proxy（如 Istio、UserDefinedProxy） 来处理，而不是由内置 kube-proxy 来转发。
		selection.DoesNotExist,     // 条件： 只匹配“没有设置此标签” 的 Service
		nil)                        // 值列表为空，因为我们只关心“是否存在”
	if err != nil {
		return err
	}

	// 排除 Headless Service 对应的对象
	noHeadlessEndpoints, err := labels.NewRequirement(
		v1.IsHeadlessService,   // label 键：kubernetes.io/service-account.name == "headless”，常量值为 service.kubernetes.io/headless。当 Service 的 spec.clusterIP 设为 "None"（即 Headless Service）时，API Server 会自动给该 Service 加上此标签。
		selection.DoesNotExist, // 条件： 只匹配“没有设置此标签” 的 Service，即非-Headless Service。
		nil)                    // Headless Service 通常由 DNS 直接返回 Pod IP，不经 kube-proxy，所以要排除。
	if err != nil {
		return err
	}

	// 5. 构造一个 LabelSelector，用于稍后 watch Service / EndpointSlice 时排除“不需要 kube-proxy 代理”的 Service
	labelSelector := labels.NewSelector()
	labelSelector = labelSelector.Add(*noProxyName, *noHeadlessEndpoints)

	// 6. 构造 informer，用于 watch API Server 对象变化, EndpointSlice、ServiceCIDR
	// Make informers that filter out objects that want a non-default service proxy.
	informerFactory := informers.NewSharedInformerFactoryWithOptions(s.Client, s.Config.ConfigSyncPeriod.Duration,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
		}))

	// Create configs (i.e. Watches for Services, EndpointSlices and ServiceCIDRs)
	// Note: RegisterHandler() calls need to happen before creation of Sources because sources
	// only notify on changes, and the initial update (on process start) may be lost if no handlers
	// are registered yet.
	// don't watch headless services for kube-proxy, they are proxied by DNS.
	// 再建一个 factory，专门 watch Service,Service 需要额外 field 过滤，而 EndpointSlice/ServiceCIDR 不能带该字段，否则 server 不接受；因此拆成两个 cache。
	serviceInformerFactory := informers.NewSharedInformerFactoryWithOptions(s.Client, s.Config.ConfigSyncPeriod.Duration,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector.String()
			options.FieldSelector = fields.OneTermNotEqualSelector("spec.clusterIP", v1.ClusterIPNone).String() // 排除 Headless Service
		}))
	// 7. 把 Service informer 事件（Add/Update/Delete）转成 ServiceUpdate 回调到 Proxier
	serviceConfig := config.NewServiceConfig(ctx, serviceInformerFactory.Core().V1().Services(), s.Config.ConfigSyncPeriod.Duration)
	serviceConfig.RegisterEventHandler(s.Proxier)
	go serviceConfig.Run(ctx.Done())

	// 8. 把 EndpointSlice informer 事件（Add/Update/Delete）转成 EndpointSliceUpdate 回调到 Proxier
	endpointSliceConfig := config.NewEndpointSliceConfig(ctx, informerFactory.Discovery().V1().EndpointSlices(), s.Config.ConfigSyncPeriod.Duration)
	endpointSliceConfig.RegisterEventHandler(s.Proxier)
	go endpointSliceConfig.Run(ctx.Done())

	// 9. 可选）ServiceCIDRConfig 仅当 FeatureGate MultiCIDRServiceAllocator 打开时才 watch ServiceCIDRs（K8s 1.24+ 特性），同样回调 Proxier
	if utilfeature.DefaultFeatureGate.Enabled(features.MultiCIDRServiceAllocator) {
		serviceCIDRConfig := config.NewServiceCIDRConfig(ctx, informerFactory.Networking().V1().ServiceCIDRs(), s.Config.ConfigSyncPeriod.Duration)
		serviceCIDRConfig.RegisterEventHandler(s.Proxier)
		go serviceCIDRConfig.Run(wait.NeverStop)
	}
	// This has to start after the calls to NewServiceConfig because that
	// function must configure its shared informer event handlers first.
	// 启动两个 informer factory，开始 watch API Server 对象变化
	informerFactory.Start(wait.NeverStop)
	serviceInformerFactory.Start(wait.NeverStop)

	// hollow-proxy doesn't need node config, and we don't create nodeManager for hollow-proxy.
	// 10. NodeManager
	if s.NodeManager != nil {
		// 创建 NodeConfig 实例
		nodeConfig := config.NewNodeConfig(ctx, s.NodeManager.NodeInformer(), s.Config.ConfigSyncPeriod.Duration)
		nodeConfig.RegisterEventHandler(s.NodeManager)
		// 创建 NodeTopologyConfig 实例
		nodeTopologyConfig := config.NewNodeTopologyConfig(ctx, s.NodeManager.NodeInformer(), s.Config.ConfigSyncPeriod.Duration)
		nodeTopologyConfig.RegisterEventHandler(s.Proxier)
		// 启动 NodeConfig
		go nodeConfig.Run(wait.NeverStop)
	}

	// Birth Cry after the birth is successful
	// 发送事件，通知 kubelet kube-proxy 已经启动
	s.birthCry()

	// 启动 Proxier 的核心同步循环，开始处理事件循环，
	// iptables 或 ipvs Proxier 会定时同步规则、防抖更新
	go s.Proxier.SyncLoop()

	// 启动阶段完成所有后台组件（指标、healthz、informer、proxier）的初始化后，就只剩两件事：
	// 	1. 后台 goroutine 们持续处理 Service/Endpoints 变化并同步代理规则；
	// 	2. 主线程阻塞等待端口启动失败的错误信号或进程被 kubelet / systemd 终止
	// 阻塞等待健康检查或 metrics 端口启动失败
	select {
	case err = <-healthzErrCh:
		s.Recorder.Eventf(s.NodeRef, nil, api.EventTypeWarning, "FailedToStartProxierHealthcheck", "StartKubeProxy", err.Error())
	case err = <-metricsErrCh:
		s.Recorder.Eventf(s.NodeRef, nil, api.EventTypeWarning, "FailedToStartMetricServer", "StartKubeProxy", err.Error())
	}
	return err
}

func (s *ProxyServer) birthCry() {
	s.Recorder.Eventf(s.NodeRef, nil, api.EventTypeNormal, "Starting", "StartKubeProxy", "")
}

// detectNodeIPs returns the proxier's "node IP" or IPs, and the IP family to use if the
// node turns out to be incapable of dual-stack. (Note that kube-proxy normally runs as
// dual-stack if the backend is capable of supporting both IP families, regardless of
// whether the node is *actually* configured as dual-stack or not.)

// (Note that on Linux, the node IPs are used only to determine whether a given
// LoadBalancerSourceRanges value matches the node or not. In particular, they are *not*
// used for NodePort handling.)
//
// The order of precedence is:
//  1. if bindAddress is not 0.0.0.0 or ::, then it is used as the primary IP.
//  2. if rawNodeIPs is not empty, then its address(es) is/are used
//  3. otherwise the node IPs are 127.0.0.1 and ::1
// detectNodeIPs 返回 proxier 使用的 “节点 IP”(一个或多个)，并在节点无法支持双栈时给出要使用的 IP 家族。
// （注意：只要底层实现同时支持 IPv4 与 IPv6，kube-proxy 默认就按双栈运行，不管节点本身是否真正配置为双栈。）

// （另请注意：在 Linux 上，这些节点 IP 仅用于判断某个 LoadBalancerSourceRanges值是否匹配本节点；尤其 **不用于** NodePort 的处理。）

// 优先级顺序：
//  1. 如果 bindAddress 不是 0.0.0.0 或 :: ，则将其作为主 IP；
//  2. 若 rawNodeIPs 非空，则使用其中的地址；
//  3. 否则使用 127.0.0.1 和 ::1 作为节点 IP。
func detectNodeIPs(ctx context.Context, rawNodeIPs []net.IP, bindAddress string) (v1.IPFamily, map[v1.IPFamily]net.IP) {
	logger := klog.FromContext(ctx)
	// v1.IPFamily：K8s API 类型，可能值 IPv4Protocol 或 IPv6Protocol。
	// primaryFamily：kube-proxy 认定的“主”IP 家族，决定很多默认行为。
	// nodeIPs：以家族为键，存放“本节点在该家族下使用的一条代表 IP”
	// 默认 IPv4
	primaryFamily := v1.IPv4Protocol
	// 默认值
	nodeIPs := map[v1.IPFamily]net.IP{
		v1.IPv4Protocol: net.IPv4(127, 0, 0, 1), // IPv4 塞入环回地址 127.0.0.1 / ::1，
		v1.IPv6Protocol: net.IPv6loopback,       // IPv6 塞入环回地址 ::1
	}

	// rawNodeIPs 非空，使用其中的地址
	// rawNodeIPs 来源：s.NodeManager.NodeIPs() 会读取 Node.Status.Addresses（type=InternalIP、ExternalIP）或通过本机接口探测得到，最多两个元素：
	// 第 0 个：被视为“最好”的地址（通常与 kubelet 同 IP），用于判断 primaryFamily。
	// 第 1 个（若存在）：必定是另一家族，实现双栈
	if len(rawNodeIPs) > 0 {
		// 第 0 个：被视为“最好”的地址（通常与 kubelet 同 IP），用于判断 primaryFamily。
		if !netutils.IsIPv4(rawNodeIPs[0]) {
			primaryFamily = v1.IPv6Protocol
		}
		nodeIPs[primaryFamily] = rawNodeIPs[0]
		// 第 1 个（若存在）：必定是另一家族，实现双栈
		if len(rawNodeIPs) > 1 {
			// If more than one address is returned, they are guaranteed to be of different families
			family := v1.IPv4Protocol
			if !netutils.IsIPv4(rawNodeIPs[1]) {
				family = v1.IPv6Protocol
			}
			nodeIPs[family] = rawNodeIPs[1]
		}
	}

	// 如果 bindAddress 非空，覆盖主 IP,用户手动指定时具有最高优先级。
	bindIP := netutils.ParseIPSloppy(bindAddress)
	if bindIP != nil && !bindIP.IsUnspecified() {
		if netutils.IsIPv4(bindIP) { // bindAddress 是 IPv4
			primaryFamily = v1.IPv4Protocol
		} else { // bindAddress 是 IPv6
			primaryFamily = v1.IPv6Protocol
		}
		nodeIPs[primaryFamily] = bindIP
	}

	// 如果主 IP 是回环地址，日志提示
	if nodeIPs[primaryFamily].IsLoopback() {
		logger.Info("Can't determine this node's IP, assuming loopback; if this is incorrect, please set the --bind-address flag")
	}
	return primaryFamily, nodeIPs
}
