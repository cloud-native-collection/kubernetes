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

package volume

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"

	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/recyclerclient"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
)

type ProbeOperation uint32
type ProbeEvent struct {
	Plugin     VolumePlugin // VolumePlugin that was added/updated/removed. if ProbeEvent.Op is 'ProbeRemove', Plugin should be nil
	PluginName string
	Op         ProbeOperation // The operation to the plugin
}

// CSIVolumePhaseType stores information about CSI volume path.
type CSIVolumePhaseType string

const (
	// Common parameter which can be specified in StorageClass to specify the desired FSType
	// Provisioners SHOULD implement support for this if they are block device based
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Default value depends on the provisioner
	// 通用参数可以存储类指定，所需的FSType provisioners应该实现支持,如果他们是块设备必须是主机操作系统支持的文件系统类型。
	//  如“ext4”、“xfs”、“ntfs”。默认值
	VolumeParameterFSType = "fstype"

	ProbeAddOrUpdate ProbeOperation = 1 << iota
	ProbeRemove
	CSIVolumeStaged    CSIVolumePhaseType = "staged"
	CSIVolumePublished CSIVolumePhaseType = "published"
)

var (
	deprecatedVolumeProviders = map[string]string{
		"kubernetes.io/cinder":    "The Cinder volume provider is deprecated and will be removed in a future release",
		"kubernetes.io/storageos": "The StorageOS volume provider is deprecated and will be removed in a future release",
		"kubernetes.io/quobyte":   "The Quobyte volume provider is deprecated and will be removed in a future release",
		"kubernetes.io/flocker":   "The Flocker volume provider is deprecated and will be removed in a future release",
	}
)

// VolumeOptions contains option information about a volume.
// 包含volume的选项信息
type VolumeOptions struct {
	// The attributes below are required by volume.Provisioner
	// TODO: refactor all of this out of volumes when an admin can configure
	// many kinds of provisioners.
	// 下面的属性是volume必须的，Provieionser

	// Reclamation policy for a persistent volume
	// 删除的卷处理模式
	PersistentVolumeReclaimPolicy v1.PersistentVolumeReclaimPolicy
	// Mount options for a persistent volume
	// PV的挂载选项
	MountOptions []string
	// Suggested PV.Name of the PersistentVolume to provision.
	// This is a generated name guaranteed to be unique in Kubernetes cluster.
	// If you choose not to use it as volume name, ensure uniqueness by either
	// combining it with your value or create unique values of your own.
	// 这是一个自动生成的名字在集群Kubernetes确保是唯一的。
	// 如果你选择不使用它作为卷名,可以结合你给定的值或者由你自己创建的值确保唯一性。
	PVName string
	// PVC is reference to the claim that lead to provisioning of a new PV.
	// Provisioners *must* create a PV that would be matched by this PVC,
	// i.e. with required capacity, accessMode, labels matching PVC.Selector and
	// so on.
	// PVC 提供一个指向新的PV的声明
	// provisioner一定会创建一个匹配指向该pv的pvc,即匹配pvc带有请求的容量，访问模式，标签,选择器等等
	PVC *v1.PersistentVolumeClaim
	// Unique name of Kubernetes cluster.
	// 唯一的Kubernetes集群名
	ClusterName string
	// Tags to attach to the real volume in the cloud provider - e.g. AWS EBS
	// 标签附加到真正的volume云提供商——例如AWS EBS
	CloudTags *map[string]string
	// Volume provisioning parameters from StorageClass
	// 从存储类卷配置参数
	Parameters map[string]string
}

// NodeResizeOptions contain options to be passed for node expansion.
// NodeResizeOptions包含选项为节点通过扩张。
type NodeResizeOptions struct {
	VolumeSpec *Spec

	// DevicePath - location of actual device on the node. In case of CSI
	// this just could be volumeID
	// DevicePath——实际设备节点的位置。在CSI情况下这是可以卷ID
	DevicePath string

	// DeviceMountPath location where device is mounted on the node. If volume type
	// is attachable - this would be global mount path otherwise
	// it would be location where volume was mounted for the pod
	// DeviceMountPath device被挂载在节点的位置。
	//  如果卷类型是可连接的,这将是全球安装路径否则它将已经安装的吊舱位置卷
	DeviceMountPath string

	// DeviceStagingPath stores location where the volume is staged
	// DeviceStagingPath 保存暂时保存的volume位置
	DeviceStagePath string

	NewSize resource.Quantity
	OldSize resource.Quantity

	// CSIVolumePhase contains volume phase on the node
	// CSIVolumePhase 包含节点上volume的阶段
	CSIVolumePhase CSIVolumePhaseType
}

// 动态插件探测器
type DynamicPluginProber interface {
	Init() error

	// If an error occurs, events are undefined.
	// 为定义事件的错误
	Probe() (events []ProbeEvent, err error)
}

