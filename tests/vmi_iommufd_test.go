/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package tests_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	expect "github.com/google/goexpect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/kubevirt/pkg/virt-config/featuregate"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"

	"kubevirt.io/kubevirt/tests/console"
	"kubevirt.io/kubevirt/tests/decorators"
	"kubevirt.io/kubevirt/tests/framework/checks"
	"kubevirt.io/kubevirt/tests/framework/kubevirt"
	"kubevirt.io/kubevirt/tests/libdomain"
	"kubevirt.io/kubevirt/tests/libkubevirt"
	kvconfig "kubevirt.io/kubevirt/tests/libkubevirt/config"
	"kubevirt.io/kubevirt/tests/libnet"
	"kubevirt.io/kubevirt/tests/libnode"
	"kubevirt.io/kubevirt/tests/libpod"
	"kubevirt.io/kubevirt/tests/libvmifact"
	"kubevirt.io/kubevirt/tests/libwait"
	"kubevirt.io/kubevirt/tests/testsuite"
)

const (
	iommufdPluginWait   = 30 * time.Second
	hostDeviceWait      = 1 * time.Minute
	allocatablePoll     = 5 * time.Second
	vmiDeleteTimeout    = 180 * time.Second
	guestExpectTimeout  = 15
	diskVerifyMemoryGi  = 2
	minIOMMUFDQuantity  = 1
	iommufdSoundCardRes = "example.org/soundcard-iommufd"
)

// Emulated PCI devices present on kubevirtci cluster-up nodes, bound to vfio-pci by gocli:
// https://github.com/kubevirt/kubevirtci/blob/main/cluster-provision/gocli/opts/bind-vfio/bind-vfio.go
var iommufdHostSoundCards = []string{
	"8086:2668", // Intel HD Audio Controller (ich6)
	"8086:293e", // Intel HD Audio Controller (ich9)
}

var _ = Describe("[sig-compute]IOMMUFD", Serial, decorators.IOMMUFD, decorators.SigCompute, func() {
	var (
		virtClient     kubecli.KubevirtClient
		config         v1.KubeVirtConfiguration
		hostDevsWasOff bool
		iommufdWasOff  bool
	)

	BeforeEach(func() {
		virtClient = kubevirt.Client()

		hostDevsWasOff = !checks.HasFeature(featuregate.HostDevicesGate)
		if hostDevsWasOff {
			kvconfig.EnableFeatureGate(featuregate.HostDevicesGate)
		}
		iommufdWasOff = !checks.HasFeature(featuregate.IOMMUFDGate)

		kv := libkubevirt.GetCurrentKv(virtClient)
		config = kv.Spec.Configuration
	})

	AfterEach(func() {
		restoreIOMMUFDTestConfig(virtClient, iommufdWasOff, hostDevsWasOff)
	})

	Context("with IOMMUFD feature gate enabled", func() {
		BeforeEach(func() {
			if iommufdWasOff {
				kvconfig.EnableFeatureGate(featuregate.IOMMUFDGate)
			}
			config = libkubevirt.GetCurrentKv(virtClient).Spec.Configuration
			skipUnlessAllocatable(virtClient, services.IOMMUFDDevice, iommufdPluginWait)
		})

		DescribeTable("with emulated PCI devices", func(deviceIDs ...string) {
			By("Adding the emulated sound card to the permitted host devices")
			hostDevs := permitPCIHostDevices(&config, iommufdSoundCardRes, deviceIDs)
			skipUnlessAllocatable(virtClient, iommufdSoundCardRes, hostDeviceWait)

			By("Creating a Fedora VMI with the sound card as a host device")
			vmi := createFedoraVMIWithHostDevices(virtClient, hostDevs)

			By("Verifying the domain XML contains IOMMUFD configuration")
			expectIOMMUFDEnabled(vmi)

			By("Verifying the launcher pod has the IOMMUFD device resource")
			expectLauncherIOMMUFDResource(vmi, true)

			By("Verifying the device is visible inside the guest")
			expectPCIDevicesInGuest(vmi, deviceIDs)

			By("Sleeping for manual inspection - VMI: " + vmi.Name)
			time.Sleep(10 * time.Minute)
		},
			Entry("should passthrough one PCI device with IOMMUFD", iommufdHostSoundCards[0]),
			Entry("should passthrough two PCI devices with IOMMUFD", iommufdHostSoundCards[0], iommufdHostSoundCards[1]),
		)
	})

	Context("with IOMMUFD feature gate disabled", func() {
		BeforeEach(func() {
			kvconfig.DisableFeatureGate(featuregate.IOMMUFDGate)
			config = libkubevirt.GetCurrentKv(virtClient).Spec.Configuration
		})

		It("should passthrough a PCI device with legacy VFIO and without IOMMUFD", func() {
			By("Adding the emulated sound card to the permitted host devices")
			hostDevs := permitPCIHostDevices(&config, iommufdSoundCardRes, []string{iommufdHostSoundCards[0]})
			skipUnlessAllocatable(virtClient, iommufdSoundCardRes, hostDeviceWait)

			By("Creating a Fedora VMI with a host device")
			vmi := createFedoraVMIWithHostDevices(virtClient, hostDevs)

			By("Verifying the domain XML does not contain IOMMUFD configuration")
			domSpec, err := libdomain.GetRunningVMIDomainSpec(vmi)
			Expect(err).ToNot(HaveOccurred())
			Expect(domSpec.IOMMUFD).To(BeNil(), "IOMMUFD should not be present when feature gate is disabled")

			By("Verifying the launcher pod does not have the IOMMUFD device resource")
			expectLauncherIOMMUFDResource(vmi, false)

			By("Verifying the device still works via legacy VFIO")
			expectPCIDevicesInGuest(vmi, []string{iommufdHostSoundCards[0]})

			By("Sleeping for manual inspection - VMI: " + vmi.Name)
			time.Sleep(10 * time.Minute)
		})
	})
})

