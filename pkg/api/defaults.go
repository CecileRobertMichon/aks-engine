// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT license.

package api

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/go-autorest/autorest/to"

	"github.com/Azure/aks-engine/pkg/api/common"
	"github.com/Azure/aks-engine/pkg/helpers"
	"github.com/blang/semver"
	"github.com/pkg/errors"
)

// DistroValues is a list of currently supported distros
var DistroValues = []Distro{"", Ubuntu, Ubuntu1804, RHEL, CoreOS, AKS, AKS1804, ACC1604}

// SetPropertiesDefaults for the container Properties, returns true if certs are generated
func (cs *ContainerService) SetPropertiesDefaults(isUpgrade, isScale bool) (bool, error) {
	properties := cs.Properties

	// Set custom cloud profile defaults if this cluster configuration has custom cloud profile
	if cs.Properties.CustomCloudProfile != nil {
		err := cs.setCustomCloudProfileDefaults()
		if err != nil {
			return false, err
		}
	}

	cs.setOrchestratorDefaults(isUpgrade || isScale)

	// Set master profile defaults if this cluster configuration includes master node(s)
	if cs.Properties.MasterProfile != nil {
		properties.setMasterProfileDefaults(isUpgrade)
	}
	// Set VMSS Defaults for Masters
	if cs.Properties.MasterProfile != nil && cs.Properties.MasterProfile.IsVirtualMachineScaleSets() {
		properties.setVMSSDefaultsForMasters()
	}

	properties.setAgentProfileDefaults(isUpgrade, isScale)

	properties.setStorageDefaults()
	properties.setExtensionDefaults()
	// Set VMSS Defaults for Agents
	if cs.Properties.HasVMSSAgentPool() {
		properties.setVMSSDefaultsForAgents()
	}

	// Set hosted master profile defaults if this cluster configuration has a hosted control plane
	if cs.Properties.HostedMasterProfile != nil {
		properties.setHostedMasterProfileDefaults()
	}

	if cs.Properties.WindowsProfile != nil {
		properties.setWindowsProfileDefaults(isUpgrade, isScale)
	}

	certsGenerated, _, e := cs.SetDefaultCerts()
	if e != nil {
		return false, e
	}
	return certsGenerated, nil
}

