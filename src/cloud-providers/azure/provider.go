// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v4"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v2"
	provider "github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers/util"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers/util/cloudinit"
	"golang.org/x/crypto/ssh"
)

var logger = log.New(log.Writer(), "[adaptor/cloud/azure] ", log.LstdFlags|log.Lmsgprefix)
var errNotReady = errors.New("address not ready")
var errNotFound = errors.New("VM name not found")

const (
	maxInstanceNameLen = 63
)

type azureProvider struct {
	azureClient   azcore.TokenCredential
	serviceConfig *Config
}

func NewProvider(config *Config) (provider.Provider, error) {

	logger.Printf("azure config %+v", config.Redact())

	// Clean the config.SSHKeyPath to avoid bad paths
	if config.SSHKeyPath != "" {
		config.SSHKeyPath = filepath.Clean(config.SSHKeyPath)
	}

	azureClient, err := NewAzureClient(*config)
	if err != nil {
		logger.Printf("creating azure client: %v", err)
		return nil, err
	}

	provider := &azureProvider{
		azureClient:   azureClient,
		serviceConfig: config,
	}

	if err = provider.updateInstanceSizeSpecList(); err != nil {
		return nil, err
	}

	return provider, nil
}

func parseIP(addr string) (*netip.Addr, error) {
	if addr == "" || addr == "0.0.0.0" {
		return nil, errNotReady
	}

	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return nil, fmt.Errorf("parse pod vm IP %q: %w", addr, err)
	}
	return &ip, nil
}

// generateSSHPublicKey generates a new RSA SSH key pair,
// but doesn't save anything in the filesystem
func generateSSHPublicKey() ([]byte, error) {
	logger.Printf("Generating a new in-memory SSH public key")

	// Generate RSA private key
	bitSize := 4096
	privateKey, err := rsa.GenerateKey(rand.Reader, bitSize)
	if err != nil {
		return nil, fmt.Errorf("failed to generate RSA private key: %w", err)
	}

	// Validate the private key
	err = privateKey.Validate()
	if err != nil {
		return nil, fmt.Errorf("failed to validate private key: %w", err)
	}

	// Generate public key
	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to generate public key: %w", err)
	}

	// Marshal public key in authorized_keys format
	publicKeyBytes := ssh.MarshalAuthorizedKey(publicKey)

	logger.Printf("Successfully generated a new in-memory SSH public key")
	return publicKeyBytes, nil
}