func restoreIOMMUFDTestConfig(virtClient kubecli.KubevirtClient, iommufdWasOff, hostDevsWasOff bool) {
	if iommufdWasOff {
		kvconfig.DisableFeatureGate(featuregate.IOMMUFDGate)
	} else {
		kvconfig.EnableFeatureGate(featuregate.IOMMUFDGate)
	}
	if hostDevsWasOff {
		kvconfig.DisableFeatureGate(featuregate.HostDevicesGate)
	}

	kv := libkubevirt.GetCurrentKv(virtClient)
	config := kv.Spec.Configuration
	if config.DeveloperConfiguration != nil {
		config.DeveloperConfiguration.DiskVerification = nil
	}
	config.PermittedHostDevices = &v1.PermittedHostDevices{}
	kvconfig.UpdateKubeVirtConfigValueAndWait(config)
}

func nodesHaveAllocatableResource(virtClient kubecli.KubevirtClient, resourceName string) bool {
	nodes := libnode.GetAllSchedulableNodes(virtClient)
	name := k8sv1.ResourceName(resourceName)
	for _, node := range nodes.Items {
		qty, ok := node.Status.Allocatable[name]
		if ok && !qty.IsZero() {
			return true
		}
	}
	return false
}

func skipUnlessAllocatable(virtClient kubecli.KubevirtClient, resourceName string, timeout time.Duration) {
	GinkgoHelper()
	deadline := time.Now().Add(timeout)
	for {
		if nodesHaveAllocatableResource(virtClient, resourceName) {
			return
		}
		if !time.Now().Before(deadline) {
			Skip(fmt.Sprintf("no schedulable node has allocatable %s", resourceName)) //nolint:forbidigo
		}
		time.Sleep(allocatablePoll)
	}
}