// setOrchestratorDefaults for orchestrators
func (cs *ContainerService) setOrchestratorDefaults(isUpdate bool) {
	a := cs.Properties

	cloudSpecConfig := cs.GetCloudSpecConfig()
	if a.OrchestratorProfile == nil {
		return
	}
	o := a.OrchestratorProfile
	o.OrchestratorVersion = common.GetValidPatchVersion(
		o.OrchestratorType,
		o.OrchestratorVersion, isUpdate, a.HasWindows())

	switch o.OrchestratorType {
	case Kubernetes:
		if o.KubernetesConfig == nil {
			o.KubernetesConfig = &KubernetesConfig{}
		}
		// For backwards compatibility with original, overloaded "NetworkPolicy" config vector
		// we translate deprecated NetworkPolicy usage to the NetworkConfig equivalent
		// and set a default network policy enforcement configuration
		switch o.KubernetesConfig.NetworkPolicy {
		case NetworkPluginAzure:
			if o.KubernetesConfig.NetworkPlugin == "" {
				o.KubernetesConfig.NetworkPlugin = NetworkPluginAzure
				o.KubernetesConfig.NetworkPolicy = DefaultNetworkPolicy
			}
		case NetworkPolicyNone:
			o.KubernetesConfig.NetworkPlugin = NetworkPluginKubenet
			o.KubernetesConfig.NetworkPolicy = DefaultNetworkPolicy
		case NetworkPolicyCalico:
			if o.KubernetesConfig.NetworkPlugin == "" {
				// If not specified, then set the network plugin to be kubenet
				// for backwards compatibility. Otherwise, use what is specified.
				o.KubernetesConfig.NetworkPlugin = NetworkPluginKubenet
			}
		case NetworkPolicyCilium:
			o.KubernetesConfig.NetworkPlugin = NetworkPluginCilium
		}

		if o.KubernetesConfig.KubernetesImageBase == "" {
			o.KubernetesConfig.KubernetesImageBase = cloudSpecConfig.KubernetesSpecConfig.KubernetesImageBase
		}
		if o.KubernetesConfig.EtcdVersion == "" {
			o.KubernetesConfig.EtcdVersion = DefaultEtcdVersion
		}

		if a.HasWindows() {
			if o.KubernetesConfig.NetworkPlugin == "" {
				o.KubernetesConfig.NetworkPlugin = DefaultNetworkPluginWindows
			}
		} else {
			if o.KubernetesConfig.NetworkPlugin == "" {
				o.KubernetesConfig.NetworkPlugin = DefaultNetworkPlugin
			}
		}
		if o.KubernetesConfig.ContainerRuntime == "" {
			o.KubernetesConfig.ContainerRuntime = DefaultContainerRuntime
		}
		switch o.KubernetesConfig.ContainerRuntime {
		case Docker:
			if o.KubernetesConfig.MobyVersion == "" {
				o.KubernetesConfig.MobyVersion = DefaultMobyVersion
			}
		case Containerd, ClearContainers, KataContainers:
			if o.KubernetesConfig.ContainerdVersion == "" {
				o.KubernetesConfig.ContainerdVersion = DefaultContainerdVersion
			}
		}
		if o.KubernetesConfig.ClusterSubnet == "" {
			if o.IsAzureCNI() {
				// When Azure CNI is enabled, all masters, agents and pods share the same large subnet.
				// Except when master is VMSS, then masters and agents have separate subnets within the same large subnet.
				o.KubernetesConfig.ClusterSubnet = DefaultKubernetesSubnet
			} else {
				o.KubernetesConfig.ClusterSubnet = DefaultKubernetesClusterSubnet
			}
		}
		if o.KubernetesConfig.GCHighThreshold == 0 {
			o.KubernetesConfig.GCHighThreshold = DefaultKubernetesGCHighThreshold
		}
		if o.KubernetesConfig.GCLowThreshold == 0 {
			o.KubernetesConfig.GCLowThreshold = DefaultKubernetesGCLowThreshold
		}
		if o.KubernetesConfig.DNSServiceIP == "" {
			o.KubernetesConfig.DNSServiceIP = DefaultKubernetesDNSServiceIP
		}
		if o.KubernetesConfig.DockerBridgeSubnet == "" {
			o.KubernetesConfig.DockerBridgeSubnet = DefaultDockerBridgeSubnet
		}
		if o.KubernetesConfig.ServiceCIDR == "" {
			o.KubernetesConfig.ServiceCIDR = DefaultKubernetesServiceCIDR
		}

		if o.KubernetesConfig.CloudProviderBackoff == nil {
			o.KubernetesConfig.CloudProviderBackoff = to.BoolPtr(DefaultKubernetesCloudProviderBackoff)
		}
		// Enforce sane cloudprovider backoff defaults.
		o.KubernetesConfig.SetCloudProviderBackoffDefaults()

		if o.KubernetesConfig.CloudProviderRateLimit == nil {
			o.KubernetesConfig.CloudProviderRateLimit = to.BoolPtr(DefaultKubernetesCloudProviderRateLimit)
		}
		// Enforce sane cloudprovider rate limit defaults.
		o.KubernetesConfig.SetCloudProviderRateLimitDefaults()

		if o.KubernetesConfig.PrivateCluster == nil {
			o.KubernetesConfig.PrivateCluster = &PrivateCluster{}
		}

		if o.KubernetesConfig.PrivateCluster.Enabled == nil {
			o.KubernetesConfig.PrivateCluster.Enabled = to.BoolPtr(DefaultPrivateClusterEnabled)
		}

		if "" == a.OrchestratorProfile.KubernetesConfig.EtcdDiskSizeGB {
			switch {
			case a.TotalNodes() > 20:
				a.OrchestratorProfile.KubernetesConfig.EtcdDiskSizeGB = DefaultEtcdDiskSizeGT20Nodes
			case a.TotalNodes() > 10:
				a.OrchestratorProfile.KubernetesConfig.EtcdDiskSizeGB = DefaultEtcdDiskSizeGT10Nodes
			case a.TotalNodes() > 3:
				a.OrchestratorProfile.KubernetesConfig.EtcdDiskSizeGB = DefaultEtcdDiskSizeGT3Nodes
			default:
				a.OrchestratorProfile.KubernetesConfig.EtcdDiskSizeGB = DefaultEtcdDiskSize
			}
		}

		if to.Bool(o.KubernetesConfig.EnableDataEncryptionAtRest) {
			if "" == a.OrchestratorProfile.KubernetesConfig.EtcdEncryptionKey {
				a.OrchestratorProfile.KubernetesConfig.EtcdEncryptionKey = generateEtcdEncryptionKey()
			}
		}

		if a.OrchestratorProfile.KubernetesConfig.PrivateJumpboxProvision() && a.OrchestratorProfile.KubernetesConfig.PrivateCluster.JumpboxProfile.OSDiskSizeGB == 0 {
			a.OrchestratorProfile.KubernetesConfig.PrivateCluster.JumpboxProfile.OSDiskSizeGB = DefaultJumpboxDiskSize
		}

		if a.OrchestratorProfile.KubernetesConfig.PrivateJumpboxProvision() && a.OrchestratorProfile.KubernetesConfig.PrivateCluster.JumpboxProfile.Username == "" {
			a.OrchestratorProfile.KubernetesConfig.PrivateCluster.JumpboxProfile.Username = DefaultJumpboxUsername
		}

		if a.OrchestratorProfile.KubernetesConfig.PrivateJumpboxProvision() && a.OrchestratorProfile.KubernetesConfig.PrivateCluster.JumpboxProfile.StorageProfile == "" {
			a.OrchestratorProfile.KubernetesConfig.PrivateCluster.JumpboxProfile.StorageProfile = ManagedDisks
		}

		if a.OrchestratorProfile.KubernetesConfig.EnableRbac == nil {
			a.OrchestratorProfile.KubernetesConfig.EnableRbac = to.BoolPtr(DefaultRBACEnabled)
		}

		if a.OrchestratorProfile.KubernetesConfig.IsRBACEnabled() {
			if common.IsKubernetesVersionGe(o.OrchestratorVersion, "1.9.0") {
				// TODO make EnableAggregatedAPIs a pointer to bool so that a user can opt out of it
				a.OrchestratorProfile.KubernetesConfig.EnableAggregatedAPIs = true
			}
		} else if isUpdate && a.OrchestratorProfile.KubernetesConfig.EnableAggregatedAPIs {
			// Upgrade scenario:
			// We need to force set EnableAggregatedAPIs to false if RBAC was previously disabled
			a.OrchestratorProfile.KubernetesConfig.EnableAggregatedAPIs = false
		}

		if a.OrchestratorProfile.KubernetesConfig.EnableSecureKubelet == nil {
			a.OrchestratorProfile.KubernetesConfig.EnableSecureKubelet = to.BoolPtr(DefaultSecureKubeletEnabled)
		}

		if a.OrchestratorProfile.KubernetesConfig.UseInstanceMetadata == nil {
			a.OrchestratorProfile.KubernetesConfig.UseInstanceMetadata = to.BoolPtr(DefaultUseInstanceMetadata)
		}

		if !a.HasAvailabilityZones() && a.OrchestratorProfile.KubernetesConfig.LoadBalancerSku == "" {
			a.OrchestratorProfile.KubernetesConfig.LoadBalancerSku = DefaultLoadBalancerSku
		}

		if common.IsKubernetesVersionGe(a.OrchestratorProfile.OrchestratorVersion, "1.11.0") && a.OrchestratorProfile.KubernetesConfig.LoadBalancerSku == StandardLoadBalancerSku && a.OrchestratorProfile.KubernetesConfig.ExcludeMasterFromStandardLB == nil {
			a.OrchestratorProfile.KubernetesConfig.ExcludeMasterFromStandardLB = to.BoolPtr(DefaultExcludeMasterFromStandardLB)
		}

		if a.OrchestratorProfile.IsAzureCNI() {
			if a.HasWindows() {
				a.OrchestratorProfile.KubernetesConfig.AzureCNIVersion = AzureCniPluginVerWindows
			} else {
				a.OrchestratorProfile.KubernetesConfig.AzureCNIVersion = AzureCniPluginVerLinux
			}
		}

		if a.OrchestratorProfile.KubernetesConfig.MaximumLoadBalancerRuleCount == 0 {
			a.OrchestratorProfile.KubernetesConfig.MaximumLoadBalancerRuleCount = DefaultMaximumLoadBalancerRuleCount
		}
		if a.OrchestratorProfile.KubernetesConfig.ProxyMode == "" {
			a.OrchestratorProfile.KubernetesConfig.ProxyMode = DefaultKubeProxyMode
		}

		// Configure addons
		cs.setAddonsConfig(isUpdate)
		// Configure kubelet
		cs.setKubeletConfig()
		// Configure controller-manager
		cs.setControllerManagerConfig()
		// Configure cloud-controller-manager
		cs.setCloudControllerManagerConfig()
		// Configure apiserver
		cs.setAPIServerConfig()
		// Configure scheduler
		cs.setSchedulerConfig()

	case DCOS:
		if o.DcosConfig == nil {
			o.DcosConfig = &DcosConfig{}
		}
		dcosSemVer, _ := semver.Make(o.OrchestratorVersion)
		dcosBootstrapSemVer, _ := semver.Make(common.DCOSVersion1Dot11Dot0)
		if !dcosSemVer.LT(dcosBootstrapSemVer) {
			if o.DcosConfig.BootstrapProfile == nil {
				o.DcosConfig.BootstrapProfile = &BootstrapProfile{}
			}
			if len(o.DcosConfig.BootstrapProfile.VMSize) == 0 {
				o.DcosConfig.BootstrapProfile.VMSize = "Standard_D2s_v3"
			}
		}
	}
}