// getIPs retrieves IP addresses from the VM's network interfaces.
//
// In multi-NIC mode (spec.MultiNic == true), the primary NIC's IPs are always
// returned first so the worker node connects to the correct control-plane
// interface regardless of the order Azure returns NICs in the response.
//
// In single-NIC mode behaviour is unchanged from before.
//
// AWS equivalent: AWS enforces ordering via DeviceIndex (0 = primary, 1 = secondary).
// Azure does not guarantee NIC return order, so we sort by nic.Properties.Primary.
func (p *azureProvider) getIPs(ctx context.Context, vm *armcompute.VirtualMachine) ([]netip.Addr, error) {
	nicClient, err := armnetwork.NewInterfacesClient(p.serviceConfig.SubscriptionID, p.azureClient, nil)
	if err != nil {
		return nil, fmt.Errorf("create network interfaces client: %w", err)
	}
	rgName := p.serviceConfig.ResourceGroupName
	nicRefs := vm.Properties.NetworkProfile.NetworkInterfaces

	// Separate primary and secondary NIC IP configurations so we can return
	// them in a deterministic order regardless of what Azure gives us.
	// In single-NIC mode secondaryIPCs will remain empty.
	var primaryIPCs []*armnetwork.InterfaceIPConfiguration
	var secondaryIPCs []*armnetwork.InterfaceIPConfiguration

	for _, nicRef := range nicRefs {
		nicID := *nicRef.ID
		// the last segment of a nic id is the name
		nicName := nicID[strings.LastIndex(nicID, "/")+1:]
		nic, err := nicClient.Get(ctx, rgName, nicName, nil)
		if err != nil {
			return nil, fmt.Errorf("get network interface: %w", err)
		}

		isPrimary := nic.Properties.Primary != nil && *nic.Properties.Primary
		if isPrimary {
			primaryIPCs = append(primaryIPCs, nic.Properties.IPConfigurations...)
		} else {
			secondaryIPCs = append(secondaryIPCs, nic.Properties.IPConfigurations...)
		}
	}

	// Build ordered list: primary NIC first, secondary NIC appended after.
	// - Single-NIC mode : only primaryIPCs is populated, identical to original behaviour.
	// - Multi-NIC mode  : primary (control-plane) NIC first so the worker node
	//                     always connects to the tunnel interface; secondary
	//                     (external) NIC appended for routing purposes.
	var orderedIPCs []*armnetwork.InterfaceIPConfiguration
	orderedIPCs = append(orderedIPCs, primaryIPCs...)
	orderedIPCs = append(orderedIPCs, secondaryIPCs...)

	var ips []netip.Addr

	// ALWAYS add primary NIC's private IP first for control plane connectivity
	if len(primaryIPCs) > 0 && primaryIPCs[0].Properties.PrivateIPAddress != nil {
		ip, err := parseIP(*primaryIPCs[0].Properties.PrivateIPAddress)
		if err != nil {
			return nil, err
		}
		ips = append(ips, *ip)
		logger.Printf("pod vm IP[%d][private]=%s (primary NIC)", len(ips)-1, ip.String())
	}

	// Then add public IPs (if enabled)
	if p.serviceConfig.UsePublicIP {
		publicIPClient, err := armnetwork.NewPublicIPAddressesClient(p.serviceConfig.SubscriptionID, p.azureClient, nil)
		if err != nil {
			return nil, fmt.Errorf("create public ip client: %w", err)
		}
		for _, ipc := range orderedIPCs {
			if ipc.Properties.PublicIPAddress == nil {
				continue
			}
			ipID := *ipc.Properties.PublicIPAddress.ID
			// the last segment of a ip id is the name
			ipName := ipID[strings.LastIndex(ipID, "/")+1:]
			publicIP, err := publicIPClient.Get(ctx, rgName, ipName, nil)
			if err != nil {
				return nil, fmt.Errorf("get public ip: %w", err)
			}
			addr := publicIP.Properties.IPAddress
			if addr != nil {
				ip, err := parseIP(*addr)
				if err != nil {
					return nil, err
				}
				ips = append(ips, *ip)
				logger.Printf("pod vm IP[%d][public]=%s", len(ips)-1, ip.String())
			}
		}
	}

	// Finally add remaining private IPs (skip primary since already added)
	for i, ipc := range orderedIPCs {
		// Skip primary NIC's first IP config (already added above)
		if i == 0 {
			continue
		}
		addr := ipc.Properties.PrivateIPAddress
		if addr == nil {
			return nil, fmt.Errorf("private IP address not found in IP configuration")
		}
		ip, err := parseIP(*addr)
		if err != nil {
			return nil, err
		}
		ips = append(ips, *ip)
		logger.Printf("pod vm IP[%d][private]=%s", len(ips)-1, ip.String())
	}

	return ips, nil
}

func (p *azureProvider) create(ctx context.Context, parameters *armcompute.VirtualMachine) (*armcompute.VirtualMachine, error) {
	vmClient, err := armcompute.NewVirtualMachinesClient(p.serviceConfig.SubscriptionID, p.azureClient, nil)
	if err != nil {
		return nil, fmt.Errorf("creating VM client: %w", err)
	}

	vmName := *parameters.Properties.OSProfile.ComputerName

	pollerResponse, err := vmClient.BeginCreateOrUpdate(ctx, p.serviceConfig.ResourceGroupName, vmName, *parameters, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning VM creation or update: %w", err)
	}

	resp, err := pollerResponse.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("waiting for the VM creation: %w", err)
	}

	logger.Printf("created VM successfully: %s", *resp.ID)

	return &resp.VirtualMachine, nil
}