// VolumePlugin is an interface to volume plugins that can be used on a
// kubernetes node (e.g. by kubelet) to instantiate and manage volumes.
// volumePlugin是个volume plugin的接口可以被一个kubernetes节点使用（例如kubelet),用来实例化和管理卷
type VolumePlugin interface {
	// Init initializes the plugin.  This will be called exactly once
	// before any New* calls are made - implementations of plugins may
	// depend on this.
	// Init 初始化插件，将会先于任何的NEW*调用运行,插件的实现或许依赖于于此(插件的预备)
	Init(host VolumeHost) error

	// Name returns the plugin's name.  Plugins must use namespaced names
	// such as "example.com/volume" and contain exactly one '/' character.
	// The "kubernetes.io" namespace is reserved for plugins which are
	// bundled with kubernetes.
	// Name 返回插件名.插件必须使用命名空间的命名如"example.com/volume"并包含一个“/”字符,
	// “kubernetes.io"命名空间是留给与kubernetes捆绑的插件。
	GetPluginName() string

	// GetVolumeName returns the name/ID to uniquely identifying the actual
	// backing device, directory, path, etc. referenced by the specified volume
	// spec.
	// For Attachable volumes, this value must be able to be passed back to
	// volume Detach methods to identify the device to act on.
	// If the plugin does not support the given spec, this returns an error.
	// GetVolumeName依赖于传入放入volume spec,返回volume名称/id用于唯一的实际的后端设备，目录，路径等。
	// 对于可挂载的volume,这个值必须能够被回传给volume的卸载方法来确定设备可执行卸载。
	// 如果插件不支持传入的spec,将会返回一个错误
	GetVolumeName(spec *Spec) (string, error)

	// CanSupport tests whether the plugin supports a given volume
	// specification from the API.  The spec pointer should be considered
	// const.
	// CanSupport 测试插件是否支持传入volume spec的api,spec pointer应当是常量 ？？
	CanSupport(spec *Spec) bool

	// RequiresRemount returns true if this plugin requires mount calls to be
	// reexecuted. Atomically updating volumes, like Downward API, depend on
	// this to update the contents of the volume.
	// 如果插件请求RequiresRemount重新执行挂载将会返回true,原子的更新volume,像向下API,取决于这个更新卷的内容。
	RequiresRemount(spec *Spec) bool

	// NewMounter creates a new volume.Mounter from an API specification.
	// Ownership of the spec pointer in *not* transferred.
	// - spec: The v1.Volume spec
	// - pod: The enclosing pod
	// NewMounter创建一个新的volume,挂载从一个spec API。
	// spec指针的所有权不转移。
	// — spec: The v1.Volume spec
	// - pod: The enclosing pod
	NewMounter(spec *Spec, podRef *v1.Pod, opts VolumeOptions) (Mounter, error)

	// NewUnmounter creates a new volume.Unmounter from recoverable state.
	// - name: The volume name, as per the v1.Volume spec.
	// - podUID: The UID of the enclosing pod
	// NewUnmounter 创建一个新卷,从可恢复状态卸载.
	// - 名称: volume名，按照 v1.Volume spec
	// - podUID： 可关闭的pod的UID
	NewUnmounter(name string, podUID types.UID) (Unmounter, error)

	// ConstructVolumeSpec constructs a volume spec based on the given volume name
	// and volumePath. The spec may have incomplete information due to limited
	// information from input. This function is used by volume manager to reconstruct
	// volume spec by reading the volume directories from disk
	// ConstructVolumeSpec 构建一个volume spec基于传入的volum name和voulmePath，spec可能不完整的信息由于输入的信息有限。
	// 此函数由volume manager调用，通过从硬盘上读取volume的目录，重建volume spec
	ConstructVolumeSpec(volumeName, volumePath string) (*Spec, error)

	// SupportsMountOption returns true if volume plugins supports Mount options
	// Specifying mount options in a volume plugin that doesn't support
	// user specified mount options will result in error creating persistent volumes
	// 如果插件支持挂载选项SupportMountOption将回返回true，
	// 在创建pv时指定的挂载选项在volume plugin中不支持将会在结果(pv)中返回一个错误
	SupportsMountOption() bool

	// SupportsBulkVolumeVerification checks if volume plugin type is capable
	// of enabling bulk polling of all nodes. This can speed up verification of
	// attached volumes by quite a bit, but underlying pluging must support it.
	// SupportsBulkVolumeVerification检测volume plugin类型是允许批量轮询所有的节点，
	// 可以加速验证挂载的卷，但底层插件必须由支持
	SupportsBulkVolumeVerification() bool
}

// PersistentVolumePlugin is an extended interface of VolumePlugin and is used
// by volumes that want to provide long term persistence of data
// PersistentVolumePlugin是一个VolumePlugin的拓展接口，
// 用于volume用于提供持久化存储的数据
type PersistentVolumePlugin interface {
	VolumePlugin
	// GetAccessModes describes the ways a given volume can be accessed/mounted.
	// GetAccessModes 描述给定的volume可以访问/安装的方式
	GetAccessModes() []v1.PersistentVolumeAccessMode
}

// RecyclableVolumePlugin is an extended interface of VolumePlugin and is used
// by persistent volumes that want to be recycled before being made available
// again to new claims
// RecyclableVolumePlugin是一个VolumePlugin的拓展接口，在一个新的pvc声明使用前回收pv
type RecyclableVolumePlugin interface {
	VolumePlugin

	// Recycle knows how to reclaim this
	// resource after the volume's release from a PersistentVolumeClaim.
	// Recycle will use the provided recorder to write any events that might be
	// interesting to user. It's expected that caller will pass these events to
	// the PV being recycled.
	// Recycle知道如何重新声明资源在volume从一个pvc释放后,
	// Recycle将使用provided记录有关用户的事件。期望调用者将会通过这些事件将pv回收
	Recycle(pvName string, spec *Spec, eventRecorder recyclerclient.RecycleEventRecorder) error
}

// DeletableVolumePlugin is an extended interface of VolumePlugin and is used
// by persistent volumes that want to be deleted from the cluster after their
// release from a PersistentVolumeClaim.
// DeletableVolumePlugin是一个VolumePlugin的拓展接口，在pvc释放后被用于从集群中删除pv
type DeletableVolumePlugin interface {
	VolumePlugin
	// NewDeleter creates a new volume.Deleter which knows how to delete this
	// resource in accordance with the underlying storage provider after the
	// volume's release from a claim
	// NewDeleter 创建一个新的volume,
	// 当volume从一个声明中释放底层的存储provider后，Deleter知道如何删除资源
	NewDeleter(spec *Spec) (Deleter, error)
}

// ProvisionableVolumePlugin is an extended interface of VolumePlugin and is
// used to create volumes for the cluster.
// ProvisionableVolumePlugin是一个VolumePlugin的拓展接口，用于从集群中创建volume
type ProvisionableVolumePlugin interface {
	VolumePlugin
	// NewProvisioner creates a new volume.Provisioner which knows how to
	// create PersistentVolumes in accordance with the plugin's underlying
	// storage provider
	// 依据底层的存储provider创建pv
	NewProvisioner(options VolumeOptions) (Provisioner, error)
}

// AttachableVolumePlugin is an extended interface of VolumePlugin and is used for volumes that require attachment
// to a node before mounting.
// volumeplugin接口的扩展，用于在挂载volume前请求attachment到节点？？
type AttachableVolumePlugin interface {
	DeviceMountableVolumePlugin
	NewAttacher() (Attacher, error)
	NewDetacher() (Detacher, error)
	// CanAttach tests if provided volume spec is attachable
	// 测试provided volume spec是可连接的
	CanAttach(spec *Spec) (bool, error)
}