func (p *Properties) setExtensionDefaults() {
	if p.ExtensionProfiles == nil {
		return
	}
	for _, extension := range p.ExtensionProfiles {
		if extension.RootURL == "" {
			extension.RootURL = DefaultExtensionsRootURL
		}
	}
}

func (p *Properties) setMasterProfileDefaults(isUpgrade bool) {
	if p.MasterProfile.Distro == "" {
		if p.OrchestratorProfile.IsKubernetes() {
			p.MasterProfile.Distro = AKS1804
		} else {
			p.MasterProfile.Distro = Ubuntu
		}
	}

	// "--protect-kernel-defaults" is only true for VHD based VMs since the base Ubuntu distros don't have a /etc/sysctl.d/60-CIS.conf file.
	if p.MasterProfile.IsVHDDistro() {
		if p.MasterProfile.KubernetesConfig == nil {
			p.MasterProfile.KubernetesConfig = &KubernetesConfig{}
		}
		if p.MasterProfile.KubernetesConfig.KubeletConfig == nil {
			p.MasterProfile.KubernetesConfig.KubeletConfig = map[string]string{}
		}
		if _, ok := p.MasterProfile.KubernetesConfig.KubeletConfig["--protect-kernel-defaults"]; !ok {
			p.MasterProfile.KubernetesConfig.KubeletConfig["--protect-kernel-defaults"] = "true"
		}
	}

	// set default to VMAS for now
	if len(p.MasterProfile.AvailabilityProfile) == 0 {
		p.MasterProfile.AvailabilityProfile = AvailabilitySet
	}

	if !p.MasterProfile.IsCustomVNET() {
		if p.OrchestratorProfile.OrchestratorType == Kubernetes {
			if p.OrchestratorProfile.IsAzureCNI() {
				// When VNET integration is enabled, all masters, agents and pods share the same large subnet.
				p.MasterProfile.Subnet = p.OrchestratorProfile.KubernetesConfig.ClusterSubnet
				// FirstConsecutiveStaticIP is not reset if it is upgrade and some value already exists
				if !isUpgrade || len(p.MasterProfile.FirstConsecutiveStaticIP) == 0 {
					if p.MasterProfile.IsVirtualMachineScaleSets() {
						p.MasterProfile.FirstConsecutiveStaticIP = DefaultFirstConsecutiveKubernetesStaticIPVMSS
						p.MasterProfile.Subnet = DefaultKubernetesMasterSubnet
						p.MasterProfile.AgentSubnet = DefaultKubernetesAgentSubnetVMSS
					} else {
						p.MasterProfile.FirstConsecutiveStaticIP = p.MasterProfile.GetFirstConsecutiveStaticIPAddress(p.MasterProfile.Subnet)
					}
				}
			} else {
				p.MasterProfile.Subnet = DefaultKubernetesMasterSubnet
				// FirstConsecutiveStaticIP is not reset if it is upgrade and some value already exists
				if !isUpgrade || len(p.MasterProfile.FirstConsecutiveStaticIP) == 0 {
					if p.MasterProfile.IsVirtualMachineScaleSets() {
						p.MasterProfile.FirstConsecutiveStaticIP = DefaultFirstConsecutiveKubernetesStaticIPVMSS
						p.MasterProfile.AgentSubnet = DefaultKubernetesAgentSubnetVMSS
					} else {
						p.MasterProfile.FirstConsecutiveStaticIP = DefaultFirstConsecutiveKubernetesStaticIP
					}
				}
			}
		} else if p.OrchestratorProfile.OrchestratorType == DCOS {
			p.MasterProfile.Subnet = DefaultDCOSMasterSubnet
			// FirstConsecutiveStaticIP is not reset if it is upgrade and some value already exists
			if !isUpgrade || len(p.MasterProfile.FirstConsecutiveStaticIP) == 0 {
				p.MasterProfile.FirstConsecutiveStaticIP = DefaultDCOSFirstConsecutiveStaticIP
			}
			if p.OrchestratorProfile.DcosConfig != nil && p.OrchestratorProfile.DcosConfig.BootstrapProfile != nil {
				if !isUpgrade || len(p.OrchestratorProfile.DcosConfig.BootstrapProfile.StaticIP) == 0 {
					p.OrchestratorProfile.DcosConfig.BootstrapProfile.StaticIP = DefaultDCOSBootstrapStaticIP
				}
			}
		} else if p.HasWindows() {
			p.MasterProfile.Subnet = DefaultSwarmWindowsMasterSubnet
			// FirstConsecutiveStaticIP is not reset if it is upgrade and some value already exists
			if !isUpgrade || len(p.MasterProfile.FirstConsecutiveStaticIP) == 0 {
				p.MasterProfile.FirstConsecutiveStaticIP = DefaultSwarmWindowsFirstConsecutiveStaticIP
			}
		} else {
			p.MasterProfile.Subnet = DefaultMasterSubnet
			// FirstConsecutiveStaticIP is not reset if it is upgrade and some value already exists
			if !isUpgrade || len(p.MasterProfile.FirstConsecutiveStaticIP) == 0 {
				p.MasterProfile.FirstConsecutiveStaticIP = DefaultFirstConsecutiveStaticIP
			}
		}
	}

	if p.MasterProfile.IsCustomVNET() && p.MasterProfile.IsVirtualMachineScaleSets() {
		if p.OrchestratorProfile.OrchestratorType == Kubernetes {
			p.MasterProfile.FirstConsecutiveStaticIP = p.MasterProfile.GetFirstConsecutiveStaticIPAddress(p.MasterProfile.VnetCidr)
		}
	}
	// Set the default number of IP addresses allocated for masters.
	if p.MasterProfile.IPAddressCount == 0 {
		// Allocate one IP address for the node.
		p.MasterProfile.IPAddressCount = 1

		// Allocate IP addresses for pods if VNET integration is enabled.
		if p.OrchestratorProfile.IsAzureCNI() {
			if p.OrchestratorProfile.OrchestratorType == Kubernetes {
				masterMaxPods, _ := strconv.Atoi(p.MasterProfile.KubernetesConfig.KubeletConfig["--max-pods"])
				p.MasterProfile.IPAddressCount += masterMaxPods
			}
		}
	}

	if p.MasterProfile.HTTPSourceAddressPrefix == "" {
		p.MasterProfile.HTTPSourceAddressPrefix = "*"
	}

	if nil == p.MasterProfile.CosmosEtcd {
		p.MasterProfile.CosmosEtcd = to.BoolPtr(DefaultUseCosmos)
	}
}

