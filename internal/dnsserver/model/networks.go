package model

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"k8s.io/klog"

	"github.com/joyrex2001/kubedock/internal/dnsserver/support"
)

type IPAddress string
type Hostname string
type NetworkId string
type PodName string

// UNKNOWN_IP_PREFIX is used to mark pods whose IP is not yet known. Lookups
// against such IPs are silently ignored.
const UNKNOWN_IP_PREFIX = "unknownip:"

type Pod struct {
	IP          IPAddress
	Namespace   string
	Name        string
	HostAliases []Hostname
	Networks    []NetworkId
	Ready       bool
}

func NewPod(ip IPAddress, namespace string, name string, hostAliases []Hostname,
	networks []NetworkId, ready bool) (*Pod, error) {

	hostAliases = slices.Clone(hostAliases)
	slices.Sort(hostAliases)
	hostAliases = slices.Compact(hostAliases)

	networks = slices.Clone(networks)
	slices.Sort(networks)
	networks = slices.Compact(networks)

	for _, host := range hostAliases {
		if !support.IsValidHostname(string(host)) {
			return nil, fmt.Errorf("%s/%s: Invalid hostname '%s'",
				namespace, name, host)
		}
	}

	return &Pod{
		IP:          ip,
		Namespace:   namespace,
		Name:        name,
		HostAliases: hostAliases,
		Networks:    networks,
		Ready:       ready,
	}, nil
}

func (pod *Pod) Equal(otherPod *Pod) bool {
	return reflect.DeepEqual(pod, otherPod)
}

func (pod *Pod) Copy() *Pod {
	return &Pod{
		IP:          pod.IP,
		Namespace:   pod.Namespace,
		Name:        pod.Name,
		HostAliases: slices.Clone(pod.HostAliases),
		Networks:    slices.Clone(pod.Networks),
		Ready:       pod.Ready,
	}
}

type Network struct {
	Id              NetworkId
	IPToPod         map[IPAddress]*Pod
	HostAliasToPods map[Hostname][]*Pod
}

func NewNetwork(id NetworkId) *Network {
	return &Network{
		Id:              id,
		IPToPod:         make(map[IPAddress]*Pod),
		HostAliasToPods: make(map[Hostname][]*Pod),
	}
}

func (net *Network) Add(pod *Pod) error {
	net.IPToPod[pod.IP] = pod
	for _, hostAlias := range pod.HostAliases {
		net.HostAliasToPods[hostAlias] = append(net.HostAliasToPods[hostAlias], pod)
	}
	return nil
}

// Networks is not thread-safe and is intended to be used via copy-on-write
// (build a new Networks from the current Pods snapshot, then swap atomically).
type NetworkMap map[NetworkId]*Network

type Networks struct {
	NameToNetwork NetworkMap
	IpToNetworks  map[IPAddress]NetworkMap
}

func NewNetworks() *Networks {
	return &Networks{
		NameToNetwork: make(NetworkMap),
		IpToNetworks:  make(map[IPAddress]NetworkMap),
	}
}

type PodError struct {
	Pod *Pod
	Err error
}

func (err *PodError) Error() string {
	return fmt.Sprintf("[%s/%s]: %v", err.Pod.Namespace, err.Pod.Name, err.Err)
}

func NewPodError(pod *Pod, err error) *PodError {
	return &PodError{Pod: pod, Err: err}
}

func (net *Networks) Add(pod *Pod) *PodError {
	if pod.IP == "" {
		klog.Fatalf("Pod IP is not set: %+v", pod)
	}
	if len(pod.Networks) == 0 {
		klog.Fatalf("Pod networks are not set: %+v", pod)
	}

	for _, networkId := range pod.Networks {
		network := net.NameToNetwork[networkId]
		if network == nil {
			network = NewNetwork(networkId)
		}
		if err := network.Add(pod); err != nil {
			return NewPodError(pod, err)
		}
		if net.IpToNetworks[pod.IP] == nil {
			net.IpToNetworks[pod.IP] = make(NetworkMap)
		}
		net.IpToNetworks[pod.IP][networkId] = network
		net.NameToNetwork[networkId] = network
	}
	return nil
}