// DeviceMountableVolumePlugin is an extended interface of VolumePlugin and is used
// for volumes that requires mount device to a node before binding to volume to pod.
// volumeplugin接口的扩展，在volume绑定到pod前将volume挂载到节点
type DeviceMountableVolumePlugin interface {
	VolumePlugin
	NewDeviceMounter() (DeviceMounter, error)
	NewDeviceUnmounter() (DeviceUnmounter, error)
	GetDeviceMountRefs(deviceMountPath string) ([]string, error)
	// CanDeviceMount determines if device in volume.Spec is mountable
	// 测试provided volume spec是可挂载的
	CanDeviceMount(spec *Spec) (bool, error)
}

// ExpandableVolumePlugin is an extended interface of VolumePlugin and is used for volumes that can be
// expanded via control-plane ExpandVolumeDevice call.
// ExpandableVolumePlugin接口是一个扩展，通过控制层面可以扩展volume   。
type ExpandableVolumePlugin interface {
	VolumePlugin
	ExpandVolumeDevice(spec *Spec, newSize resource.Quantity, oldSize resource.Quantity) (resource.Quantity, error)
	RequiresFSResize() bool
}

// NodeExpandableVolumePlugin is an expanded interface of VolumePlugin and is used for volumes that
// require expansion on the node via NodeExpand call.
// ExpandableVolumePlugin接口是一个volumePlugin接口的扩展，通过NodeExpand调用扩展volume。
type NodeExpandableVolumePlugin interface {
	VolumePlugin
	RequiresFSResize() bool
	// NodeExpand expands volume on given deviceMountPath and returns true if resize is successful.
	// NodeExpand 在非定的deviceMountPath扩展volume,如果调整成功返回true。
	NodeExpand(resizeOptions NodeResizeOptions) (bool, error)
}

// VolumePluginWithAttachLimits is an extended interface of VolumePlugin that restricts number of
// volumes that can be attached to a node.
// 限制volume在一个node可以attach的数量
type VolumePluginWithAttachLimits interface {
	VolumePlugin
	// Return maximum number of volumes that can be attached to a node for this plugin.
	// The key must be same as string returned by VolumeLimitKey function. The returned
	// map may look like:
	//     - { "storage-limits-aws-ebs": 39 }
	//     - { "storage-limits-gce-pd": 10 }
	// A volume plugin may return error from this function - if it can not be used on a given node or not
	// applicable in given environment (where environment could be cloudprovider or any other dependency)
	// For example - calling this function for EBS volume plugin on a GCE node should
	// result in error.
	// The returned values are stored in node allocatable property and will be used
	// by scheduler to determine how many pods with volumes can be scheduled on given node.
	GetVolumeLimits() (map[string]int64, error)
	// Return volume limit key string to be used in node capacity constraints
	// The key must start with prefix storage-limits-. For example:
	//    - storage-limits-aws-ebs
	//    - storage-limits-csi-cinder
	// The key should respect character limit of ResourceName type
	// This function may be called by kubelet or scheduler to identify node allocatable property
	// which stores volumes limits.
	VolumeLimitKey(spec *Spec) string
}

// BlockVolumePlugin is an extend interface of VolumePlugin and is used for block volumes support.
// 用于支持块的volume
type BlockVolumePlugin interface {
	VolumePlugin
	// NewBlockVolumeMapper creates a new volume.BlockVolumeMapper from an API specification.
	// Ownership of the spec pointer in *not* transferred.
	// - spec: The v1.Volume spec
	// - pod: The enclosing pod
	NewBlockVolumeMapper(spec *Spec, podRef *v1.Pod, opts VolumeOptions) (BlockVolumeMapper, error)
	// NewBlockVolumeUnmapper creates a new volume.BlockVolumeUnmapper from recoverable state.
	// - name: The volume name, as per the v1.Volume spec.
	// - podUID: The UID of the enclosing pod
	NewBlockVolumeUnmapper(name string, podUID types.UID) (BlockVolumeUnmapper, error)
	// ConstructBlockVolumeSpec constructs a volume spec based on the given
	// podUID, volume name and a pod device map path.
	// The spec may have incomplete information due to limited information
	// from input. This function is used by volume manager to reconstruct
	// volume spec by reading the volume directories from disk.
	ConstructBlockVolumeSpec(podUID types.UID, volumeName, volumePath string) (*Spec, error)
}

// TODO(#14217)
// As part of the Volume Host refactor we are starting to create Volume Hosts
// for specific hosts. New methods for each specific host can be added here.
// Currently consumers will do type assertions to get the specific type of Volume
// Host; however, the end result should be that specific Volume Hosts are passed
// to the specific functions they are needed in (instead of using a catch-all
// VolumeHost interface)

// KubeletVolumeHost is a Kubelet specific interface that plugins can use to access the kubelet.
type KubeletVolumeHost interface {
	// SetKubeletError lets plugins set an error on the Kubelet runtime status
	// that will cause the Kubelet to post NotReady status with the error message provided
	SetKubeletError(err error)

	// GetInformerFactory returns the informer factory for CSIDriverLister
	GetInformerFactory() informers.SharedInformerFactory
	// CSIDriverLister returns the informer lister for the CSIDriver API Object
	CSIDriverLister() storagelistersv1.CSIDriverLister
	// CSIDriverSynced returns the informer synced for the CSIDriver API Object
	CSIDriversSynced() cache.InformerSynced
	// WaitForCacheSync is a helper function that waits for cache sync for CSIDriverLister
	WaitForCacheSync() error
	// Returns hostutil.HostUtils
	GetHostUtil() hostutil.HostUtils
}