// setVMSSDefaultsForMasters
func (p *Properties) setVMSSDefaultsForMasters() {
	if p.MasterProfile.SinglePlacementGroup == nil {
		p.MasterProfile.SinglePlacementGroup = to.BoolPtr(DefaultSinglePlacementGroup)
	}
	if p.MasterProfile.HasAvailabilityZones() && (p.OrchestratorProfile.KubernetesConfig != nil && p.OrchestratorProfile.KubernetesConfig.LoadBalancerSku == "") {
		p.OrchestratorProfile.KubernetesConfig.LoadBalancerSku = StandardLoadBalancerSku
		p.OrchestratorProfile.KubernetesConfig.ExcludeMasterFromStandardLB = to.BoolPtr(DefaultExcludeMasterFromStandardLB)
	}
}

// setVMSSDefaultsForAgents
func (p *Properties) setVMSSDefaultsForAgents() {
	for _, profile := range p.AgentPoolProfiles {
		if profile.AvailabilityProfile == VirtualMachineScaleSets {
			if profile.Count > 100 {
				profile.SinglePlacementGroup = to.BoolPtr(false)
			}
			if profile.SinglePlacementGroup == nil {
				profile.SinglePlacementGroup = to.BoolPtr(DefaultSinglePlacementGroup)
			}
			if profile.HasAvailabilityZones() && (p.OrchestratorProfile.KubernetesConfig != nil && p.OrchestratorProfile.KubernetesConfig.LoadBalancerSku == "") {
				p.OrchestratorProfile.KubernetesConfig.LoadBalancerSku = StandardLoadBalancerSku
				p.OrchestratorProfile.KubernetesConfig.ExcludeMasterFromStandardLB = to.BoolPtr(DefaultExcludeMasterFromStandardLB)
			}
		}

	}
}