func permitPCIHostDevices(config *v1.KubeVirtConfiguration, deviceName string, deviceIDs []string) []v1.HostDevice {
	GinkgoHelper()
	if config.DeveloperConfiguration == nil {
		config.DeveloperConfiguration = &v1.DeveloperConfiguration{}
	}
	config.DeveloperConfiguration.DiskVerification = &v1.DiskVerification{
		MemoryLimit: resource.NewScaledQuantity(diskVerifyMemoryGi, resource.Giga),
	}
	config.PermittedHostDevices = &v1.PermittedHostDevices{}
	var hostDevs []v1.HostDevice
	for i, id := range deviceIDs {
		config.PermittedHostDevices.PciHostDevices = append(config.PermittedHostDevices.PciHostDevices, v1.PciHostDevice{
			PCIVendorSelector: id,
			ResourceName:      deviceName,
		})
		hostDevs = append(hostDevs, v1.HostDevice{
			Name:       fmt.Sprintf("sound%d", i),
			DeviceName: deviceName,
		})
	}
	kvconfig.UpdateKubeVirtConfigValueAndWait(*config)
	return hostDevs
}

func createFedoraVMIWithHostDevices(virtClient kubecli.KubevirtClient, hostDevs []v1.HostDevice) *v1.VirtualMachineInstance {
	GinkgoHelper()
	vmi := libvmifact.NewFedora(libnet.WithMasqueradeNetworking())
	vmi.Spec.Domain.Devices.HostDevices = hostDevs
	vmi, err := virtClient.VirtualMachineInstance(testsuite.NamespaceTestDefault).Create(context.Background(), vmi, metav1.CreateOptions{})
	Expect(err).ToNot(HaveOccurred())

	name, namespace := vmi.Name, vmi.Namespace
	DeferCleanup(func() {
		_ = virtClient.VirtualMachineInstance(namespace).Delete(context.Background(), name, metav1.DeleteOptions{})
		Expect(libwait.WaitForVirtualMachineToDisappearWithTimeout(vmi, vmiDeleteTimeout)).To(Succeed())
	})

	return libwait.WaitForSuccessfulVMIStart(vmi)
}

func expectIOMMUFDEnabled(vmi *v1.VirtualMachineInstance) {
	GinkgoHelper()
	domSpec, err := libdomain.GetRunningVMIDomainSpec(vmi)
	Expect(err).ToNot(HaveOccurred())
	Expect(domSpec.IOMMUFD).ToNot(BeNil(), "IOMMUFD element should be present in domain XML")
	Expect(domSpec.IOMMUFD.Enabled).To(Equal("yes"))
	Expect(domSpec.IOMMUFD.FDGroup).To(Equal("iommu"))
}

func expectLauncherIOMMUFDResource(vmi *v1.VirtualMachineInstance, present bool) {
	GinkgoHelper()
	compute, err := libpod.LookupComputeContainerFromVmi(vmi)
	Expect(err).ToNot(HaveOccurred())
	quantity, found := compute.Resources.Limits[k8sv1.ResourceName(services.IOMMUFDDevice)]
	if present {
		Expect(found).To(BeTrue(), fmt.Sprintf("launcher pod should request %s", services.IOMMUFDDevice))
		Expect(quantity.Value()).To(BeNumerically(">=", minIOMMUFDQuantity))
	} else {
		Expect(found).To(BeFalse(), fmt.Sprintf("launcher pod should not request %s when feature gate is disabled", services.IOMMUFDDevice))
	}
}

func expectPCIDevicesInGuest(vmi *v1.VirtualMachineInstance, deviceIDs []string) {
	GinkgoHelper()
	Expect(console.LoginToFedora(vmi)).To(Succeed())
	for _, id := range deviceIDs {
		Expect(console.SafeExpectBatch(vmi, []expect.Batcher{
			&expect.BSnd{S: "grep -c " + strings.Replace(id, ":", "", 1) + " /proc/bus/pci/devices\n"},
			&expect.BExp{R: console.RetValue("1")},
		}, guestExpectTimeout)).To(Succeed(), fmt.Sprintf("device %s not found inside guest", id))
	}
}