// buildNetworkConfig creates a single NIC configuration using the primary subnet.
// Used in single-NIC mode (unchanged from original behaviour).
//
// The Primary flag is explicitly set to true so that if the caller later
// switches to multi-NIC mode the control-plane NIC is unambiguously identified.
func (p *azureProvider) buildNetworkConfig(nicName string) *armcompute.VirtualMachineNetworkInterfaceConfiguration {
	ipConfig := armcompute.VirtualMachineNetworkInterfaceIPConfiguration{
		Name: to.Ptr("ip-config"),
		Properties: &armcompute.VirtualMachineNetworkInterfaceIPConfigurationProperties{
			Subnet: &armcompute.SubResource{
				ID: to.Ptr(p.serviceConfig.SubnetID),
			},
		},
	}

	if p.serviceConfig.UsePublicIP {
		publicIPConfig := armcompute.VirtualMachinePublicIPAddressConfiguration{
			Name: to.Ptr(nicName),
			Properties: &armcompute.VirtualMachinePublicIPAddressConfigurationProperties{
				// DeleteOptionsDelete ensures the public IP is automatically
				// removed when the VM is deleted — no explicit cleanup needed,
				// unlike AWS where ReleaseAddress must be called explicitly.
				DeleteOption: to.Ptr(armcompute.DeleteOptionsDelete),
			},
		}
		ipConfig.Properties.PublicIPAddressConfiguration = &publicIPConfig
	}

	config := armcompute.VirtualMachineNetworkInterfaceConfiguration{
		Name: to.Ptr(nicName),
		Properties: &armcompute.VirtualMachineNetworkInterfaceConfigurationProperties{
			// CHANGE: explicitly mark as primary so Azure and getIPs() agree on
			// which NIC carries control-plane traffic.
			Primary: to.Ptr(true),
			// DeleteOptionsDelete auto-cleans the NIC on VM deletion.
			// AWS equivalent: ModifyNetworkInterfaceAttribute(DeleteOnTermination: true).
			DeleteOption:     to.Ptr(armcompute.DeleteOptionsDelete),
			IPConfigurations: []*armcompute.VirtualMachineNetworkInterfaceIPConfiguration{&ipConfig},
		},
	}

	if p.serviceConfig.SecurityGroupID != "" {
		config.Properties.NetworkSecurityGroup = &armcompute.SubResource{
			ID: to.Ptr(p.serviceConfig.SecurityGroupID),
		}
	}

	return &config
}

// buildSingleNetworkConfig creates one NIC configuration for use in multi-NIC
// mode. The caller supplies all varying parameters so that primary and secondary
// NICs can be placed on different subnets with independent public IP settings.
//
// AWS equivalent: the primary NIC is configured inline in RunInstancesInput
// (NetworkInterfaces[DeviceIndex=0]); the secondary is created separately via
// CreateNetworkInterface + AttachNetworkInterface. Azure declares both at
// creation time, so a parameterised builder is the right abstraction.
func (p *azureProvider) buildSingleNetworkConfig(nicName string, isPrimary bool, withPublicIP bool, subnetID string) *armcompute.VirtualMachineNetworkInterfaceConfiguration {
	ipConfig := armcompute.VirtualMachineNetworkInterfaceIPConfiguration{
		Name: to.Ptr("ip-config"),
		Properties: &armcompute.VirtualMachineNetworkInterfaceIPConfigurationProperties{
			Subnet: &armcompute.SubResource{
				ID: to.Ptr(subnetID),
			},
			Primary: to.Ptr(isPrimary),
		},
	}

	if withPublicIP {
		publicIPConfig := armcompute.VirtualMachinePublicIPAddressConfiguration{
			Name: to.Ptr(nicName),
			Properties: &armcompute.VirtualMachinePublicIPAddressConfigurationProperties{
				// Auto-delete on VM termination — equivalent to AWS EIP
				// ReleaseAddress being called in deleteElasticIPforInstance().
				DeleteOption: to.Ptr(armcompute.DeleteOptionsDelete),
			},
		}
		ipConfig.Properties.PublicIPAddressConfiguration = &publicIPConfig
	}

	config := armcompute.VirtualMachineNetworkInterfaceConfiguration{
		Name: to.Ptr(nicName),
		Properties: &armcompute.VirtualMachineNetworkInterfaceConfigurationProperties{
			Primary: to.Ptr(isPrimary),
			// Auto-delete NIC on VM termination — equivalent to AWS
			// ModifyNetworkInterfaceAttribute(DeleteOnTermination: true).
			DeleteOption:     to.Ptr(armcompute.DeleteOptionsDelete),
			IPConfigurations: []*armcompute.VirtualMachineNetworkInterfaceIPConfiguration{&ipConfig},
		},
	}

	if p.serviceConfig.SecurityGroupID != "" {
		config.Properties.NetworkSecurityGroup = &armcompute.SubResource{
			ID: to.Ptr(p.serviceConfig.SecurityGroupID),
		}
	}

	return &config
}