func (net *Networks) Log() {
	klog.Infof("Network count: %d", len(net.NameToNetwork))
	for networkId, network := range net.NameToNetwork {
		klog.Infof("Network %s", networkId)
		for ip, pod := range network.IPToPod {
			klog.Infof("  Pod: %s/%s ready %v", pod.Namespace, pod.Name, pod.Ready)
			klog.Infof("    IP: %s", ip)
			for _, hostAlias := range pod.HostAliases {
				klog.Infof("    Hostalias: %s", hostAlias)
			}
		}
	}
}

func (net *Networks) Lookup(sourceIp IPAddress, hostname Hostname) []IPAddress {
	res := make([]IPAddress, 0)
	if strings.HasPrefix(string(sourceIp), UNKNOWN_IP_PREFIX) {
		return res
	}
	klog.V(3).Infof("Lookup source ip '%s' host '%s'", sourceIp, hostname)
	networks := net.IpToNetworks[sourceIp]
	if networks == nil {
		return res
	}
	for _, network := range networks {
		for _, pod := range network.HostAliasToPods[hostname] {
			if pod.Ready {
				res = append(res, pod.IP)
			}
		}
	}
	return res
}

func (net *Networks) ReverseLookup(sourceIp IPAddress, ip IPAddress) []Hostname {
	if strings.HasPrefix(string(sourceIp), UNKNOWN_IP_PREFIX) {
		return nil
	}
	if strings.HasPrefix(string(ip), UNKNOWN_IP_PREFIX) {
		return nil
	}
	klog.V(3).Infof("ReverseLookup: sourceIP %s IP %s", sourceIp, ip)
	networks := net.IpToNetworks[sourceIp]
	if networks == nil {
		return nil
	}
	for _, network := range networks {
		klog.V(3).Infof("Trying %s %v", network.Id, network)
		pod := network.IPToPod[ip]
		if pod != nil && pod.Ready {
			klog.V(3).Infof("Found hostaliases %v", pod.HostAliases)
			return pod.HostAliases
		}
	}
	return nil
}

type Pods struct {
	mutex sync.RWMutex
	// Insertion-order-preserving map from "namespace/name" to Pod.
	// Preserving insertion order gives deterministic conflict reporting
	// when building networks from the pod set.
	Pods *support.LinkedMap[string, *Pod]
}

func NewPods() *Pods {
	return &Pods{
		mutex: sync.RWMutex{},
		Pods:  support.NewLinkedMap[string, *Pod](),
	}
}

func (pods *Pods) AddOrUpdate(pod *Pod) bool {
	pods.mutex.Lock()
	defer pods.mutex.Unlock()

	key := pod.Namespace + "/" + pod.Name
	oldpod, _ := pods.Pods.Get(key)
	if oldpod != nil && pod.Equal(oldpod) {
		klog.V(2).Infof("no change to pod definition %s/%s", pod.Namespace, pod.Name)
		return false
	}
	klog.Infof("%s/%s updated", pod.Namespace, pod.Name)
	pods.Pods.Put(key, pod.Copy())
	return true
}

func (pods *Pods) Get(namespace, name string) *Pod {
	pod, _ := pods.Pods.Get(namespace + "/" + name)
	return pod
}

func (pods *Pods) Delete(namespace, name string) {
	pods.mutex.Lock()
	defer pods.mutex.Unlock()
	pods.Pods.Delete(namespace + "/" + name)
}

type PodErrors struct {
	Errors []*PodError
}

func NewPodErrors(errors []*PodError) *PodErrors {
	if len(errors) == 0 {
		return nil
	}
	return &PodErrors{Errors: errors}
}

func (e *PodErrors) Error() string {
	res := ""
	for _, err := range e.Errors {
		res = res + err.Error() + "\n"
	}
	return res
}

func (e *PodErrors) FirstError(pod *Pod) error {
	for _, err := range e.Errors {
		if err.Pod.Namespace == pod.Namespace && err.Pod.Name == pod.Name {
			return err
		}
	}
	return nil
}

func (pods *Pods) Networks() (*Networks, *PodErrors) {
	pods.mutex.RLock()
	defer pods.mutex.RUnlock()

	networks := NewNetworks()
	errorList := make([]*PodError, 0)
	for _, pod := range pods.Pods.Iter() {
		if err := networks.Add(pod); err != nil {
			errorList = append(errorList, err)
		}
	}
	return networks, NewPodErrors(errorList)
}

func (pods *Pods) Copy() *Pods {
	pods.mutex.RLock()
	defer pods.mutex.RUnlock()
	res := NewPods()
	for key, value := range pods.Pods.Iter() {
		res.Pods.Put(key, value)
	}
	return res
}