func (p *Properties) setAgentProfileDefaults(isUpgrade, isScale bool) {
	// configure the subnets if not in custom VNET
	if p.MasterProfile != nil && !p.MasterProfile.IsCustomVNET() {
		subnetCounter := 0
		for _, profile := range p.AgentPoolProfiles {
			if p.OrchestratorProfile.OrchestratorType == Kubernetes {
				if !p.MasterProfile.IsVirtualMachineScaleSets() {
					profile.Subnet = p.MasterProfile.Subnet
				}
			} else {
				profile.Subnet = fmt.Sprintf(DefaultAgentSubnetTemplate, subnetCounter)
			}

			subnetCounter++
		}
	}

	for _, profile := range p.AgentPoolProfiles {
		// set default OSType to Linux
		if profile.OSType == "" {
			profile.OSType = Linux
		}

		// Accelerated Networking is supported on most general purpose and compute-optimized instance sizes with 2 or more vCPUs.
		// These supported series are: D/DSv2 and F/Fs // All the others are not supported
		// On instances that support hyperthreading, Accelerated Networking is supported on VM instances with 4 or more vCPUs.
		// Supported series are: D/DSv3, E/ESv3, Fsv2, and Ms/Mms.
		if profile.AcceleratedNetworkingEnabled == nil {
			profile.AcceleratedNetworkingEnabled = to.BoolPtr(DefaultAcceleratedNetworking && !isUpgrade && !isScale && helpers.AcceleratedNetworkingSupported(profile.VMSize))
		}

		if profile.AcceleratedNetworkingEnabledWindows == nil {
			profile.AcceleratedNetworkingEnabledWindows = to.BoolPtr(DefaultAcceleratedNetworkingWindowsEnabled && !isUpgrade && !isScale && helpers.AcceleratedNetworkingSupported(profile.VMSize))
		}

		if profile.VMSSOverProvisioningEnabled == nil {
			profile.VMSSOverProvisioningEnabled = to.BoolPtr(DefaultVMSSOverProvisioningEnabled && !isUpgrade && !isScale)
		}

		if profile.AuditDEnabled == nil {
			profile.AuditDEnabled = to.BoolPtr(DefaultAuditDEnabled && !isUpgrade && !isScale)
		}

		if profile.OSType != Windows {
			if profile.Distro == "" {
				if p.OrchestratorProfile.IsKubernetes() {
					if profile.OSDiskSizeGB != 0 && profile.OSDiskSizeGB < VHDDiskSizeAKS {
						if p.OrchestratorProfile.IsAzureCNI() {
							// Workaround for https://github.com/Azure/aks-engine/issues/761.
							profile.Distro = Ubuntu
						} else {
							profile.Distro = Ubuntu1804
						}
					} else {
						profile.Distro = AKS1804
					}
				} else {
					profile.Distro = Ubuntu
				}
				// Ensure distro is set properly for N Series SKUs, because
				// Previous versions of aks-engine required the docker-engine distro for N series vms,
				// so we need to hard override it in order to produce a working cluster in upgrade/scale contexts
			} else if p.OrchestratorProfile.IsKubernetes() && (isUpgrade || isScale) {
				if profile.Distro == AKSDockerEngine {
					profile.Distro = AKS1804
				}
			}
		}

		// "--protect-kernel-defaults" is only true for VHD based VMs since the base Ubuntu distros don't have a /etc/sysctl.d/60-CIS.conf file.
		if profile.IsVHDDistro() {
			if profile.KubernetesConfig == nil {
				profile.KubernetesConfig = &KubernetesConfig{}
			}
			if profile.KubernetesConfig.KubeletConfig == nil {
				profile.KubernetesConfig.KubeletConfig = map[string]string{}
			}
			if _, ok := profile.KubernetesConfig.KubeletConfig["--protect-kernel-defaults"]; !ok {
				profile.KubernetesConfig.KubeletConfig["--protect-kernel-defaults"] = "true"
			}
		}

		// Set the default number of IP addresses allocated for agents.
		if profile.IPAddressCount == 0 {
			// Allocate one IP address for the node.
			profile.IPAddressCount = 1

			// Allocate IP addresses for pods if VNET integration is enabled.
			if p.OrchestratorProfile.IsAzureCNI() {
				agentPoolMaxPods, _ := strconv.Atoi(profile.KubernetesConfig.KubeletConfig["--max-pods"])
				profile.IPAddressCount += agentPoolMaxPods
			}
		}

		if profile.PreserveNodesProperties == nil {
			profile.PreserveNodesProperties = to.BoolPtr(DefaultPreserveNodesProperties)
		}

		if profile.EnableVMSSNodePublicIP == nil {
			profile.EnableVMSSNodePublicIP = to.BoolPtr(DefaultEnableVMSSNodePublicIP)
		}
	}
}