// AttachDetachVolumeHost is a AttachDetach Controller specific interface that plugins can use
// to access methods on the Attach Detach Controller.
type AttachDetachVolumeHost interface {
	// CSINodeLister returns the informer lister for the CSINode API Object
	CSINodeLister() storagelistersv1.CSINodeLister

	// CSIDriverLister returns the informer lister for the CSIDriver API Object
	CSIDriverLister() storagelistersv1.CSIDriverLister

	// VolumeAttachmentLister returns the informer lister for the VolumeAttachment API Object
	VolumeAttachmentLister() storagelistersv1.VolumeAttachmentLister
	// IsAttachDetachController is an interface marker to strictly tie AttachDetachVolumeHost
	// to the attachDetachController
	IsAttachDetachController() bool
}

// VolumeHost is an interface that plugins can use to access the kubelet.
// VolumeHos是一个接口,插件可以通过kubelet使用。
type VolumeHost interface {
	// GetPluginDir returns the absolute path to a directory under which
	// a given plugin may store data.  This directory might not actually
	// exist on disk yet.  For plugin data that is per-pod, see
	// GetPodPluginDir().
	// GetPluginDir 返回一个目录(或许用来存储数据)的绝对路径，这个路径或需不在磁盘上
	// 对于每个pod的插件数据,请参阅GetPodPluginDir()。
	GetPluginDir(pluginName string) string

	// GetVolumeDevicePluginDir returns the absolute path to a directory
	// under which a given plugin may store data.
	// ex. plugins/kubernetes.io/{PluginName}/{DefaultKubeletVolumeDevicesDirName}/{volumePluginDependentPath}/
    // GetVolumeDevicePluginDir 返回一个目录(或许用来存储数据)的绝对路径
    // 例如. plugins/kubernetes.io/{PluginName}/{DefaultKubeletVolumeDevicesDirName}/{volumePluginDependentPath}/
	GetVolumeDevicePluginDir(pluginName string) string

	// GetPodsDir returns the absolute path to a directory where all the pods
	// information is stored
	// GetPodsDir 返回
	GetPodsDir() string

	// GetPodVolumeDir returns the absolute path a directory which
	// represents the named volume under the named plugin for the given
	// pod.  If the specified pod does not exist, the result of this call
	// might not exist.
	GetPodVolumeDir(podUID types.UID, pluginName string, volumeName string) string

	// GetPodPluginDir returns the absolute path to a directory under which
	// a given plugin may store data for a given pod.  If the specified pod
	// does not exist, the result of this call might not exist.  This
	// directory might not actually exist on disk yet.
	GetPodPluginDir(podUID types.UID, pluginName string) string

	// GetPodVolumeDeviceDir returns the absolute path a directory which
	// represents the named plugin for the given pod.
	// If the specified pod does not exist, the result of this call
	// might not exist.
	// ex. pods/{podUid}/{DefaultKubeletVolumeDevicesDirName}/{escapeQualifiedPluginName}/
	GetPodVolumeDeviceDir(podUID types.UID, pluginName string) string

	// GetKubeClient returns a client interface
	GetKubeClient() clientset.Interface

	// NewWrapperMounter finds an appropriate plugin with which to handle
	// the provided spec.  This is used to implement volume plugins which
	// "wrap" other plugins.  For example, the "secret" volume is
	// implemented in terms of the "emptyDir" volume.
	NewWrapperMounter(volName string, spec Spec, pod *v1.Pod, opts VolumeOptions) (Mounter, error)

	// NewWrapperUnmounter finds an appropriate plugin with which to handle
	// the provided spec.  See comments on NewWrapperMounter for more
	// context.
	NewWrapperUnmounter(volName string, spec Spec, podUID types.UID) (Unmounter, error)

	// Get cloud provider from kubelet.
	GetCloudProvider() cloudprovider.Interface

	// Get mounter interface.
	GetMounter(pluginName string) mount.Interface

	// Returns the hostname of the host kubelet is running on
	GetHostName() string

	// Returns host IP or nil in the case of error.
	GetHostIP() (net.IP, error)

	// Returns node allocatable.
	GetNodeAllocatable() (v1.ResourceList, error)

	// Returns a function that returns a secret.
	GetSecretFunc() func(namespace, name string) (*v1.Secret, error)

	// Returns a function that returns a configmap.
	GetConfigMapFunc() func(namespace, name string) (*v1.ConfigMap, error)

	GetServiceAccountTokenFunc() func(namespace, name string, tr *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error)

	DeleteServiceAccountTokenFunc() func(podUID types.UID)

	// Returns an interface that should be used to execute any utilities in volume plugins
	GetExec(pluginName string) exec.Interface

	// Returns the labels on the node
	GetNodeLabels() (map[string]string, error)

	// Returns the name of the node
	GetNodeName() types.NodeName

	// Returns the event recorder of kubelet.
	GetEventRecorder() record.EventRecorder

	// Returns an interface that should be used to execute subpath operations
	GetSubpather() subpath.Interface

	// Returns options to pass for proxyutil filtered dialers.
	GetFilteredDialOptions() *proxyutil.FilteredDialOptions
}

// VolumePluginMgr tracks registered plugins.
// VolumePluginMgr 追踪注册的插件
type VolumePluginMgr struct {
	mutex                     sync.RWMutex
	plugins                   map[string]VolumePlugin //插件
	prober                    DynamicPluginProber //动态插件探测器
	probedPlugins             map[string]VolumePlugin //探测器插件
	loggedDeprecationWarnings sets.String
	Host                      VolumeHost
}

// Spec is an internal representation of a volume.  All API volume types translate to Spec.
// spec是一个volume的内部表示，所有的volume api(接口)类型翻译为spec
type Spec struct {
	Volume                          *v1.Volume
	PersistentVolume                *v1.PersistentVolume
	ReadOnly                        bool
	InlineVolumeSpecForCSIMigration bool
	Migrated                        bool
}

// Name returns the name of either Volume or PersistentVolume, one of which must not be nil.
// 返回volume(或者pv)的名称，并且他们其中至少一个必须nil
func (spec *Spec) Name() string {
	switch {
	case spec.Volume != nil:
		return spec.Volume.Name
	case spec.PersistentVolume != nil:
		return spec.PersistentVolume.Name
	default:
		return ""
	}
}