// buildNetworkConfigs returns the full slice of NIC configurations to embed
// in the VM creation request.
//
// Single-NIC mode (multiNic == false):
//
//	One NIC on SubnetID, marked primary. Identical to original behaviour.
//
// Multi-NIC mode (multiNic == true):
//
//	Two NICs declared simultaneously at VM-creation time.
//	Azure does not support hot-attaching a NIC to a running VM without first
//	deallocating it, so unlike AWS (which creates and attaches the secondary
//	ENI after the instance is running) both NICs must be in the create payload.
//
//	  eth0 – primary NIC   – SubnetID          – no public IP
//	         Control-plane traffic: vxlan tunnel back to the worker node.
//
//	  eth1 – secondary NIC – SecondarySubnetID – public IP optional
//	         External connectivity for the pod VM.
//	         A separate subnet gives the guest kernel unambiguous routing and
//	         avoids the asymmetric-routing problem described in issue #2276.
//
//	SecondarySubnetID must be set and must differ from SubnetID.
//	This matches Azure's dual-NIC routing model and avoids ambiguous routing.
//
// AWS equivalent: the if spec.MultiNic { createAddonNICforInstance();
// createElasticIPforInstance() } block in CreateInstance.
func (p *azureProvider) buildNetworkConfigs(instanceName string, multiNic bool) []*armcompute.VirtualMachineNetworkInterfaceConfiguration {
	if !multiNic {
		// Single-NIC path — unchanged from original.
		nicName := fmt.Sprintf("%s-net", instanceName)
		return []*armcompute.VirtualMachineNetworkInterfaceConfiguration{
			p.buildNetworkConfig(nicName),
		}
	}

	// Multi-NIC path.
	logger.Printf("Creating multi-NIC configuration for instance %s", instanceName)

	secondarySubnetID := p.serviceConfig.SecondarySubnetID

	// Primary NIC — control-plane, no public IP.
	// AWS equivalent: DeviceIndex=0 NIC in RunInstancesInput.
	primaryNic := p.buildSingleNetworkConfig(
		fmt.Sprintf("%s-net", instanceName),
		true,  // isPrimary
		false, // no public IP on primary (control-plane) NIC
		p.serviceConfig.SubnetID,
	)

	// Secondary NIC — external connectivity, public IP optional.
	// AWS equivalent: DeviceIndex=1 ENI created by createAddonNICforInstance()
	// with an EIP optionally attached by createElasticIPforInstance().
	secondaryNic := p.buildSingleNetworkConfig(
		fmt.Sprintf("%s-ext", instanceName),
		false, // not primary
		p.serviceConfig.UsePublicIP,
		secondarySubnetID,
	)

	logger.Printf("Primary NIC subnet: %s | Secondary NIC subnet: %s | public IP on secondary: %v",
		p.serviceConfig.SubnetID, secondarySubnetID, p.serviceConfig.UsePublicIP)

	return []*armcompute.VirtualMachineNetworkInterfaceConfiguration{primaryNic, secondaryNic}
}

