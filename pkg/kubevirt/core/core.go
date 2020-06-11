/*
Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved.

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

// Package kubevirt contains the cloud kubevirt specific implementations to manage machines
package core

import (
	"context"
	"fmt"
	"time"

	"github.com/moadqassem/machine-controller-manager-provider-kubevirt/pkg/kubevirt"
	clouderrors "github.com/moadqassem/machine-controller-manager-provider-kubevirt/pkg/kubevirt/errors"

	"gopkg.in/yaml.v2"

	api "github.com/moadqassem/machine-controller-manager-provider-kubevirt/pkg/kubevirt/apis"

	kubevirtv1 "kubevirt.io/client-go/api/v1"
	cdi "kubevirt.io/containerized-data-importer/pkg/apis/core/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
	utilpointer "k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ProviderName = "kubevirt"

// PluginSPIImpl is the real implementation of PluginSPI interface
// that makes the calls to the provider SDK
type PluginSPIImpl struct{}

func (p PluginSPIImpl) CreateMachine(ctx context.Context, machineName string, providerSpec *api.KubeVirtProviderSpec, secrets *corev1.Secret) (providerID string, err error) {
	//TODO(MQ): adding support for ignition.
	requestsAndLimits, err := kubevirt.ParseResources(providerSpec.CPUs, providerSpec.Memory)
	if err != nil {
		return "", err
	}

	pvcSize, err := resource.ParseQuantity(providerSpec.PVCSize)
	if err != nil {
		return "", fmt.Errorf("failed to parse value of pvcSize field: %v", err)
	}

	var (
		pvcRequest                    = corev1.ResourceList{corev1.ResourceStorage: pvcSize}
		terminationGracePeriodSeconds = int64(30)

		dnsPolicy corev1.DNSPolicy
		dnsConfig *corev1.PodDNSConfig
	)

	if providerSpec.DNSPolicy != "" {
		dnsPolicy, err = kubevirt.DNSPolicy(providerSpec.DNSPolicy)
		if err != nil {
			return "", fmt.Errorf("invalid dns policy: %v", err)
		}
	}

	if providerSpec.DNSConfig != "" {
		if err := yaml.Unmarshal([]byte(providerSpec.DNSConfig), dnsConfig); err != nil {
			return "", fmt.Errorf(`failed to unmarshal "dnsConfig" field: %v`, err)
		}
	}

	virtualMachine := &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: providerSpec.Namespace,
			Labels: map[string]string{
				"kubevirt.io/vm": machineName,
			},
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			Running: utilpointer.BoolPtr(true),
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"kubevirt.io/vm": machineName,
					},
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Devices: kubevirtv1.Devices{
							Disks: []kubevirtv1.Disk{
								{
									Name:       "datavolumedisk",
									DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}},
								},
								{
									Name:       "cloudinitdisk",
									DiskDevice: kubevirtv1.DiskDevice{Disk: &kubevirtv1.DiskTarget{Bus: "virtio"}},
								},
							},
						},
						Resources: kubevirtv1.ResourceRequirements{
							Requests: *requestsAndLimits,
							Limits:   *requestsAndLimits,
						},
					},
					TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
					Volumes: []kubevirtv1.Volume{
						{
							Name: "datavolumedisk",
							VolumeSource: kubevirtv1.VolumeSource{
								DataVolume: &kubevirtv1.DataVolumeSource{
									Name: machineName,
								},
							},
						},
						{
							Name: "cloudinitdisk",
							VolumeSource: kubevirtv1.VolumeSource{
								CloudInitNoCloud: &kubevirtv1.CloudInitNoCloudSource{
									UserDataSecretRef: &corev1.LocalObjectReference{
										Name: secrets.Name,
									},
								},
							},
						},
					},
					DNSPolicy: dnsPolicy,
					DNSConfig: dnsConfig,
				},
			},
			DataVolumeTemplates: []cdi.DataVolume{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: machineName,
					},
					Spec: cdi.DataVolumeSpec{
						PVC: &corev1.PersistentVolumeClaimSpec{
							StorageClassName: utilpointer.StringPtr(providerSpec.StorageClassName),
							AccessModes: []corev1.PersistentVolumeAccessMode{
								"ReadWriteOnce",
							},
							Resources: corev1.ResourceRequirements{
								Requests: pvcRequest,
							},
						},
						Source: cdi.DataVolumeSource{
							HTTP: &cdi.DataVolumeSourceHTTP{
								URL: providerSpec.SourceURL,
							},
						},
					},
				},
			},
		},
	}

	kubevirtClient, err := kubevirtClient(secrets)
	if err != nil {
		return "", fmt.Errorf("failed to get kubevirt client: %v", err)
	}

	if err := kubevirtClient.Create(ctx, virtualMachine); err != nil {
		return "", fmt.Errorf("failed to create vmi: %v", err)
	}

	// TODO(MQ): do we really need this? another approach is to return an empty provider id and leave it to the
	// Get
	return machineProviderID(secrets, machineName, providerSpec.Namespace)
}

func (p PluginSPIImpl) DeleteMachine(ctx context.Context, machineName, providerID string, providerSpec *api.KubeVirtProviderSpec, secrets *corev1.Secret) (foundProviderID string, err error) {
	vm, err := getVM(secrets, machineName, providerSpec.Namespace)
	if err != nil {
		return "", fmt.Errorf("failed to find vm %v: %v", machineName, err)
	}

	kubevirtClient, err := kubevirtClient(secrets)
	if err != nil {
		return "", fmt.Errorf("failed to create kubevirt client: %v", err)
	}

	if err := kubevirtClient.Delete(context.Background(), vm); err != nil {
		return "", fmt.Errorf("failed to delete vm %v: %v", machineName, err)
	}

	return encodeProviderID(string(vm.UID)), nil
}

func (p PluginSPIImpl) GetMachineStatus(ctx context.Context, machineName, providerID string, providerSpec *api.KubeVirtProviderSpec, secrets *corev1.Secret) (foundProviderID string, err error) {
	panic("implement me")
}

func (p PluginSPIImpl) ListMachines(ctx context.Context, providerSpec *api.KubeVirtProviderSpec, secrets *corev1.Secret) (providerIDList map[string]string, err error) {
	panic("implement me")
}

func (p PluginSPIImpl) ShutDownMachine(ctx context.Context, machineName, providerID string, providerSpec *api.KubeVirtProviderSpec, secrets *corev1.Secret) (foundProviderID string, err error) {
	panic("implement me")
}

func getVM(secret *corev1.Secret, machineName, namespace string) (*kubevirtv1.VirtualMachine, error) {
	client, err := kubevirtClient(secret)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubevirt client: %v", err)
	}

	virtualMachine := &kubevirtv1.VirtualMachine{}
	if err := client.Get(context.TODO(), types.NamespacedName{Namespace: namespace, Name: machineName}, virtualMachine); err != nil {
		if !kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get VirtualMachine %s: %v", machineName, err)
		}
		return nil, clouderrors.ErrInstanceNotFound
	}

	return virtualMachine, err
}

func kubevirtClient(secret *corev1.Secret) (client.Client, error) {
	kubeconfig, kubevirtKubeconifgCheck := secret.Data["kubeconfig"]
	if !kubevirtKubeconifgCheck {
		return nil, fmt.Errorf("kubevirt kubeconfig is not found")
	}

	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to decode kubeconfig: %v", err)
	}

	return client.New(config, client.Options{})
}

func machineProviderID(secret *corev1.Secret, vmName, namespace string) (string, error) {
	var vm = &kubevirtv1.VirtualMachine{}
	if err := wait.Poll(500*time.Microsecond, 60*time.Second, func() (done bool, err error) {
		vm, err = getVM(secret, vmName, namespace)
		if err != nil {
			if err == clouderrors.ErrInstanceNotFound {
				klog.V(2).Infof("vm %v is not found, retrying...", vmName)
				return false, nil
			}

			return false, err
		}
		return true, nil
	}); err != nil {
		return "", fmt.Errorf("failed to find vm %v forever", vmName)
	}

	return encodeProviderID(string(vm.UID)), nil
}