// IsKubeletExpandable returns true for volume types that can be expanded only by the node
// and not the controller. Currently Flex volume is the only one in this category since
// it is typically not installed on the controller
// 如果一个volume类型只能由node节点(而不是controller)扩容，IsKubeletExpandable将会返回true,
// Flex volume是当前唯一的这种类型，因为没有安装在controller上
func (spec *Spec) IsKubeletExpandable() bool {
	switch {
	case spec.Volume != nil:
		return spec.Volume.FlexVolume != nil
	case spec.PersistentVolume != nil:
		return spec.PersistentVolume.Spec.FlexVolume != nil
	default:
		return false

	}
}

// KubeletExpandablePluginName creates and returns a name for the plugin
// this is used in context on the controller where the plugin lookup fails
// as volume expansion on controller isn't supported, but a plugin name is
// required
// KubeletExpandablePluginName从controller的上下文中创建并返回plugin名称,
// 此plugin不支持通过controller扩展容量，但plugin的名称又是必须的
func (spec *Spec) KubeletExpandablePluginName() string {
	switch {
	case spec.Volume != nil && spec.Volume.FlexVolume != nil:
		return spec.Volume.FlexVolume.Driver
	case spec.PersistentVolume != nil && spec.PersistentVolume.Spec.FlexVolume != nil:
		return spec.PersistentVolume.Spec.FlexVolume.Driver
	default:
		return ""
	}
}

// VolumeConfig is how volume plugins receive configuration.  An instance
// specific to the plugin will be passed to the plugin's
// ProbeVolumePlugins(config) func.  Reasonable defaults will be provided by
// the binary hosting the plugins while allowing override of those default
// values.  Those config values are then set to an instance of VolumeConfig
// and passed to the plugin.
//
// VolumeConfig决定了卷插件如何接收配置，一个实例的specific通过插件的ProbeVolumePlugins函数接受
// 默认值将由二进制插件提供,同时允许覆盖默认值，这些配置值被通过和传递给插件的一个实例。
// Values in VolumeConfig are intended to be relevant to several plugins, but
// not necessarily all plugins.  The preference is to leverage strong typing
// in this struct.  All config items must have a descriptive but non-specific
// name (i.e, RecyclerMinimumTimeout is OK but RecyclerMinimumTimeoutForNFS is
// !OK).  An instance of config will be given directly to the plugin, so
// config names specific to plugins are unneeded and wrongly expose plugins in
// this VolumeConfig struct.
//
// volumeConfigs的值是用于几个相关的插件而不是所有的插件，这个结构体的最好是强类型。
// 所有的配置项必须是描述性名称的不带有特殊的名称(例如RecyclerMinimumTimeout是可接受的但RecyclerMinimumTimeoutForNFS是不可接受的)
// 一个配置的实例将会定向的发给插件，所以配置名称特定于插件是不必要的和错误暴露插件在这卷配置结构。
// OtherAttributes is a map of string values intended for one-off
// configuration of a plugin or config that is only relevant to a single
// plugin.  All values are passed by string and require interpretation by the
// plugin. Passing config as strings is the least desirable option but can be
// used for truly one-off configuration. The binary should still use strong
// typing for this value when binding CLI values before they are passed as
// strings in OtherAttributes.
// OtherAttributes是map string结构用于一次性配置的插件或相关配置相关的单例插件。所有的值都是通过字符串,需要由插件解析
// 通过字符串作为配置是最理想的选择,而是真正可用于一次性配置。二进制在绑定cli参数是应当还是使用强类型，以字符串传递到OtherAttributes
type VolumeConfig struct {
	// RecyclerPodTemplate is pod template that understands how to scrub clean
	// a persistent volume after its release. The template is used by plugins
	// which override specific properties of the pod in accordance with that
	// plugin. See NewPersistentVolumeRecyclerPodTemplate for the properties
	// that are expected to be overridden.
	// RecyclerPodTemplate是pod的模版用于清除释放后的pv,pod依据该插件，插件将使用该模版覆写spec的属性
	// NewPersistentVolumeRecyclerPodTemplate被用于这些属性的覆写
	RecyclerPodTemplate *v1.Pod

	// RecyclerMinimumTimeout is the minimum amount of time in seconds for the
	// recycler pod's ActiveDeadlineSeconds attribute. Added to the minimum
	// timeout is the increment per Gi of capacity.
	// RecyclerMinimumTimeout是最少的时间以秒为单位的回收pod的ActiveDeadlineSeconds属性。添加到最小超时的增量/Gi是能力。
	RecyclerMinimumTimeout int

	// RecyclerTimeoutIncrement is the number of seconds added to the recycler
	// pod's ActiveDeadlineSeconds for each Gi of capacity in the persistent
	// volume. Example: 5Gi volume x 30s increment = 150s + 30s minimum = 180s
	// ActiveDeadlineSeconds for recycler pod
	// RecyclerTimeoutIncrement 按持久化卷的容量计算每G增加的pod的回收器ActiveDeadlineSeconds
	// 例如: 5Gi容量*30s增量=150s+30s最小量=180s的ActiveDeadlineSeconds pod 回收器
	RecyclerTimeoutIncrement int

	// PVName is name of the PersistentVolume instance that is being recycled.
	// It is used to generate unique recycler pod name.
	// PVName 被回收pv实例的名称。它是用于生成唯一的recycler pod 名。
	PVName string

	// OtherAttributes stores config as strings.  These strings are opaque to
	// the system and only understood by the binary hosting the plugin and the
	// plugin itself.
	// OtherAttributes 以字符串的形式保存配置，这些字符串对于系统不是透明的，只有二进制部署的插件和插件本身理解。
	OtherAttributes map[string]string

	// ProvisioningEnabled configures whether provisioning of this plugin is
	// enabled or not. Currently used only in host_path plugin.
	// ProvisioningEnabled 配置provisioning是否启用，当前只用host_path 插件被使用
	ProvisioningEnabled bool
}

// NewSpecFromVolume creates an Spec from an v1.Volume
// 从volume创建spec
func NewSpecFromVolume(vs *v1.Volume) *Spec {
	return &Spec{
		Volume: vs,
	}
}

// NewSpecFromPersistentVolume creates an Spec from an v1.PersistentVolume
// 从pv中创建spec
func NewSpecFromPersistentVolume(pv *v1.PersistentVolume, readOnly bool) *Spec {
	return &Spec{
		PersistentVolume: pv,
		ReadOnly:         readOnly,
	}
}