// CreateInstance creates a new Azure VM for a peer-pod sandbox.
//
// When spec.MultiNic is true (set by cloud.go after inspecting the pod netns
// and reading ExternalNetViaPodVM from config) two NICs are included in the VM
// creation payload so that:
//   - eth0 carries all control-plane / pod-network traffic via the vxlan tunnel.
//   - eth1 carries external traffic directly from the pod VM.
//
// AWS equivalent: the MultiNic block that calls createAddonNICforInstance() and
// optionally createElasticIPforInstance() after RunInstances returns. Azure
// does the equivalent work at creation time via buildNetworkConfigs().
func (p *azureProvider) CreateInstance(ctx context.Context, podName, sandboxID string, cloudConfig cloudinit.CloudConfigGenerator, spec provider.InstanceTypeSpec) (instance *provider.Instance, err error) {

	instanceName := util.GenerateInstanceName(podName, sandboxID, maxInstanceNameLen)

	cloudConfigData, err := cloudConfig.Generate()
	if err != nil {
		return nil, err
	}

	instanceSize, err := p.selectInstanceType(ctx, spec)
	if err != nil {
		return nil, err
	}

	diskName := fmt.Sprintf("%s-disk", instanceName)

	sshPublicKeyPath := os.ExpandEnv(p.serviceConfig.SSHKeyPath)
	var sshBytes []byte
	if sshPublicKeyPath != "" {
		logger.Printf("Using existing SSH public key from %s", sshPublicKeyPath)
		sshBytes, err = os.ReadFile(sshPublicKeyPath)
		if err != nil {
			err = fmt.Errorf("reading ssh public key file: %w", err)
			logger.Printf("%v", err)
			return nil, err
		}
	} else {
		logger.Printf("SSH public key path is empty, generating new public key")
		sshBytes, err = generateSSHPublicKey()
		if err != nil {
			err = fmt.Errorf("failed to generate SSH public key: %w", err)
			logger.Printf("%v", err)
			return nil, err
		}
	}

	imageID := p.serviceConfig.ImageID

	if spec.Image != "" {
		logger.Printf("Choosing %s from annotation as the Azure Image for the PodVM image", spec.Image)
		imageID = spec.Image
	}

	// CHANGE: read MultiNic from spec (set by cloud.go via podNetworkConfig.ExternalNetViaPodVM)
	// rather than deriving it locally from config presence.
	// AWS equivalent: spec.MultiNic is tested in the if spec.MultiNic { ... } block
	// in CreateInstance after RunInstances returns.
	multiNic := spec.MultiNic

	// CHANGE: nicName removed from call — buildNetworkConfigs now generates NIC
	// names internally so they are consistent between creation and getIPs().
	vmParameters, err := p.getVMParameters(instanceSize, diskName, cloudConfigData, sshBytes, instanceName, imageID, multiNic)
	if err != nil {
		return nil, err
	}

	logger.Printf("CreateInstance: name: %q, multi-NIC: %v", instanceName, multiNic)

	vm, err := p.create(ctx, vmParameters)
	if err != nil {
		return nil, fmt.Errorf("Creating instance (%v): %s", vm, err)
	}

	vmID := *vm.ID

	// Create partial instance to return on error (allows caller to cleanup).
	instance = &provider.Instance{
		ID:   vmID,
		Name: instanceName,
	}

	ips, err := p.getIPs(ctx, vm)
	if err != nil {
		logger.Printf("getting IPs for the instance : %v ", err)
		return instance, err
	}

	instance.IPs = ips

	return instance, nil
}

// DeleteInstance deletes the Azure VM.
//
// Unlike AWS, no explicit NIC or public IP cleanup is required here.
// Both the NIC(s) and any inline public IP configurations are created with
// DeleteOption: DeleteOptionsDelete, which instructs Azure to cascade-delete
// those resources when the VM is deleted.
//
// AWS equivalent: deleteElasticIPforInstance() (DisassociateAddress +
// ReleaseAddress) and the secondary ENI deletion are not needed in Azure
// because DeleteOptions handles lifecycle automatically.
func (p *azureProvider) DeleteInstance(ctx context.Context, instanceID string) error {
	vmClient, err := armcompute.NewVirtualMachinesClient(p.serviceConfig.SubscriptionID, p.azureClient, nil)
	if err != nil {
		return fmt.Errorf("creating VM client: %w", err)
	}

	// instanceID in the form of /subscriptions/<subID>/resourceGroups/<resource_name>/providers/Microsoft.Compute/virtualMachines/<VM_Name>.
	re := regexp.MustCompile(`^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Compute/virtualMachines/(.*)$`)
	match := re.FindStringSubmatch(instanceID)
	if len(match) < 1 {
		logger.Print("finding VM name using regexp:", match)
		return errNotFound
	}

	vmName := match[1]

	pollerResponse, err := vmClient.BeginDelete(ctx, p.serviceConfig.ResourceGroupName, vmName, nil)
	if err != nil {
		return fmt.Errorf("beginning VM deletion: %w", err)
	}

	if _, err = pollerResponse.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("waiting for the VM deletion: %w", err)
	}

	logger.Printf("deleted VM successfully: %s", vmName)
	return nil
}