// setWindowsProfileDefaults sets default WindowsProfile values
func (p *Properties) setWindowsProfileDefaults(isUpgrade, isScale bool) {
	windowsProfile := p.WindowsProfile
	if !isUpgrade && !isScale {
		if windowsProfile.WindowsPublisher == "" {
			windowsProfile.WindowsPublisher = DefaultWindowsPublisher
		}
		if windowsProfile.WindowsOffer == "" {
			windowsProfile.WindowsOffer = DefaultWindowsOffer
		}
		if windowsProfile.WindowsSku == "" {
			windowsProfile.WindowsSku = DefaultWindowsSku
		}
		if windowsProfile.ImageVersion == "" {
			windowsProfile.ImageVersion = DefaultImageVersion
		}
	}
}

// setStorageDefaults for agents
func (p *Properties) setStorageDefaults() {
	if p.MasterProfile != nil && len(p.MasterProfile.StorageProfile) == 0 {
		if p.OrchestratorProfile.OrchestratorType == Kubernetes {
			p.MasterProfile.StorageProfile = ManagedDisks
		} else {
			p.MasterProfile.StorageProfile = StorageAccount
		}
	}
	for _, profile := range p.AgentPoolProfiles {
		if len(profile.StorageProfile) == 0 {
			if p.OrchestratorProfile.OrchestratorType == Kubernetes {
				profile.StorageProfile = ManagedDisks
			} else {
				profile.StorageProfile = StorageAccount
			}
		}
		if len(profile.AvailabilityProfile) == 0 {
			profile.AvailabilityProfile = VirtualMachineScaleSets
			// VMSS is not supported for k8s below 1.10.2
			if p.OrchestratorProfile.OrchestratorType == Kubernetes && !common.IsKubernetesVersionGe(p.OrchestratorProfile.OrchestratorVersion, "1.10.2") {
				profile.AvailabilityProfile = AvailabilitySet
			}
		}
		if len(profile.ScaleSetEvictionPolicy) == 0 && profile.ScaleSetPriority == ScaleSetPriorityLow {
			profile.ScaleSetEvictionPolicy = ScaleSetEvictionPolicyDelete
		}
	}
}