// InitPlugins initializes each plugin.  All plugins must have unique names.
// This must be called exactly once before any New* methods are called on any
// plugins.
// 初始化插件，所有的插件都必须有唯一的名，必须在任意插件的new*方法被调用前调用一次
func (pm *VolumePluginMgr) InitPlugins(plugins []VolumePlugin, prober DynamicPluginProber, host VolumeHost) error {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()

	pm.Host = host
	pm.loggedDeprecationWarnings = sets.NewString()

	if prober == nil {
		// Use a dummy prober to prevent nil deference.
		// 使用一个虚拟探测器以防止nil。
		pm.prober = &dummyPluginProber{}
	} else {
		pm.prober = prober
	}
	if err := pm.prober.Init(); err != nil {
		// Prober init failure should not affect the initialization of other plugins.
		// 探测器init失败不应该影响其他插件的初始化。
		klog.ErrorS(err, "Error initializing dynamic plugin prober")
		pm.prober = &dummyPluginProber{}
	}

	if pm.plugins == nil {
		pm.plugins = map[string]VolumePlugin{}
	}
	//
	if pm.probedPlugins == nil {
		pm.probedPlugins = map[string]VolumePlugin{}
	}

	allErrs := []error{}
	for _, plugin := range plugins {
		name := plugin.GetPluginName()
		if errs := validation.IsQualifiedName(name); len(errs) != 0 {
			allErrs = append(allErrs, fmt.Errorf("volume plugin has invalid name: %q: %s", name, strings.Join(errs, ";")))
			continue
		}

		if _, found := pm.plugins[name]; found {
			allErrs = append(allErrs, fmt.Errorf("volume plugin %q was registered more than once", name))
			continue
		}
		err := plugin.Init(host)
		if err != nil {
			klog.ErrorS(err, "Failed to load volume plugin", "pluginName", name)
			allErrs = append(allErrs, err)
			continue
		}
		pm.plugins[name] = plugin
		klog.V(1).InfoS("Loaded volume plugin", "pluginName", name)
	}
	return utilerrors.NewAggregate(allErrs)
}

// 初始化探测器插件
func (pm *VolumePluginMgr) initProbedPlugin(probedPlugin VolumePlugin) error {
	name := probedPlugin.GetPluginName()
	if errs := validation.IsQualifiedName(name); len(errs) != 0 {
		return fmt.Errorf("volume plugin has invalid name: %q: %s", name, strings.Join(errs, ";"))
	}

	err := probedPlugin.Init(pm.Host)
	if err != nil {
		return fmt.Errorf("failed to load volume plugin %s, error: %s", name, err.Error())
	}

	klog.V(1).InfoS("Loaded volume plugin", "pluginName", name)
	return nil
}

// FindPluginBySpec looks for a plugin that can support a given volume
// specification.  If no plugins can support or more than one plugin can
// support it, return error.
// FindPluginBySpec 通过sepc找到支持的插件，若果没有插件支持或者不止一个插件支持返回错误
func (pm *VolumePluginMgr) FindPluginBySpec(spec *Spec) (VolumePlugin, error) {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()

	if spec == nil {
		return nil, fmt.Errorf("could not find plugin because volume spec is nil")
	}

	matches := []VolumePlugin{}
	for _, v := range pm.plugins {
		if v.CanSupport(spec) {
			matches = append(matches, v)
		}
	}

	// 刷新探测器插件
	pm.refreshProbedPlugins()
	for _, plugin := range pm.probedPlugins {
		if plugin.CanSupport(spec) {
			matches = append(matches, plugin)
		}
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("no volume plugin matched")
	}
	if len(matches) > 1 {
		matchedPluginNames := []string{}
		for _, plugin := range matches {
			matchedPluginNames = append(matchedPluginNames, plugin.GetPluginName())
		}
		return nil, fmt.Errorf("multiple volume plugins matched: %s", strings.Join(matchedPluginNames, ","))
	}

	// Issue warning if the matched provider is deprecated
	pm.logDeprecation(matches[0].GetPluginName())
	return matches[0], nil
}

// FindPluginByName fetches a plugin by name or by legacy name.  If no plugin
// is found, returns error.
// FindPluginByName 通过名称或者(遗留的名称)找到一个插件，如果没有插件找到，返回错误
func (pm *VolumePluginMgr) FindPluginByName(name string) (VolumePlugin, error) {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()

	// Once we can get rid of legacy names we can reduce this to a map lookup.
	matches := []VolumePlugin{}
	if v, found := pm.plugins[name]; found {
		matches = append(matches, v)
	}

	pm.refreshProbedPlugins()
	if plugin, found := pm.probedPlugins[name]; found {
		matches = append(matches, plugin)
	}

	if len(matches) == 0 {
		return nil, fmt.Errorf("no volume plugin matched name: %s", name)
	}
	if len(matches) > 1 {
		matchedPluginNames := []string{}
		for _, plugin := range matches {
			matchedPluginNames = append(matchedPluginNames, plugin.GetPluginName())
		}
		return nil, fmt.Errorf("multiple volume plugins matched: %s", strings.Join(matchedPluginNames, ","))
	}

	// Issue warning if the matched provider is deprecated
	pm.logDeprecation(matches[0].GetPluginName())
	return matches[0], nil
}

// logDeprecation logs warning when a deprecated plugin is used.
func (pm *VolumePluginMgr) logDeprecation(plugin string) {
	if detail, ok := deprecatedVolumeProviders[plugin]; ok && !pm.loggedDeprecationWarnings.Has(plugin) {
		klog.Warningf("WARNING: %s built-in volume provider is now deprecated. %s", plugin, detail)
		// Make sure the message is logged only once. It has Warning severity
		// and we don't want to spam the log too much.
		pm.loggedDeprecationWarnings.Insert(plugin)
	}
}