func (p *azureProvider) Teardown() error {
	return nil
}

// ConfigVerifier validates the provider configuration at startup.
func (p *azureProvider) ConfigVerifier() error {
	imageID := p.serviceConfig.ImageID
	if len(imageID) == 0 {
		return fmt.Errorf("ImageId is empty")
	}

	if p.serviceConfig.SSHKeyPath != "" {
		if err := provider.VerifySSHKeyFile(p.serviceConfig.SSHKeyPath); err != nil {
			return fmt.Errorf("SSH key is invalid: %s", err)
		}
	}

	if p.serviceConfig.SecondarySubnetID != "" && p.serviceConfig.SecondarySubnetID == p.serviceConfig.SubnetID {
		return fmt.Errorf("azure secondary subnet id must differ from primary subnet id when configured")
	}

	if p.serviceConfig.UsePublicIP && p.serviceConfig.SecondarySubnetID == "" {
		return fmt.Errorf("azure secondary subnet id must be set when public IP is enabled for multi-nic external networking")
	}

	return nil
}

// selectInstanceType selects an instance type based on the memory and vcpu requirements.
func (p *azureProvider) selectInstanceType(ctx context.Context, spec provider.InstanceTypeSpec) (string, error) {
	return provider.SelectInstanceTypeToUse(spec, p.serviceConfig.InstanceSizeSpecList, p.serviceConfig.InstanceSizes, p.serviceConfig.Size)
}

// updateInstanceSizeSpecList populates InstanceSizeSpecList for all configured instance sizes.
func (p *azureProvider) updateInstanceSizeSpecList() error {

	vmSizesClient, err := armcompute.NewVirtualMachineSizesClient(p.serviceConfig.SubscriptionID, p.azureClient, nil)
	if err != nil {
		return fmt.Errorf("creating VM sizes client: %w", err)
	}

	instanceSizes := p.serviceConfig.InstanceSizes

	if len(instanceSizes) == 0 {
		instanceSizes = append(instanceSizes, p.serviceConfig.Size)
	}

	var instanceSizeSpecList []provider.InstanceTypeSpec

	pager := vmSizesClient.NewListPager(p.serviceConfig.Region, &armcompute.VirtualMachineSizesClientListOptions{})

	for pager.More() {
		nextResult, err := pager.NextPage(context.Background())
		if err != nil {
			return fmt.Errorf("getting next page of VM sizes: %w", err)
		}
		for _, vmSize := range nextResult.Value {
			if util.Contains(instanceSizes, *vmSize.Name) {
				instanceSizeSpecList = append(instanceSizeSpecList, provider.InstanceTypeSpec{InstanceType: *vmSize.Name, VCPUs: int64(*vmSize.NumberOfCores), Memory: int64(*vmSize.MemoryInMB)})
			}
		}
	}

	p.serviceConfig.InstanceSizeSpecList = provider.SortInstanceTypesOnResources(instanceSizeSpecList)
	logger.Printf("instanceSizeSpecList (%v)", p.serviceConfig.InstanceSizeSpecList)
	return nil
}

func (p *azureProvider) getResourceTags() map[string]*string {
	tags := map[string]*string{}
	for k, v := range p.serviceConfig.Tags {
		tags[k] = to.Ptr(v)
	}
	return tags
}