func (p *Properties) setHostedMasterProfileDefaults() {
	p.HostedMasterProfile.Subnet = DefaultKubernetesMasterSubnet
}

// SetDefaultCerts generates and sets defaults for the container certificateProfile, returns true if certs are generated
func (cs *ContainerService) SetDefaultCerts() (bool, []net.IP, error) {
	p := cs.Properties
	if p.MasterProfile == nil || p.OrchestratorProfile.OrchestratorType != Kubernetes {
		return false, nil, nil
	}

	provided := certsAlreadyPresent(p.CertificateProfile, p.MasterProfile.Count)

	if areAllTrue(provided) {
		return false, nil, nil
	}

	var azureProdFQDNs []string
	for _, location := range cs.GetLocations() {
		azureProdFQDNs = append(azureProdFQDNs, FormatProdFQDNByLocation(p.MasterProfile.DNSPrefix, location, p.GetCustomCloudName()))
	}

	masterExtraFQDNs := append(azureProdFQDNs, p.MasterProfile.SubjectAltNames...)
	masterExtraFQDNs = append(masterExtraFQDNs, "localhost")
	firstMasterIP := net.ParseIP(p.MasterProfile.FirstConsecutiveStaticIP).To4()
	localhostIP := net.ParseIP("127.0.0.1").To4()

	if firstMasterIP == nil {
		return false, nil, errors.Errorf("MasterProfile.FirstConsecutiveStaticIP '%s' is an invalid IP address", p.MasterProfile.FirstConsecutiveStaticIP)
	}

	ips := []net.IP{firstMasterIP, localhostIP}

	// Include the Internal load balancer as well
	if p.MasterProfile.IsVirtualMachineScaleSets() {
		ips = append(ips, net.IP{firstMasterIP[0], firstMasterIP[1], byte(255), byte(DefaultInternalLbStaticIPOffset)})
	} else {
		// Add the Internal Loadbalancer IP which is always at p known offset from the firstMasterIP
		ips = append(ips, net.IP{firstMasterIP[0], firstMasterIP[1], firstMasterIP[2], firstMasterIP[3] + byte(DefaultInternalLbStaticIPOffset)})
	}

	var offsetMultiplier int
	if p.MasterProfile.IsVirtualMachineScaleSets() {
		offsetMultiplier = p.MasterProfile.IPAddressCount
	} else {
		offsetMultiplier = 1
	}
	addr := binary.BigEndian.Uint32(firstMasterIP)
	for i := 1; i < p.MasterProfile.Count; i++ {
		newAddr := getNewAddr(addr, i, offsetMultiplier)
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, newAddr)
		ips = append(ips, ip)
	}
	if p.CertificateProfile == nil {
		p.CertificateProfile = &CertificateProfile{}
	}

	// use the specified Certificate Authority pair, or generate p new pair
	var caPair *helpers.PkiKeyCertPair
	if provided["ca"] {
		caPair = &helpers.PkiKeyCertPair{CertificatePem: p.CertificateProfile.CaCertificate, PrivateKeyPem: p.CertificateProfile.CaPrivateKey}
	} else {
		var err error
		caPair, err = helpers.CreatePkiKeyCertPair("ca")
		if err != nil {
			return false, ips, err
		}
		p.CertificateProfile.CaCertificate = caPair.CertificatePem
		p.CertificateProfile.CaPrivateKey = caPair.PrivateKeyPem
	}

	cidrFirstIP, err := common.CidrStringFirstIP(p.OrchestratorProfile.KubernetesConfig.ServiceCIDR)
	if err != nil {
		return false, ips, err
	}
	ips = append(ips, cidrFirstIP)

	apiServerPair, clientPair, kubeConfigPair, etcdServerPair, etcdClientPair, etcdPeerPairs, err := helpers.CreatePki(masterExtraFQDNs, ips, DefaultKubernetesClusterDomain, caPair, p.MasterProfile.Count)
	if err != nil {
		return false, ips, err
	}

	// If no Certificate Authority pair or no cert/key pair was provided, use generated cert/key pairs signed by provided Certificate Authority pair
	if !provided["apiserver"] || !provided["ca"] {
		p.CertificateProfile.APIServerCertificate = apiServerPair.CertificatePem
		p.CertificateProfile.APIServerPrivateKey = apiServerPair.PrivateKeyPem
	}
	if !provided["client"] || !provided["ca"] {
		p.CertificateProfile.ClientCertificate = clientPair.CertificatePem
		p.CertificateProfile.ClientPrivateKey = clientPair.PrivateKeyPem
	}
	if !provided["kubeconfig"] || !provided["ca"] {
		p.CertificateProfile.KubeConfigCertificate = kubeConfigPair.CertificatePem
		p.CertificateProfile.KubeConfigPrivateKey = kubeConfigPair.PrivateKeyPem
	}
	if !provided["etcd"] || !provided["ca"] {
		p.CertificateProfile.EtcdServerCertificate = etcdServerPair.CertificatePem
		p.CertificateProfile.EtcdServerPrivateKey = etcdServerPair.PrivateKeyPem
		p.CertificateProfile.EtcdClientCertificate = etcdClientPair.CertificatePem
		p.CertificateProfile.EtcdClientPrivateKey = etcdClientPair.PrivateKeyPem
		p.CertificateProfile.EtcdPeerCertificates = make([]string, p.MasterProfile.Count)
		p.CertificateProfile.EtcdPeerPrivateKeys = make([]string, p.MasterProfile.Count)
		for i, v := range etcdPeerPairs {
			p.CertificateProfile.EtcdPeerCertificates[i] = v.CertificatePem
			p.CertificateProfile.EtcdPeerPrivateKeys[i] = v.PrivateKeyPem
		}
	}

	return true, ips, nil
}