// Check if probedPlugin cache update is required.
// If it is, initialize all probed plugins and replace the cache with them.
func (pm *VolumePluginMgr) refreshProbedPlugins() {
	events, err := pm.prober.Probe()
	if err != nil {
		klog.ErrorS(err, "Error dynamically probing plugins")
		return // Use cached plugins upon failure.
	}

	for _, event := range events {
		if event.Op == ProbeAddOrUpdate {
			if err := pm.initProbedPlugin(event.Plugin); err != nil {
				klog.ErrorS(err, "Error initializing dynamically probed plugin",
					"pluginName", event.Plugin.GetPluginName())
				continue
			}
			pm.probedPlugins[event.Plugin.GetPluginName()] = event.Plugin
		} else if event.Op == ProbeRemove {
			// Plugin is not available on ProbeRemove event, only PluginName
			delete(pm.probedPlugins, event.PluginName)
		} else {
			klog.ErrorS(nil, "Unknown Operation on PluginName.",
				"pluginName", event.Plugin.GetPluginName())
		}
	}
}

// ListVolumePluginWithLimits returns plugins that have volume limits on nodes
func (pm *VolumePluginMgr) ListVolumePluginWithLimits() []VolumePluginWithAttachLimits {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()

	matchedPlugins := []VolumePluginWithAttachLimits{}
	for _, v := range pm.plugins {
		if plugin, ok := v.(VolumePluginWithAttachLimits); ok {
			matchedPlugins = append(matchedPlugins, plugin)
		}
	}
	return matchedPlugins
}

// FindPersistentPluginBySpec looks for a persistent volume plugin that can
// support a given volume specification.  If no plugin is found, return an
// error
func (pm *VolumePluginMgr) FindPersistentPluginBySpec(spec *Spec) (PersistentVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, fmt.Errorf("could not find volume plugin for spec: %#v", spec)
	}
	if persistentVolumePlugin, ok := volumePlugin.(PersistentVolumePlugin); ok {
		return persistentVolumePlugin, nil
	}
	return nil, fmt.Errorf("no persistent volume plugin matched")
}

// FindVolumePluginWithLimitsBySpec returns volume plugin that has a limit on how many
// of them can be attached to a node
func (pm *VolumePluginMgr) FindVolumePluginWithLimitsBySpec(spec *Spec) (VolumePluginWithAttachLimits, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, fmt.Errorf("could not find volume plugin for spec : %#v", spec)
	}

	if limitedPlugin, ok := volumePlugin.(VolumePluginWithAttachLimits); ok {
		return limitedPlugin, nil
	}
	return nil, fmt.Errorf("no plugin with limits found")
}

// FindPersistentPluginByName fetches a persistent volume plugin by name.  If
// no plugin is found, returns error.
func (pm *VolumePluginMgr) FindPersistentPluginByName(name string) (PersistentVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}
	if persistentVolumePlugin, ok := volumePlugin.(PersistentVolumePlugin); ok {
		return persistentVolumePlugin, nil
	}
	return nil, fmt.Errorf("no persistent volume plugin matched")
}

// FindRecyclablePluginByName fetches a persistent volume plugin by name.  If
// no plugin is found, returns error.
func (pm *VolumePluginMgr) FindRecyclablePluginBySpec(spec *Spec) (RecyclableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if recyclableVolumePlugin, ok := volumePlugin.(RecyclableVolumePlugin); ok {
		return recyclableVolumePlugin, nil
	}
	return nil, fmt.Errorf("no recyclable volume plugin matched")
}

// FindProvisionablePluginByName fetches  a persistent volume plugin by name.  If
// no plugin is found, returns error.
func (pm *VolumePluginMgr) FindProvisionablePluginByName(name string) (ProvisionableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}
	if provisionableVolumePlugin, ok := volumePlugin.(ProvisionableVolumePlugin); ok {
		return provisionableVolumePlugin, nil
	}
	return nil, fmt.Errorf("no provisionable volume plugin matched")
}

// FindDeletablePluginBySpec fetches a persistent volume plugin by spec.  If
// no plugin is found, returns error.
func (pm *VolumePluginMgr) FindDeletablePluginBySpec(spec *Spec) (DeletableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if deletableVolumePlugin, ok := volumePlugin.(DeletableVolumePlugin); ok {
		return deletableVolumePlugin, nil
	}
	return nil, fmt.Errorf("no deletable volume plugin matched")
}

// FindDeletablePluginByName fetches a persistent volume plugin by name.  If
// no plugin is found, returns error.
func (pm *VolumePluginMgr) FindDeletablePluginByName(name string) (DeletableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}
	if deletableVolumePlugin, ok := volumePlugin.(DeletableVolumePlugin); ok {
		return deletableVolumePlugin, nil
	}
	return nil, fmt.Errorf("no deletable volume plugin matched")
}

// FindCreatablePluginBySpec fetches a persistent volume plugin by name.  If
// no plugin is found, returns error.
func (pm *VolumePluginMgr) FindCreatablePluginBySpec(spec *Spec) (ProvisionableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if provisionableVolumePlugin, ok := volumePlugin.(ProvisionableVolumePlugin); ok {
		return provisionableVolumePlugin, nil
	}
	return nil, fmt.Errorf("no creatable volume plugin matched")
}

// FindAttachablePluginBySpec fetches a persistent volume plugin by spec.
// Unlike the other "FindPlugin" methods, this does not return error if no
// plugin is found.  All volumes require a mounter and unmounter, but not
// every volume will have an attacher/detacher.
func (pm *VolumePluginMgr) FindAttachablePluginBySpec(spec *Spec) (AttachableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if attachableVolumePlugin, ok := volumePlugin.(AttachableVolumePlugin); ok {
		if canAttach, err := attachableVolumePlugin.CanAttach(spec); err != nil {
			return nil, err
		} else if canAttach {
			return attachableVolumePlugin, nil
		}
	}
	return nil, nil
}

// FindAttachablePluginByName fetches an attachable volume plugin by name.
// Unlike the other "FindPlugin" methods, this does not return error if no
// plugin is found.  All volumes require a mounter and unmounter, but not
// every volume will have an attacher/detacher.
func (pm *VolumePluginMgr) FindAttachablePluginByName(name string) (AttachableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}
	if attachablePlugin, ok := volumePlugin.(AttachableVolumePlugin); ok {
		return attachablePlugin, nil
	}
	return nil, nil
}