// getVMParameters builds the full VirtualMachine ARM payload.
//
// CHANGE: nicName parameter removed; multiNic bool added.
// NIC configuration is now delegated to buildNetworkConfigs() which handles
// both single- and multi-NIC layouts, returning a slice of configs that is
// passed directly into NetworkInterfaceConfigurations.
//
// AWS equivalent: the RunInstancesInput NetworkInterfaces field is set once
// (for the primary NIC with optional public IP) and the secondary NIC is
// handled post-launch. In Azure everything is declared upfront here.
func (p *azureProvider) getVMParameters(instanceSize, diskName, cloudConfig string, sshBytes []byte, instanceName, imageID string, multiNic bool) (*armcompute.VirtualMachine, error) {
	userDataB64 := base64.StdEncoding.EncodeToString([]byte(cloudConfig))

	if len(userDataB64) > 64*1024 {
		return nil, fmt.Errorf("base64 encoded userData is greater than 64KB")
	}

	var managedDiskParams *armcompute.ManagedDiskParameters
	var securityProfile *armcompute.SecurityProfile
	if !p.serviceConfig.DisableCVM {
		managedDiskParams = &armcompute.ManagedDiskParameters{
			StorageAccountType: to.Ptr(armcompute.StorageAccountTypesPremiumLRS),
			SecurityProfile: &armcompute.VMDiskSecurityProfile{
				SecurityEncryptionType: to.Ptr(armcompute.SecurityEncryptionTypesVMGuestStateOnly),
			},
		}

		securityProfile = &armcompute.SecurityProfile{
			SecurityType: to.Ptr(armcompute.SecurityTypesConfidentialVM),
			UefiSettings: &armcompute.UefiSettings{
				SecureBootEnabled: to.Ptr(p.serviceConfig.EnableSecureBoot),
				VTpmEnabled:       to.Ptr(true),
			},
		}
	} else {
		managedDiskParams = &armcompute.ManagedDiskParameters{
			StorageAccountType: to.Ptr(armcompute.StorageAccountTypesPremiumLRS),
		}
		securityProfile = nil
	}

	imgRef := &armcompute.ImageReference{
		ID: to.Ptr(imageID),
	}
	if strings.HasPrefix(imageID, "/CommunityGalleries/") {
		imgRef = &armcompute.ImageReference{
			CommunityGalleryImageID: to.Ptr(imageID),
		}
	}

	// CHANGE: buildNetworkConfigs replaces the single buildNetworkConfig call.
	// Returns [primaryNic] in single-NIC mode or [primaryNic, secondaryNic] in
	// multi-NIC mode — passed directly into NetworkInterfaceConfigurations.
	networkConfigs := p.buildNetworkConfigs(instanceName, multiNic)

	osDisk := &armcompute.OSDisk{
		Name:         to.Ptr(diskName),
		CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
		Caching:      to.Ptr(armcompute.CachingTypesReadWrite),
		DeleteOption: to.Ptr(armcompute.DiskDeleteOptionTypesDelete),
		ManagedDisk:  managedDiskParams,
	}

	if p.serviceConfig.RootVolumeSize > 0 {
		osDisk.DiskSizeGB = to.Ptr(int32(p.serviceConfig.RootVolumeSize))
		logger.Printf("Setting root volume size to %d GB", p.serviceConfig.RootVolumeSize)
	}

	vmParameters := armcompute.VirtualMachine{
		Location: to.Ptr(p.serviceConfig.Region),
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(instanceSize)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: imgRef,
				OSDisk:         osDisk,
			},
			OSProfile: &armcompute.OSProfile{
				AdminUsername: to.Ptr(p.serviceConfig.SSHUserName),
				ComputerName:  to.Ptr(instanceName),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: to.Ptr(true),
					SSH: &armcompute.SSHConfiguration{
						PublicKeys: []*armcompute.SSHPublicKey{{
							Path:    to.Ptr(fmt.Sprintf("/home/%s/.ssh/authorized_keys", p.serviceConfig.SSHUserName)),
							KeyData: to.Ptr(string(sshBytes)),
						}},
					},
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkAPIVersion:              to.Ptr(armcompute.NetworkAPIVersionTwoThousandTwenty1101),
				NetworkInterfaceConfigurations: networkConfigs,
			},
			SecurityProfile: securityProfile,
			DiagnosticsProfile: &armcompute.DiagnosticsProfile{
				BootDiagnostics: &armcompute.BootDiagnostics{
					Enabled: to.Ptr(true),
				},
			},
			UserData: to.Ptr(userDataB64),
		},
		Tags: p.getResourceTags(),
	}

	return &vmParameters, nil
}