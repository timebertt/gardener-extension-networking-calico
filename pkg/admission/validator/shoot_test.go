// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package validator

import (
	"context"

	"github.com/gardener/gardener/pkg/apis/core"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"github.com/gardener/gardener-extension-networking-calico/pkg/apis/calico/install"
)

var _ = Describe("Shoot validator migration", func() {
	const (
		nativeRouting = `{"apiVersion":"calico.networking.extensions.gardener.cloud/v1alpha1","kind":"NetworkConfig","overlay":{"enabled":false}}`
		overlay       = `{"apiVersion":"calico.networking.extensions.gardener.cloud/v1alpha1","kind":"NetworkConfig","overlay":{"enabled":true}}`
		noOverlayKey  = `{"apiVersion":"calico.networking.extensions.gardener.cloud/v1alpha1","kind":"NetworkConfig"}`
		unknownField  = `{"apiVersion":"calico.networking.extensions.gardener.cloud/v1alpha1","kind":"NetworkConfig","overlay":{"enabled":false},"doesNotExist":true}`
	)

	var s *shoot

	newShoot := func(networkingType, providerConfig string) *core.Shoot {
		networking := &core.Networking{
			Type:  new(networkingType),
			Pods:  new("10.0.0.0/16"),
			Nodes: new("10.1.0.0/16"),
		}
		if providerConfig != "" {
			networking.ProviderConfig = &runtime.RawExtension{Raw: []byte(providerConfig)}
		}
		return &core.Shoot{Spec: core.ShootSpec{Networking: networking}}
	}

	BeforeEach(func() {
		scheme := runtime.NewScheme()
		Expect(install.AddToScheme(scheme)).To(Succeed())
		s = &shoot{
			decoder:        serializer.NewCodecFactory(scheme, serializer.EnableStrict).UniversalDecoder(),
			lenientDecoder: serializer.NewCodecFactory(scheme).UniversalDecoder(),
		}
	})

	DescribeTable("#validateMigration",
		func(oldType, oldConfig, newType, newConfig string, matcher OmegaMatcher) {
			Expect(s.validateMigration(newShoot(oldType, oldConfig), newShoot(newType, newConfig))).To(matcher)
		},

		// no migration
		Entry("calico to calico without overlay config", "calico", "", "calico", "", Succeed()),
		Entry("calico to calico with overlay", "calico", overlay, "calico", overlay, Succeed()),
		Entry("other to other", "cilium", overlay, "other", "", Succeed()),

		// entering calico: the new config is checked
		Entry("to calico with native routing", "cilium", "", "calico", nativeRouting, Succeed()),
		Entry("to calico with overlay", "cilium", "", "calico", overlay, MatchError(ContainSubstring("overlay"))),
		Entry("to calico without overlay setting", "cilium", "", "calico", noOverlayKey, MatchError(ContainSubstring("overlay"))),
		Entry("to calico without providerConfig", "cilium", "", "calico", "", MatchError(ContainSubstring("overlay"))),
		// the other Extension (here Cilium) is responsible for checking the own config
		Entry("to calico ignores old config", "cilium", overlay, "calico", nativeRouting, Succeed()),
		Entry("to calico rejects unknown fields in the new config", "cilium", "", "calico", unknownField, HaveOccurred()),

		// leaving calico: the old config is checked
		Entry("from calico with native routing", "calico", nativeRouting, "cilium", "", Succeed()),
		Entry("from calico with overlay", "calico", overlay, "cilium", "", MatchError(ContainSubstring("overlay"))),
		Entry("from calico without providerConfig", "calico", "", "cilium", "", MatchError(ContainSubstring("overlay"))),
		Entry("from calico ignores new config", "calico", nativeRouting, "cilium", overlay, Succeed()),
	)

	Describe("#validateShootUpdate", func() {
		It("should not run calico validation on a shoot that leaves calico", func() {
			// The new providerConfig is a cilium config, which calico's strict decoding would reject.
			cilium := `{"apiVersion":"cilium.networking.extensions.gardener.cloud/v1alpha1","kind":"NetworkConfig"}`
			Expect(s.validateShootUpdate(context.Background(), newShoot("calico", nativeRouting), newShoot("cilium", cilium))).To(Succeed())
		})

		It("should still validate calico shoots that are not migrating", func() {
			invalid := newShoot("calico", nativeRouting)
			invalid.Spec.Networking.Pods = new("not-a-cidr")
			Expect(s.validateShootUpdate(context.Background(), newShoot("calico", nativeRouting), invalid)).To(MatchError(ContainSubstring("pods")))
		})
	})

	Describe("#validateShootCreation", func() {
		It("should skip shoots that are not calico", func() {
			Expect(s.validateShootCreation(context.Background(), newShoot("cilium", ""))).To(Succeed())
		})
	})
})