// FindDeviceMountablePluginBySpec fetches a persistent volume plugin by spec.
func (pm *VolumePluginMgr) FindDeviceMountablePluginBySpec(spec *Spec) (DeviceMountableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if deviceMountableVolumePlugin, ok := volumePlugin.(DeviceMountableVolumePlugin); ok {
		if canMount, err := deviceMountableVolumePlugin.CanDeviceMount(spec); err != nil {
			return nil, err
		} else if canMount {
			return deviceMountableVolumePlugin, nil
		}
	}
	return nil, nil
}

// FindDeviceMountablePluginByName fetches a devicemountable volume plugin by name.
func (pm *VolumePluginMgr) FindDeviceMountablePluginByName(name string) (DeviceMountableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}
	if deviceMountableVolumePlugin, ok := volumePlugin.(DeviceMountableVolumePlugin); ok {
		return deviceMountableVolumePlugin, nil
	}
	return nil, nil
}

// FindExpandablePluginBySpec fetches a persistent volume plugin by spec.
func (pm *VolumePluginMgr) FindExpandablePluginBySpec(spec *Spec) (ExpandableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		if spec.IsKubeletExpandable() {
			// for kubelet expandable volumes, return a noop plugin that
			// returns success for expand on the controller
			klog.V(4).InfoS("FindExpandablePluginBySpec -> returning noopExpandableVolumePluginInstance", "specName", spec.Name())
			return &noopExpandableVolumePluginInstance{spec}, nil
		}
		klog.V(4).InfoS("FindExpandablePluginBySpec -> err", "specName", spec.Name(), "err", err)
		return nil, err
	}

	if expandableVolumePlugin, ok := volumePlugin.(ExpandableVolumePlugin); ok {
		return expandableVolumePlugin, nil
	}
	return nil, nil
}

// FindExpandablePluginBySpec fetches a persistent volume plugin by name.
func (pm *VolumePluginMgr) FindExpandablePluginByName(name string) (ExpandableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}

	if expandableVolumePlugin, ok := volumePlugin.(ExpandableVolumePlugin); ok {
		return expandableVolumePlugin, nil
	}
	return nil, nil
}

// FindMapperPluginBySpec fetches a block volume plugin by spec.
func (pm *VolumePluginMgr) FindMapperPluginBySpec(spec *Spec) (BlockVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}

	if blockVolumePlugin, ok := volumePlugin.(BlockVolumePlugin); ok {
		return blockVolumePlugin, nil
	}
	return nil, nil
}

// FindMapperPluginByName fetches a block volume plugin by name.
func (pm *VolumePluginMgr) FindMapperPluginByName(name string) (BlockVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}

	if blockVolumePlugin, ok := volumePlugin.(BlockVolumePlugin); ok {
		return blockVolumePlugin, nil
	}
	return nil, nil
}

// FindNodeExpandablePluginBySpec fetches a persistent volume plugin by spec
func (pm *VolumePluginMgr) FindNodeExpandablePluginBySpec(spec *Spec) (NodeExpandableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if fsResizablePlugin, ok := volumePlugin.(NodeExpandableVolumePlugin); ok {
		return fsResizablePlugin, nil
	}
	return nil, nil
}

// FindNodeExpandablePluginByName fetches a persistent volume plugin by name
func (pm *VolumePluginMgr) FindNodeExpandablePluginByName(name string) (NodeExpandableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}

	if fsResizablePlugin, ok := volumePlugin.(NodeExpandableVolumePlugin); ok {
		return fsResizablePlugin, nil
	}

	return nil, nil
}

func (pm *VolumePluginMgr) Run(stopCh <-chan struct{}) {
	kletHost, ok := pm.Host.(KubeletVolumeHost)
	if ok {
		// start informer for CSIDriver
		informerFactory := kletHost.GetInformerFactory()
		informerFactory.Start(stopCh)
		informerFactory.WaitForCacheSync(stopCh)
	}
}

// NewPersistentVolumeRecyclerPodTemplate creates a template for a recycler
// pod.  By default, a recycler pod simply runs "rm -rf" on a volume and tests
// for emptiness.  Most attributes of the template will be correct for most
// plugin implementations.  The following attributes can be overridden per
// plugin via configuration:
//
// 1.  pod.Spec.Volumes[0].VolumeSource must be overridden.  Recycler
//     implementations without a valid VolumeSource will fail.
// 2.  pod.GenerateName helps distinguish recycler pods by name.  Recommended.
//     Default is "pv-recycler-".
// 3.  pod.Spec.ActiveDeadlineSeconds gives the recycler pod a maximum timeout
//     before failing.  Recommended.  Default is 60 seconds.
//
// See HostPath and NFS for working recycler examples
func NewPersistentVolumeRecyclerPodTemplate() *v1.Pod {
	timeout := int64(60)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pv-recycler-",
			Namespace:    metav1.NamespaceDefault,
		},
		Spec: v1.PodSpec{
			ActiveDeadlineSeconds: &timeout,
			RestartPolicy:         v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name: "vol",
					// IMPORTANT!  All plugins using this template MUST
					// override pod.Spec.Volumes[0].VolumeSource Recycler
					// implementations without a valid VolumeSource will fail.
					VolumeSource: v1.VolumeSource{},
				},
			},
			Containers: []v1.Container{
				{
					Name:    "pv-recycler",
					Image:   "busybox:1.27",
					Command: []string{"/bin/sh"},
					Args:    []string{"-c", "test -e /scrub && rm -rf /scrub/..?* /scrub/.[!.]* /scrub/*  && test -z \"$(ls -A /scrub)\" || exit 1"},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "vol",
							MountPath: "/scrub",
						},
					},
				},
			},
		},
	}
	return pod
}

// Check validity of recycle pod template
// List of checks:
// - at least one volume is defined in the recycle pod template
// If successful, returns nil
// if unsuccessful, returns an error.
func ValidateRecyclerPodTemplate(pod *v1.Pod) error {
	if len(pod.Spec.Volumes) < 1 {
		return fmt.Errorf("does not contain any volume(s)")
	}
	return nil
}

type dummyPluginProber struct{}

func (*dummyPluginProber) Init() error                  { return nil }
func (*dummyPluginProber) Probe() ([]ProbeEvent, error) { return nil, nil }
