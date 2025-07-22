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

package ipvs

import (
	"k8s.io/apimachinery/pkg/util/sets"
)

// NetLinkHandle for revoke netlink interface
// 用于管理网络链路的核心接口
type NetLinkHandle interface {
	// EnsureAddressBind checks if address is bound to the interface and, if not, binds it.  If the address is already bound, return true.
	EnsureAddressBind(address, devName string) (exist bool, err error) //// 确保地址绑定到指定网络接口，如果已绑定则返回true
	// UnbindAddress unbind address from the interface
	UnbindAddress(address, devName string) error // 从网络接口解绑指定地址
	// EnsureDummyDevice checks if dummy device is exist and, if not, create one.  If the dummy device is already exist, return true.
	EnsureDummyDevice(devName string) (exist bool, err error) // 确保虚拟设备存在，如果不存在则创建一个
	// DeleteDummyDevice deletes the given dummy device by name.
	DeleteDummyDevice(devName string) error // 删除指定名称的虚拟设备
	// ListBindAddress will list all IP addresses which are bound in a given interface
	ListBindAddress(devName string) ([]string, error) // 列出指定接口上绑定的所有IP地址
	// GetAllLocalAddresses return all local addresses on the node.
	// Only the addresses of the current family are returned.
	// IPv6 link-local and loopback addresses are excluded.
	GetAllLocalAddresses() (sets.Set[string], error) // 返回节点上的所有本地地址
	// GetLocalAddresses return all local addresses for an interface.
	// Only the addresses of the current family are returned.
	// IPv6 link-local and loopback addresses are excluded.
	GetLocalAddresses(dev string) (sets.Set[string], error) // 返回指定接口上的所有本地地址
	// GetAllLocalAddressesExcept return all local addresses on the node, except from the passed dev.
	// This is not the same as to take the diff between GetAllLocalAddresses and GetLocalAddresses
	// since an address can be assigned to many interfaces. This problem raised
	// https://github.com/kubernetes/kubernetes/issues/114815
	GetAllLocalAddressesExcept(dev string) (sets.Set[string], error) // 返回节点上的所有本地地址，除了指定接口
}