func areAllTrue(m map[string]bool) bool {
	for _, v := range m {
		if !v {
			return false
		}
	}
	return true
}

// getNewIP returns a new IP derived from an address plus a multiple of an offset
func getNewAddr(addr uint32, count int, offsetMultiplier int) uint32 {
	offset := count * offsetMultiplier
	newAddr := addr + uint32(offset)
	return newAddr
}

// certsAlreadyPresent already present returns a map where each key is a type of cert and each value is true if that cert/key pair is user-provided
func certsAlreadyPresent(c *CertificateProfile, m int) map[string]bool {
	g := map[string]bool{
		"ca":         false,
		"apiserver":  false,
		"kubeconfig": false,
		"client":     false,
		"etcd":       false,
	}
	if c != nil {
		etcdPeer := true
		if len(c.EtcdPeerCertificates) != m || len(c.EtcdPeerPrivateKeys) != m {
			etcdPeer = false
		} else {
			for i, p := range c.EtcdPeerCertificates {
				if !(len(p) > 0) || !(len(c.EtcdPeerPrivateKeys[i]) > 0) {
					etcdPeer = false
				}
			}
		}
		g["ca"] = len(c.CaCertificate) > 0 && len(c.CaPrivateKey) > 0
		g["apiserver"] = len(c.APIServerCertificate) > 0 && len(c.APIServerPrivateKey) > 0
		g["kubeconfig"] = len(c.KubeConfigCertificate) > 0 && len(c.KubeConfigPrivateKey) > 0
		g["client"] = len(c.ClientCertificate) > 0 && len(c.ClientPrivateKey) > 0
		g["etcd"] = etcdPeer && len(c.EtcdClientCertificate) > 0 && len(c.EtcdClientPrivateKey) > 0 && len(c.EtcdServerCertificate) > 0 && len(c.EtcdServerPrivateKey) > 0
	}
	return g
}

// combine user-provided --feature-gates vals with defaults
// a minimum k8s version may be declared as required for defaults assignment
func addDefaultFeatureGates(m map[string]string, version string, minVersion string, defaults string) {
	if minVersion != "" {
		if common.IsKubernetesVersionGe(version, minVersion) {
			m["--feature-gates"] = combineValues(m["--feature-gates"], defaults)
		} else {
			m["--feature-gates"] = combineValues(m["--feature-gates"], "")
		}
	} else {
		m["--feature-gates"] = combineValues(m["--feature-gates"], defaults)
	}
}

func combineValues(inputs ...string) string {
	valueMap := make(map[string]string)
	for _, input := range inputs {
		applyValueStringToMap(valueMap, input)
	}
	return mapToString(valueMap)
}

func applyValueStringToMap(valueMap map[string]string, input string) {
	values := strings.Split(input, ",")
	for index := 0; index < len(values); index++ {
		// trim spaces (e.g. if the input was "foo=true, bar=true" - we want to drop the space after the comma)
		value := strings.Trim(values[index], " ")
		valueParts := strings.Split(value, "=")
		if len(valueParts) == 2 {
			valueMap[valueParts[0]] = valueParts[1]
		}
	}
}

func mapToString(valueMap map[string]string) string {
	// Order by key for consistency
	keys := []string{}
	for key := range valueMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	for _, key := range keys {
		buf.WriteString(fmt.Sprintf("%s=%s,", key, valueMap[key]))
	}
	return strings.TrimSuffix(buf.String(), ",")
}

func generateEtcdEncryptionKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}
