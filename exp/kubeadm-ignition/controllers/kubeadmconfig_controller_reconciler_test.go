/*
Copyright 2020 The Kubernetes Authors.

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

package controllers

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1 "sigs.k8s.io/cluster-api/exp/kubeadm-ignition/api/v1alpha4"
)

var _ = Describe("KubeadmIgnitionConfigReconciler", func() {
	BeforeEach(func() {})
	AfterEach(func() {})

	Context("Reconcile a KubeadmIgnitionConfig", func() {
		It("should wait until infrastructure is ready", func() {
			cluster := newCluster("cluster1")
			Expect(testEnv.Create(ctx, cluster)).To(Succeed())

			machine := newMachine(cluster, "my-machine")
			Expect(testEnv.Create(ctx, machine)).To(Succeed())

			config := newKubeadmIgnitionConfig(machine, "my-machine-config")
			Expect(testEnv.Create(ctx, config)).To(Succeed())

			reconciler := KubeadmIgnitionConfigReconciler{
				Client: testEnv,
			}
			By("Calling reconcile should requeue")
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: client.ObjectKey{
					Namespace: "default",
					Name:      "my-machine-config",
				},
			})
			Expect(err).To(Succeed())
			Expect(result.Requeue).To(BeFalse())
		})
	})
})

// getKubeadmIgnitionConfig returns a KubeadmIgnitionConfig object from the cluster
func getKubeadmIgnitionConfig(c client.Client, name string) (*bootstrapv1.KubeadmIgnitionConfig, error) {
	controlplaneIgnitionConfigKey := client.ObjectKey{
		Namespace: "default",
		Name:      name,
	}
	config := &bootstrapv1.KubeadmIgnitionConfig{}
	err := c.Get(ctx, controlplaneIgnitionConfigKey, config)
	return config, err
}
