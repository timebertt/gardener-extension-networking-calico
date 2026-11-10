// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package validator

import (
	"context"
	"fmt"

	extensionswebhook "github.com/gardener/gardener/extensions/pkg/webhook"
	gardencorehelper "github.com/gardener/gardener/pkg/api/core/helper"
	"github.com/gardener/gardener/pkg/apis/core"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/gardener/gardener-extension-networking-calico/pkg/apis/calico"
	calicov1alpha1 "github.com/gardener/gardener-extension-networking-calico/pkg/apis/calico/v1alpha1"
	calicov1alpha1helper "github.com/gardener/gardener-extension-networking-calico/pkg/apis/calico/v1alpha1/helper"
	calicovalidation "github.com/gardener/gardener-extension-networking-calico/pkg/apis/calico/validation"
	calicotype "github.com/gardener/gardener-extension-networking-calico/pkg/calico"
)

// NewShootValidator returns a new instance of a shoot validator.
func NewShootValidator(mgr manager.Manager) extensionswebhook.Validator {
	return &shoot{
		client:         mgr.GetClient(),
		apiReader:      mgr.GetAPIReader(),
		decoder:        serializer.NewCodecFactory(mgr.GetScheme(), serializer.EnableStrict).UniversalDecoder(),
		lenientDecoder: serializer.NewCodecFactory(mgr.GetScheme()).UniversalDecoder(),
	}
}

type shoot struct {
	client         client.Client
	apiReader      client.Reader
	decoder        runtime.Decoder
	lenientDecoder runtime.Decoder
}

// Validate validates the given shoot object.
func (s *shoot) Validate(ctx context.Context, new, old client.Object) error {
	shoot, ok := new.(*core.Shoot)
	if !ok {
		return fmt.Errorf("wrong object type %T", new)
	}

	// Skip if it's a workerless Shoot
	if gardencorehelper.IsWorkerless(shoot) {
		return nil
	}

	shootV1Beta1 := &gardencorev1beta1.Shoot{}
	err := gardencorev1beta1.Convert_core_Shoot_To_v1beta1_Shoot(shoot, shootV1Beta1, nil)
	if err != nil {
		return err
	}

	if old != nil {
		oldShoot, ok := old.(*core.Shoot)
		if !ok {
			return fmt.Errorf("wrong object type %T for old object", old)
		}
		return s.validateShootUpdate(ctx, oldShoot, shoot)
	}

	return s.validateShootCreation(ctx, shoot)
}

func (s *shoot) validateShoot(_ context.Context, shoot *core.Shoot) error {
	// Network validation
	if shoot.Spec.Networking != nil {
		if errList := calicovalidation.ValidateNetworking(shoot.Spec.Networking, field.NewPath("spec", "networking")); len(errList) != 0 {
			return errList.ToAggregate()
		}

		if shoot.Spec.Networking.ProviderConfig != nil {
			network := &extensionsv1alpha1.Network{}
			network.Spec.ProviderConfig = shoot.Spec.Networking.ProviderConfig
			networkConfig, err := calicov1alpha1helper.CalicoNetworkConfigFromNetworkResource(network)
			if err != nil {
				return err
			}

			ipFamilies := shoot.Spec.Networking.IPFamilies

			internalNetworkConfig := &calico.NetworkConfig{}
			err = calicov1alpha1.Convert_v1alpha1_NetworkConfig_To_calico_NetworkConfig(networkConfig, internalNetworkConfig, nil)
			if err != nil {
				return err
			}

			if errList := calicovalidation.ValidateNetworkConfig(internalNetworkConfig, ipFamilies, field.NewPath("spec", "networking", "providerConfig")); len(errList) != 0 {
				return errList.ToAggregate()
			}
		}
	}

	networkConfig, err := s.decodeNetworkingConfig(shoot.Spec.Networking.ProviderConfig)
	if err != nil {
		return err
	}

	if shoot.Spec.Kubernetes.KubeProxy != nil {
		if shoot.Spec.Kubernetes.KubeProxy.Enabled != nil {
			if !*shoot.Spec.Kubernetes.KubeProxy.Enabled {
				if networkConfig.EbpfDataplane == nil || (networkConfig.EbpfDataplane != nil && !networkConfig.EbpfDataplane.Enabled) {
					return field.Forbidden(field.NewPath("spec", "kubernetes", "kubeProxy", "enabled"), "Disabling kube-proxy is forbidden in conjunction with calico without running in ebpf dataplane")
				}
			}
		}
	}

	return nil
}

func (s *shoot) validateShootUpdate(ctx context.Context, oldShoot, shoot *core.Shoot) error {
	if err := s.validateMigration(oldShoot, shoot); err != nil {
		return err
	}

	// Leaving calico means we don't need to validate the networking config by this extension.
	if networkingType(shoot) != calicotype.Type {
		return nil
	}

	return s.validateShoot(ctx, shoot)
}

func (s *shoot) validateShootCreation(ctx context.Context, shoot *core.Shoot) error {
	if networkingType(shoot) != calicotype.Type {
		return nil
	}

	return s.validateShoot(ctx, shoot)
}

// validateMigration ensures that migrations to or from calico only happen with native routing (overlay disabled),
// as the migration strategy relies on native routing and a routing controller.
func (s *shoot) validateMigration(oldShoot, shoot *core.Shoot) error {
	oldIsCalico := networkingType(oldShoot) == calicotype.Type
	newIsCalico := networkingType(shoot) == calicotype.Type
	if oldIsCalico == newIsCalico {
		return nil
	}

	// Leaving calico: the old config is relevant. Entering calico: the new one.
	target, msg := shoot, "overlay must be explicitly disabled (overlay.enabled=false) to switch to calico"
	if oldIsCalico {
		target, msg = oldShoot, "cannot switch from calico unless the overlay is explicitly disabled, set overlay.enabled=false first"
	}

	providerConfigPath := field.NewPath("spec", "networking", "providerConfig")
	if target.Spec.Networking == nil || target.Spec.Networking.ProviderConfig == nil || len(target.Spec.Networking.ProviderConfig.Raw) == 0 {
		return field.ErrorList{field.Forbidden(providerConfigPath.Child("overlay"), msg)}.ToAggregate()
	}

	network := &extensionsv1alpha1.Network{}
	network.Spec.ProviderConfig = target.Spec.Networking.ProviderConfig
	networkConfig, err := calicov1alpha1helper.CalicoNetworkConfigFromNetworkResource(network)
	if err != nil {
		return field.ErrorList{field.Invalid(providerConfigPath, string(target.Spec.Networking.ProviderConfig.Raw), err.Error())}.ToAggregate()
	}

	// Require explicit overlay off for strictness.
	if networkConfig.Overlay == nil || networkConfig.Overlay.Enabled {
		return field.ErrorList{field.Forbidden(providerConfigPath.Child("overlay"), msg)}.ToAggregate()
	}

	return nil
}

func networkingType(shoot *core.Shoot) string {
	if shoot.Spec.Networking == nil || shoot.Spec.Networking.Type == nil {
		return ""
	}
	return *shoot.Spec.Networking.Type
}

func (s *shoot) decodeNetworkingConfig(network *runtime.RawExtension) (*calicov1alpha1.NetworkConfig, error) {
	networkConfig := &calicov1alpha1.NetworkConfig{}
	if network != nil && network.Raw != nil {
		if _, _, err := s.decoder.Decode(network.Raw, nil, networkConfig); err != nil {
			return nil, err
		}
	}
	return networkConfig, nil
}
