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
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha3"
	capierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const defaultNamespaceName = "default"

var _ = Describe("MachineHealthCheck Reconciler", func() {
	var namespace *corev1.Namespace
	var testCluster *clusterv1.Cluster

	var clusterName = "test-cluster"
	var clusterKubeconfigName = "test-cluster-kubeconfig"
	var namespaceName string

	BeforeEach(func() {
		namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mhc-test-"}}
		testCluster = &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName}}

		By("Ensuring the namespace exists")
		Expect(testEnv.Create(ctx, namespace)).To(Succeed())
		namespaceName = namespace.Name

		By("Creating the Cluster")
		testCluster.Namespace = namespaceName
		Expect(testEnv.Create(ctx, testCluster)).To(Succeed())

		By("Creating the remote Cluster kubeconfig")
		Expect(testEnv.CreateKubeconfigSecret(testCluster)).To(Succeed())
	})

	AfterEach(func() {
		By("Deleting any Nodes")
		Expect(cleanupTestNodes(ctx, testEnv)).To(Succeed())
		By("Deleting any Machines")
		Expect(cleanupTestMachines(ctx, testEnv)).To(Succeed())
		By("Deleting any MachineHealthChecks")
		Expect(cleanupTestMachineHealthChecks(ctx, testEnv)).To(Succeed())
		By("Deleting the Cluster")
		Expect(testEnv.Delete(ctx, testCluster)).To(Succeed())
		By("Deleting the remote Cluster kubeconfig")
		remoteClusterKubeconfig := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespaceName, Name: clusterKubeconfigName}}
		Expect(testEnv.Delete(ctx, remoteClusterKubeconfig)).To(Succeed())
		By("Deleting the Namespace")
		Expect(testEnv.Delete(ctx, namespace)).To(Succeed())

		// Ensure the cluster is actually gone before moving on
		Eventually(func() error {
			c := &clusterv1.Cluster{}
			err := testEnv.Get(ctx, client.ObjectKey{Namespace: namespaceName, Name: clusterName}, c)
			if err != nil && apierrors.IsNotFound(err) {
				return nil
			} else if err != nil {
				return err
			}
			return errors.New("Cluster not yet deleted")
		}, timeout).Should(Succeed())
	})

	Context("when reconciling a MachineHealthCheck", func() {

		// createMachine creates a machine while also maintaining the status that
		// has been set on it
		createMachine := func(m *clusterv1.Machine) {
			status := m.Status
			Expect(testEnv.Create(ctx, m)).To(Succeed())
			key := util.ObjectKey(m)
			Eventually(func() error {
				if err := testEnv.Get(ctx, key, m); err != nil {
					return err
				}
				m.Status = status
				return testEnv.Status().Update(ctx, m)
			}, timeout).Should(Succeed())
		}

		// createNode creates a Node while also maintaining the status that
		// has been set on it
		createNode := func(n *corev1.Node) {
			status := n.Status
			Expect(testEnv.Create(ctx, n)).To(Succeed())
			key := util.ObjectKey(n)
			Eventually(func() error {
				if err := testEnv.Get(ctx, key, n); err != nil {
					return err
				}
				n.Status = status
				return testEnv.Status().Update(ctx, n)
			}, timeout).Should(Succeed())
		}

		// getMHCStatus is a function to be used in Eventually() matchers to check the MHC status
		getMHCStatus := func(namespace, name string) func() clusterv1.MachineHealthCheckStatus {
			return func() clusterv1.MachineHealthCheckStatus {
				mhc := &clusterv1.MachineHealthCheck{}
				if err := testEnv.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, mhc); err != nil {
					return clusterv1.MachineHealthCheckStatus{}
				}
				return mhc.Status
			}
		}

		type reconcileTestCase struct {
			mhc                 func() *clusterv1.MachineHealthCheck
			nodes               func() []*corev1.Node
			machines            func() []*clusterv1.Machine
			expectHealthy       func() []*clusterv1.Machine
			expectUnhealthy     func() []*clusterv1.Machine
			expectRemediation   func() []*clusterv1.Machine
			expectNoRemediation func() []*clusterv1.Machine
			expectedStatus      clusterv1.MachineHealthCheckStatus
		}

		var labels = map[string]string{"cluster": clusterName, "nodepool": "foo"}
		var healthyNodeCondition = corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}
		var unhealthyNodeCondition = corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionUnknown, LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute))}

		// Objects for use in test cases below
		var testMHC, testMHCWithMaxUnhealthy *clusterv1.MachineHealthCheck
		var healthyNode1, healthyNode2, unhealthyNode1, unhealthyNode2, unlabelledNode *corev1.Node
		var healthyMachine1, healthyMachine2, unhealthyMachine1, unhealthyMachine2, noNodeRefMachine1, noNodeRefMachine2, nodeGoneMachine1, unlabelledMachine *clusterv1.Machine

		BeforeEach(func() {
			// Set up objects for test cases before each test
			By("Setting up resources")
			testMHC = newTestMachineHealthCheck("test-mhc", namespaceName, clusterName, labels)
			testMHC.Default()
			testMHCWithMaxUnhealthy = newTestMachineHealthCheck("test-mhc-with-max-unhealthy", namespaceName, clusterName, labels)
			maxUnhealthy := intstr.Parse("40%")
			testMHCWithMaxUnhealthy.Spec.MaxUnhealthy = &maxUnhealthy
			testMHCWithMaxUnhealthy.Default()

			healthyNode1 = newTestNode("healthy-node-1")
			healthyNode1.Status.Conditions = []corev1.NodeCondition{healthyNodeCondition}
			healthyMachine1 = newTestMachine("healthy-machine-1", namespaceName, clusterName, healthyNode1.Name, labels)

			healthyNode2 = newTestNode("healthy-node-2")
			healthyNode2.Status.Conditions = []corev1.NodeCondition{healthyNodeCondition}
			healthyMachine2 = newTestMachine("healthy-machine-2", namespaceName, clusterName, healthyNode2.Name, labels)

			unhealthyNode1 = newTestNode("unhealthy-node-1")
			unhealthyNode1.Status.Conditions = []corev1.NodeCondition{unhealthyNodeCondition}
			unhealthyMachine1 = newTestMachine("unhealthy-machine-1", namespaceName, clusterName, unhealthyNode1.Name, labels)

			unhealthyNode2 = newTestNode("unhealthy-node-2")
			unhealthyNode2.Status.Conditions = []corev1.NodeCondition{unhealthyNodeCondition}
			unhealthyMachine2 = newTestMachine("unhealthy-machine-2", namespaceName, clusterName, unhealthyNode2.Name, labels)

			noNodeRefMachine1 = newTestMachine("no-node-ref-machine-1", namespaceName, clusterName, "", labels)
			noNodeRefMachine1.Status.NodeRef = nil
			now := metav1.NewTime(time.Now())
			noNodeRefMachine1.Status.LastUpdated = &now

			noNodeRefMachine2 = newTestMachine("no-node-ref-machine-2", namespaceName, clusterName, "", labels)
			noNodeRefMachine2.Status.NodeRef = nil
			lastUpdatedTwiceNodeStartupTimeout := metav1.NewTime(time.Now().Add(-2 * testMHC.Spec.NodeStartupTimeout.Duration))
			noNodeRefMachine2.Status.LastUpdated = &lastUpdatedTwiceNodeStartupTimeout

			nodeGoneMachine1 = newTestMachine("node-gone-machine-1", namespaceName, clusterName, "node-gone-node-1", labels)

			unlabelledNode = newTestNode("unlabelled-node")
			unlabelledMachine = newTestMachine("unlabelled-machine", namespaceName, clusterName, unlabelledNode.Name, map[string]string{})
		})

		DescribeTable("should mark unhealthy nodes for remediation",
			func(rtc *reconcileTestCase) {
				By("Creating a MachineHealthCheck")
				mhc := rtc.mhc()
				mhc.Default()
				Expect(testEnv.Create(ctx, mhc)).To(Succeed())

				By("Creating Machines")
				for _, m := range rtc.machines() {
					createMachine(m.DeepCopy())
				}

				By("Creating Nodes")
				for _, n := range rtc.nodes() {
					createNode(n.DeepCopy())
				}

				allThatShouldGetConditions := append(rtc.expectNoRemediation(), rtc.expectRemediation()...)
				allThatShouldGetConditions = append(allThatShouldGetConditions, rtc.expectUnhealthy()...)
				allThatShouldGetConditions = append(allThatShouldGetConditions, rtc.expectHealthy()...)
				Eventually(func() []*clusterv1.Machine {
					var hasCondition []*clusterv1.Machine
					for _, m := range allThatShouldGetConditions {
						machine := &clusterv1.Machine{}
						key := types.NamespacedName{Namespace: m.Namespace, Name: m.Name}
						Expect(testEnv.Get(ctx, key, machine)).To(Succeed())
						if conditions.Has(machine, clusterv1.MachineHealthCheckSuccededCondition) {
							hasCondition = append(hasCondition, m)
						}
					}
					return hasCondition
				}, timeout).Should(ConsistOf(allThatShouldGetConditions), "Expected all machines to have received a HealthCheckSucceeded condition")
				// All machines have been health checked, assume it is safe to continue

				By("Verifying the status has been updated")
				Eventually(getMHCStatus(namespaceName, rtc.mhc().Name), 20*time.Second).Should(Equal(rtc.expectedStatus))

				By("Verifying Machine conditions")
				for _, m := range rtc.expectHealthy() {
					machine := &clusterv1.Machine{}
					key := types.NamespacedName{Namespace: m.Namespace, Name: m.Name}
					Expect(testEnv.Get(ctx, key, machine)).To(Succeed())
					Expect(conditions.IsTrue(machine, clusterv1.MachineHealthCheckSuccededCondition)).To(BeTrue(), fmt.Sprintf("Expected machine %q to have passed healthcheck", machine.Name))
				}
				for _, m := range rtc.expectUnhealthy() {
					machine := &clusterv1.Machine{}
					key := types.NamespacedName{Namespace: m.Namespace, Name: m.Name}
					Expect(testEnv.Get(ctx, key, machine)).To(Succeed())
					Expect(conditions.IsFalse(machine, clusterv1.MachineHealthCheckSuccededCondition)).To(BeTrue(), fmt.Sprintf("Expected machine %q to have failed healthcheck", machine.Name))
				}
				for _, m := range rtc.expectRemediation() {
					machine := &clusterv1.Machine{}
					key := types.NamespacedName{Namespace: m.Namespace, Name: m.Name}
					Expect(testEnv.Get(ctx, key, machine)).To(Succeed())
					Expect(conditions.IsFalse(machine, clusterv1.MachineOwnerRemediatedCondition)).To(BeTrue(), fmt.Sprintf("Expected machine %q to need remediation", machine.Name))
				}
				for _, m := range rtc.expectNoRemediation() {
					machine := &clusterv1.Machine{}
					key := types.NamespacedName{Namespace: m.Namespace, Name: m.Name}
					Expect(testEnv.Get(ctx, key, machine)).To(Succeed())
					Expect(conditions.Get(machine, clusterv1.MachineOwnerRemediatedCondition)).To(BeNil(), fmt.Sprintf("Expected machine %q to not need remediation", machine.Name))
				}
			},
			Entry("with healthy Machines", &reconcileTestCase{
				mhc:                 func() *clusterv1.MachineHealthCheck { return testMHC },
				nodes:               func() []*corev1.Node { return []*corev1.Node{healthyNode1, healthyNode2} },
				machines:            func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectUnhealthy:     none,
				expectRemediation:   none,
				expectHealthy:       func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectNoRemediation: func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectedStatus:      clusterv1.MachineHealthCheckStatus{ExpectedMachines: 2, CurrentHealthy: 2},
			}),
			Entry("with an unhealthy Machine", &reconcileTestCase{
				mhc:   func() *clusterv1.MachineHealthCheck { return testMHC },
				nodes: func() []*corev1.Node { return []*corev1.Node{healthyNode1, healthyNode2, unhealthyNode1} },
				machines: func() []*clusterv1.Machine {
					return []*clusterv1.Machine{healthyMachine1, healthyMachine2, unhealthyMachine1}
				},
				expectUnhealthy:     func() []*clusterv1.Machine { return []*clusterv1.Machine{unhealthyMachine1} },
				expectRemediation:   func() []*clusterv1.Machine { return []*clusterv1.Machine{unhealthyMachine1} },
				expectHealthy:       func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectNoRemediation: func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectedStatus:      clusterv1.MachineHealthCheckStatus{ExpectedMachines: 3, CurrentHealthy: 2},
			}),
			Entry("when the unhealthy Machines exceed MaxUnhealthy", &reconcileTestCase{
				mhc:   func() *clusterv1.MachineHealthCheck { return testMHCWithMaxUnhealthy },
				nodes: func() []*corev1.Node { return []*corev1.Node{healthyNode1, unhealthyNode1, unhealthyNode2} },
				machines: func() []*clusterv1.Machine {
					return []*clusterv1.Machine{healthyMachine1, unhealthyMachine1, unhealthyMachine2}
				},
				expectRemediation: none,
				expectNoRemediation: func() []*clusterv1.Machine {
					return []*clusterv1.Machine{healthyMachine1, unhealthyMachine1, unhealthyMachine2}
				},
				expectUnhealthy: func() []*clusterv1.Machine { return []*clusterv1.Machine{unhealthyMachine1, unhealthyMachine2} },
				expectHealthy:   func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1} },
				expectedStatus:  clusterv1.MachineHealthCheckStatus{ExpectedMachines: 3, CurrentHealthy: 1},
			}),
			Entry("when a Machine has no Node ref for less than the NodeStartupTimeout", &reconcileTestCase{
				mhc:   func() *clusterv1.MachineHealthCheck { return testMHC },
				nodes: func() []*corev1.Node { return []*corev1.Node{healthyNode1, healthyNode2} },
				machines: func() []*clusterv1.Machine {
					return []*clusterv1.Machine{healthyMachine1, healthyMachine2, noNodeRefMachine1}
				},
				expectUnhealthy:     none,
				expectRemediation:   none,
				expectHealthy:       func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectNoRemediation: func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectedStatus:      clusterv1.MachineHealthCheckStatus{ExpectedMachines: 3, CurrentHealthy: 2},
			}),
			Entry("when a Machine has no Node ref for longer than the NodeStartupTimeout", &reconcileTestCase{
				mhc:   func() *clusterv1.MachineHealthCheck { return testMHC },
				nodes: func() []*corev1.Node { return []*corev1.Node{healthyNode1, healthyNode2} },
				machines: func() []*clusterv1.Machine {
					return []*clusterv1.Machine{healthyMachine1, healthyMachine2, noNodeRefMachine2}
				},
				expectUnhealthy:     func() []*clusterv1.Machine { return []*clusterv1.Machine{noNodeRefMachine2} },
				expectRemediation:   func() []*clusterv1.Machine { return []*clusterv1.Machine{noNodeRefMachine2} },
				expectHealthy:       func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectNoRemediation: func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectedStatus:      clusterv1.MachineHealthCheckStatus{ExpectedMachines: 3, CurrentHealthy: 2},
			}),
			Entry("when a Machine's Node has gone away", &reconcileTestCase{
				mhc:   func() *clusterv1.MachineHealthCheck { return testMHC },
				nodes: func() []*corev1.Node { return []*corev1.Node{healthyNode1, healthyNode2} },
				machines: func() []*clusterv1.Machine {
					return []*clusterv1.Machine{healthyMachine1, healthyMachine2, nodeGoneMachine1}
				},
				expectUnhealthy:     func() []*clusterv1.Machine { return []*clusterv1.Machine{nodeGoneMachine1} },
				expectRemediation:   func() []*clusterv1.Machine { return []*clusterv1.Machine{nodeGoneMachine1} },
				expectHealthy:       func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectNoRemediation: func() []*clusterv1.Machine { return []*clusterv1.Machine{healthyMachine1, healthyMachine2} },
				expectedStatus:      clusterv1.MachineHealthCheckStatus{ExpectedMachines: 3, CurrentHealthy: 2},
			}),
			Entry("when no Machines are matched by the selector", &reconcileTestCase{
				mhc:                 func() *clusterv1.MachineHealthCheck { return testMHC },
				nodes:               func() []*corev1.Node { return []*corev1.Node{unlabelledNode} },
				machines:            func() []*clusterv1.Machine { return []*clusterv1.Machine{unlabelledMachine} },
				expectUnhealthy:     none,
				expectRemediation:   none,
				expectHealthy:       none,
				expectNoRemediation: none,
				expectedStatus:      clusterv1.MachineHealthCheckStatus{ExpectedMachines: 0, CurrentHealthy: 0},
			}),
		)

		Context("when a remote Node is modified", func() {
			It("should react to the updated Node", func() {
				By("Creating a Node")
				remoteNode := newTestNode("remote-node-1")
				remoteNode.Status.Conditions = []corev1.NodeCondition{healthyNodeCondition}
				Expect(testEnv.Create(ctx, remoteNode)).To(Succeed())

				By("Creating a Machine")
				// Set up the Machine to reduce events triggered by other controllers updating the Machine
				remoteMachine := newTestMachine("remote-machine-1", namespaceName, clusterName, remoteNode.Name, labels)
				now := metav1.NewTime(time.Now())
				remoteMachine.SetFinalizers([]string{"machine.cluster.x-k8s.io"})
				remoteMachine.Status.LastUpdated = &now
				remoteMachine.Status.Phase = "Provisioned"
				createMachine(remoteMachine)

				By("Creating a MachineHealthCheck")
				mhc := newTestMachineHealthCheck("remote-test-mhc", namespaceName, clusterName, labels)
				maxUnhealthy := intstr.Parse("1")
				mhc.Spec.MaxUnhealthy = &maxUnhealthy
				mhc.Default()
				Expect(testEnv.Create(ctx, mhc)).To(Succeed())

				By("Verifying the status has been updated, and the machine is currently healthy")
				Eventually(getMHCStatus(namespaceName, mhc.Name), timeout).Should(Equal(clusterv1.MachineHealthCheckStatus{ExpectedMachines: 1, CurrentHealthy: 1}))
				// Make sure the status is stable before making any changes, this allows in-flight reconciles to finish
				Consistently(getMHCStatus(namespaceName, mhc.Name), 100*time.Millisecond).Should(Equal(clusterv1.MachineHealthCheckStatus{ExpectedMachines: 1, CurrentHealthy: 1}))

				By("Updating the node to make it unhealthy")
				Eventually(func() error {
					node := &corev1.Node{}
					if err := testEnv.Get(ctx, util.ObjectKey(remoteNode), node); err != nil {
						return err
					}
					node.Status.Conditions = []corev1.NodeCondition{unhealthyNodeCondition}
					if err := testEnv.Status().Update(ctx, node); err != nil {
						return err
					}
					return nil
				}, timeout).Should(Succeed())

				By("Verifying the status has been updated, and the machine is now unhealthy")
				Eventually(getMHCStatus(namespaceName, mhc.Name), timeout).Should(Equal(clusterv1.MachineHealthCheckStatus{ExpectedMachines: 1, CurrentHealthy: 0}))
			})
		})
	})
})

func cleanupTestMachineHealthChecks(ctx context.Context, c client.Client) error {
	mhcList := &clusterv1.MachineHealthCheckList{}
	if err := c.List(ctx, mhcList); err != nil {
		return err
	}
	for _, mhc := range mhcList.Items {
		m := mhc
		if err := c.Delete(ctx, &m); err != nil {
			return err
		}
	}
	return nil
}

func cleanupTestMachines(ctx context.Context, c client.Client) error {
	machineList := &clusterv1.MachineList{}
	if err := c.List(ctx, machineList); err != nil {
		return err
	}
	for _, machine := range machineList.Items {
		m := machine
		if err := c.Delete(ctx, &m); err != nil && apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return err
		}
		Eventually(func() error {
			if err := c.Get(ctx, util.ObjectKey(&m), &m); err != nil && apierrors.IsNotFound(err) {
				return nil
			} else if err != nil {
				return err
			}
			m.SetFinalizers([]string{})
			return c.Update(ctx, &m)
		}, timeout).Should(Succeed())
	}
	return nil
}

func cleanupTestNodes(ctx context.Context, c client.Client) error {
	nodeList := &corev1.NodeList{}
	if err := c.List(ctx, nodeList); err != nil {
		return err
	}
	for _, node := range nodeList.Items {
		n := node
		if err := c.Delete(ctx, &n); err != nil {
			return err
		}
	}
	return nil
}

func ownerReferenceForCluster(ctx context.Context, c *clusterv1.Cluster) metav1.OwnerReference {
	// Fetch the cluster to populate the UID
	cc := &clusterv1.Cluster{}
	Expect(testEnv.GetClient().Get(ctx, util.ObjectKey(c), cc)).To(Succeed())

	return metav1.OwnerReference{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Cluster",
		Name:       cc.Name,
		UID:        cc.UID,
	}
}

var _ = Describe("MachineHealthCheck", func() {
	Context("on reconciliation", func() {
		var (
			cluster *clusterv1.Cluster
			mhc     *clusterv1.MachineHealthCheck
		)

		BeforeEach(func() {
			cluster = &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-cluster-",
					Namespace:    "default",
				},
			}

			mhc = &clusterv1.MachineHealthCheck{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-mhc-",
					Namespace:    "default",
				},
				Spec: clusterv1.MachineHealthCheckSpec{
					UnhealthyConditions: []clusterv1.UnhealthyCondition{
						{
							Type:    corev1.NodeReady,
							Status:  corev1.ConditionUnknown,
							Timeout: metav1.Duration{Duration: 5 * time.Minute},
						},
					},
				},
			}
			mhc.Default()

			Expect(testEnv.Create(ctx, cluster)).To(Succeed())
			Expect(testEnv.CreateKubeconfigSecret(cluster)).To(Succeed())
		})

		AfterEach(func() {
			Expect(testEnv.Delete(ctx, mhc)).To(Succeed())
			Expect(testEnv.Delete(ctx, cluster)).To(Succeed())
		})

		Context("it should ensure the correct cluster-name label", func() {
			Specify("with no existing labels exist", func() {
				mhc.Spec.ClusterName = cluster.Name
				mhc.Labels = map[string]string{}
				Expect(testEnv.Create(ctx, mhc)).To(Succeed())

				Eventually(func() map[string]string {
					err := testEnv.Get(ctx, util.ObjectKey(mhc), mhc)
					if err != nil {
						return nil
					}
					return mhc.GetLabels()
				}, timeout).Should(HaveKeyWithValue(clusterv1.ClusterLabelName, cluster.Name))
			})

			Specify("when the label has the wrong value", func() {
				mhc.Spec.ClusterName = cluster.Name
				mhc.Labels = map[string]string{
					clusterv1.ClusterLabelName: "wrong-cluster",
				}
				Expect(testEnv.Create(ctx, mhc)).To(Succeed())

				Eventually(func() map[string]string {
					err := testEnv.Get(ctx, util.ObjectKey(mhc), mhc)
					if err != nil {
						return nil
					}
					return mhc.GetLabels()
				}, timeout).Should(HaveKeyWithValue(clusterv1.ClusterLabelName, cluster.Name))
			})

			Specify("when other labels are present", func() {
				mhc.Spec.ClusterName = cluster.Name
				mhc.Labels = map[string]string{
					"extra-label": "1",
				}
				Expect(testEnv.Create(ctx, mhc)).To(Succeed())

				Eventually(func() map[string]string {
					err := testEnv.Get(ctx, util.ObjectKey(mhc), mhc)
					if err != nil {
						return nil
					}
					return mhc.GetLabels()

				}, timeout).Should(And(
					HaveKeyWithValue(clusterv1.ClusterLabelName, cluster.Name),
					HaveKeyWithValue("extra-label", "1"),
					HaveLen(2),
				))
			})
		})

		Context("it should ensure an owner reference is present", func() {
			Specify("when no existing ones exist", func() {
				mhc.Spec.ClusterName = cluster.Name
				mhc.OwnerReferences = nil
				Expect(testEnv.Create(ctx, mhc)).To(Succeed())

				Eventually(func() []metav1.OwnerReference {
					err := testEnv.Get(ctx, util.ObjectKey(mhc), mhc)
					if err != nil {
						return nil
					}
					return mhc.GetOwnerReferences()
				}, timeout).Should(And(
					ContainElement(ownerReferenceForCluster(ctx, cluster)),
					HaveLen(1),
				))
			})

			Specify("when modifying existing ones", func() {
				mhc.Spec.ClusterName = cluster.Name
				mhc.OwnerReferences = []metav1.OwnerReference{
					{Kind: "Foo", APIVersion: "foo.bar.baz/v1", Name: "Bar", UID: "12345"},
				}
				Expect(testEnv.Create(ctx, mhc)).To(Succeed())

				Eventually(func() []metav1.OwnerReference {
					err := testEnv.Get(ctx, util.ObjectKey(mhc), mhc)
					if err != nil {
						return nil
					}
					return mhc.GetOwnerReferences()
				}, timeout).Should(And(
					ContainElements(
						metav1.OwnerReference{Kind: "Foo", APIVersion: "foo.bar.baz/v1", Name: "Bar", UID: "12345"},
						ownerReferenceForCluster(ctx, cluster)),
					HaveLen(2),
				))
			})
		})
	})
})

func TestClusterToMachineHealthCheck(t *testing.T) {
	_ = clusterv1.AddToScheme(scheme.Scheme)
	fakeClient := fake.NewFakeClient()

	r := &MachineHealthCheckReconciler{
		Log:    log.Log,
		Client: fakeClient,
	}

	namespace := defaultNamespaceName
	clusterName := "test-cluster"
	labels := make(map[string]string)

	mhc1 := newTestMachineHealthCheck("mhc1", namespace, clusterName, labels)
	mhc1Req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: mhc1.Namespace, Name: mhc1.Name}}
	mhc2 := newTestMachineHealthCheck("mhc2", namespace, clusterName, labels)
	mhc2Req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: mhc2.Namespace, Name: mhc2.Name}}
	mhc3 := newTestMachineHealthCheck("mhc3", namespace, "othercluster", labels)
	mhc4 := newTestMachineHealthCheck("mhc4", "othernamespace", clusterName, labels)
	cluster1 := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
		},
	}

	testCases := []struct {
		name     string
		toCreate []clusterv1.MachineHealthCheck
		object   handler.MapObject
		expected []reconcile.Request
	}{
		{
			name:     "when the object passed isn't a cluster",
			toCreate: []clusterv1.MachineHealthCheck{*mhc1},
			object: handler.MapObject{
				Object: &clusterv1.Machine{},
			},
			expected: []reconcile.Request{},
		},
		{
			name:     "when a MachineHealthCheck exists for the Cluster in the same namespace",
			toCreate: []clusterv1.MachineHealthCheck{*mhc1},
			object: handler.MapObject{
				Object: cluster1,
			},
			expected: []reconcile.Request{mhc1Req},
		},
		{
			name:     "when 2 MachineHealthChecks exists for the Cluster in the same namespace",
			toCreate: []clusterv1.MachineHealthCheck{*mhc1, *mhc2},
			object: handler.MapObject{
				Object: cluster1,
			},
			expected: []reconcile.Request{mhc1Req, mhc2Req},
		},
		{
			name:     "when a MachineHealthCheck exists for another Cluster in the same namespace",
			toCreate: []clusterv1.MachineHealthCheck{*mhc3},
			object: handler.MapObject{
				Object: cluster1,
			},
			expected: []reconcile.Request{},
		},
		{
			name:     "when a MachineHealthCheck exists for another Cluster in another namespace",
			toCreate: []clusterv1.MachineHealthCheck{*mhc4},
			object: handler.MapObject{
				Object: cluster1,
			},
			expected: []reconcile.Request{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gs := NewWithT(t)

			ctx := context.Background()
			for _, obj := range tc.toCreate {
				o := obj
				gs.Expect(r.Client.Create(ctx, &o)).To(Succeed())
				defer func() {
					gs.Expect(r.Client.Delete(ctx, &o)).To(Succeed())
				}()
				// Check the cache is populated
				getObj := func() error {
					return r.Client.Get(ctx, util.ObjectKey(&o), &clusterv1.MachineHealthCheck{})
				}
				gs.Eventually(getObj, timeout).Should(Succeed())
			}

			got := r.clusterToMachineHealthCheck(tc.object)
			gs.Expect(got).To(ConsistOf(tc.expected))
		})
	}
}

func newTestMachineHealthCheck(name, namespace, cluster string, labels map[string]string) *clusterv1.MachineHealthCheck {
	l := make(map[string]string, len(labels))
	for k, v := range labels {
		l[k] = v
	}
	l[clusterv1.ClusterLabelName] = cluster

	return &clusterv1.MachineHealthCheck{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    l,
		},
		Spec: clusterv1.MachineHealthCheckSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: l,
			},
			ClusterName: cluster,
			UnhealthyConditions: []clusterv1.UnhealthyCondition{
				{
					Type:    corev1.NodeReady,
					Status:  corev1.ConditionUnknown,
					Timeout: metav1.Duration{Duration: 5 * time.Minute},
				},
			},
		},
	}
}

func TestMachineToMachineHealthCheck(t *testing.T) {
	_ = clusterv1.AddToScheme(scheme.Scheme)
	fakeClient := fake.NewFakeClient()

	r := &MachineHealthCheckReconciler{
		Log:    log.Log,
		Client: fakeClient,
	}

	namespace := defaultNamespaceName
	clusterName := "test-cluster"
	nodeName := "node1"
	labels := map[string]string{"cluster": "foo", "nodepool": "bar"}

	mhc1 := newTestMachineHealthCheck("mhc1", namespace, clusterName, labels)
	mhc1Req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: mhc1.Namespace, Name: mhc1.Name}}
	mhc2 := newTestMachineHealthCheck("mhc2", namespace, clusterName, labels)
	mhc2Req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: mhc2.Namespace, Name: mhc2.Name}}
	mhc3 := newTestMachineHealthCheck("mhc3", namespace, clusterName, map[string]string{"cluster": "foo", "nodepool": "other"})
	mhc4 := newTestMachineHealthCheck("mhc4", "othernamespace", clusterName, labels)
	machine1 := newTestMachine("machine1", namespace, clusterName, nodeName, labels)

	testCases := []struct {
		name     string
		toCreate []clusterv1.MachineHealthCheck
		object   handler.MapObject
		expected []reconcile.Request
	}{
		{
			name:     "when the object passed isn't a machine",
			toCreate: []clusterv1.MachineHealthCheck{*mhc1},
			object: handler.MapObject{
				Object: &clusterv1.Cluster{},
			},
			expected: []reconcile.Request{},
		},
		{
			name:     "when a MachineHealthCheck matches labels for the Machine in the same namespace",
			toCreate: []clusterv1.MachineHealthCheck{*mhc1},
			object: handler.MapObject{
				Object: machine1,
			},
			expected: []reconcile.Request{mhc1Req},
		},
		{
			name:     "when 2 MachineHealthChecks match labels for the Machine in the same namespace",
			toCreate: []clusterv1.MachineHealthCheck{*mhc1, *mhc2},
			object: handler.MapObject{
				Object: machine1,
			},
			expected: []reconcile.Request{mhc1Req, mhc2Req},
		},
		{
			name:     "when a MachineHealthCheck does not match labels for the Machine in the same namespace",
			toCreate: []clusterv1.MachineHealthCheck{*mhc3},
			object: handler.MapObject{
				Object: machine1,
			},
			expected: []reconcile.Request{},
		},
		{
			name:     "when a MachineHealthCheck matches labels for the Machine in another namespace",
			toCreate: []clusterv1.MachineHealthCheck{*mhc4},
			object: handler.MapObject{
				Object: machine1,
			},
			expected: []reconcile.Request{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gs := NewWithT(t)

			ctx := context.Background()
			for _, obj := range tc.toCreate {
				o := obj
				gs.Expect(r.Client.Create(ctx, &o)).To(Succeed())
				defer func() {
					gs.Expect(r.Client.Delete(ctx, &o)).To(Succeed())
				}()
				// Check the cache is populated
				getObj := func() error {
					return r.Client.Get(ctx, util.ObjectKey(&o), &clusterv1.MachineHealthCheck{})
				}
				gs.Eventually(getObj, timeout).Should(Succeed())
			}

			got := r.machineToMachineHealthCheck(tc.object)
			gs.Expect(got).To(ConsistOf(tc.expected))
		})
	}
}

func TestNodeToMachineHealthCheck(t *testing.T) {
	_ = clusterv1.AddToScheme(scheme.Scheme)
	fakeClient := fake.NewFakeClient()

	r := &MachineHealthCheckReconciler{
		Log:    log.Log,
		Client: fakeClient,
	}

	namespace := defaultNamespaceName
	clusterName := "test-cluster"
	nodeName := "node1"
	labels := map[string]string{"cluster": "foo", "nodepool": "bar"}

	mhc1 := newTestMachineHealthCheck("mhc1", namespace, clusterName, labels)
	mhc1Req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: mhc1.Namespace, Name: mhc1.Name}}
	mhc2 := newTestMachineHealthCheck("mhc2", namespace, clusterName, labels)
	mhc2Req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: mhc2.Namespace, Name: mhc2.Name}}
	mhc3 := newTestMachineHealthCheck("mhc3", namespace, "othercluster", labels)
	mhc4 := newTestMachineHealthCheck("mhc4", "othernamespace", clusterName, labels)

	machine1 := newTestMachine("machine1", namespace, clusterName, nodeName, labels)
	machine2 := newTestMachine("machine2", namespace, clusterName, nodeName, labels)

	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
	}

	testCases := []struct {
		name        string
		mhcToCreate []clusterv1.MachineHealthCheck
		mToCreate   []clusterv1.Machine
		object      handler.MapObject
		expected    []reconcile.Request
	}{
		{
			name:        "when the object passed isn't a Node",
			mhcToCreate: []clusterv1.MachineHealthCheck{*mhc1},
			mToCreate:   []clusterv1.Machine{*machine1},
			object: handler.MapObject{
				Object: &clusterv1.Machine{},
			},
			expected: []reconcile.Request{},
		},
		{
			name:        "when no Machine exists for the Node",
			mhcToCreate: []clusterv1.MachineHealthCheck{*mhc1},
			mToCreate:   []clusterv1.Machine{},
			object: handler.MapObject{
				Object: node1,
			},
			expected: []reconcile.Request{},
		},
		{
			name:        "when two Machines exist for the Node",
			mhcToCreate: []clusterv1.MachineHealthCheck{*mhc1},
			mToCreate:   []clusterv1.Machine{*machine1, *machine2},
			object: handler.MapObject{
				Object: node1,
			},
			expected: []reconcile.Request{},
		},
		{
			name:        "when no MachineHealthCheck exists for the Node in the Machine's namespace",
			mhcToCreate: []clusterv1.MachineHealthCheck{*mhc4},
			mToCreate:   []clusterv1.Machine{*machine1},
			object: handler.MapObject{
				Object: node1,
			},
			expected: []reconcile.Request{},
		},
		{
			name:        "when a MachineHealthCheck exists for the Node in the Machine's namespace",
			mhcToCreate: []clusterv1.MachineHealthCheck{*mhc1},
			mToCreate:   []clusterv1.Machine{*machine1},
			object: handler.MapObject{
				Object: node1,
			},
			expected: []reconcile.Request{mhc1Req},
		},
		{
			name:        "when two MachineHealthChecks exist for the Node in the Machine's namespace",
			mhcToCreate: []clusterv1.MachineHealthCheck{*mhc1, *mhc2},
			mToCreate:   []clusterv1.Machine{*machine1},
			object: handler.MapObject{
				Object: node1,
			},
			expected: []reconcile.Request{mhc1Req, mhc2Req},
		},
		{
			name:        "when a MachineHealthCheck exists for the Node, but not in the Machine's cluster",
			mhcToCreate: []clusterv1.MachineHealthCheck{*mhc3},
			mToCreate:   []clusterv1.Machine{*machine1},
			object: handler.MapObject{
				Object: node1,
			},
			expected: []reconcile.Request{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gs := NewWithT(t)

			ctx := context.Background()
			for _, obj := range tc.mhcToCreate {
				o := obj
				gs.Expect(r.Client.Create(ctx, &o)).To(Succeed())
				defer func() {
					gs.Expect(r.Client.Delete(ctx, &o)).To(Succeed())
				}()
				// Check the cache is populated
				key := util.ObjectKey(&o)
				getObj := func() error {
					return r.Client.Get(ctx, key, &clusterv1.MachineHealthCheck{})
				}
				gs.Eventually(getObj, timeout).Should(Succeed())
			}
			for _, obj := range tc.mToCreate {
				o := obj
				gs.Expect(r.Client.Create(ctx, &o)).To(Succeed())
				defer func() {
					gs.Expect(r.Client.Delete(ctx, &o)).To(Succeed())
				}()
				// Ensure the status is set (required for matching node to machine)
				o.Status = obj.Status
				gs.Expect(r.Client.Status().Update(ctx, &o)).To(Succeed())

				// Check the cache is up to date with the status update
				key := util.ObjectKey(&o)
				checkStatus := func() clusterv1.MachineStatus {
					m := &clusterv1.Machine{}
					err := r.Client.Get(ctx, key, m)
					if err != nil {
						return clusterv1.MachineStatus{}
					}
					return m.Status
				}
				gs.Eventually(checkStatus, timeout).Should(Equal(o.Status))
			}

			got := r.nodeToMachineHealthCheck(tc.object)
			gs.Expect(got).To(ConsistOf(tc.expected))
		})
	}
}

func TestIndexMachineByNodeName(t *testing.T) {
	r := &MachineHealthCheckReconciler{
		Log: log.Log,
	}

	testCases := []struct {
		name     string
		object   runtime.Object
		expected []string
	}{
		{
			name:     "when the machine has no NodeRef",
			object:   &clusterv1.Machine{},
			expected: []string{},
		},
		{
			name: "when the machine has valid a NodeRef",
			object: &clusterv1.Machine{
				Status: clusterv1.MachineStatus{
					NodeRef: &corev1.ObjectReference{
						Name: "node1",
					},
				},
			},
			expected: []string{"node1"},
		},
		{
			name:     "when the object passed is not a Machine",
			object:   &corev1.Node{},
			expected: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			got := r.indexMachineByNodeName(tc.object)
			g.Expect(got).To(ConsistOf(tc.expected))
		})
	}
}

func TestIsAllowedRedmediation(t *testing.T) {
	testCases := []struct {
		name             string
		maxUnhealthy     *intstr.IntOrString
		expectedMachines int32
		currentHealthy   int32
		allowed          bool
	}{
		{
			name:             "when maxUnhealthy is not set",
			maxUnhealthy:     nil,
			expectedMachines: int32(3),
			currentHealthy:   int32(0),
			allowed:          true,
		},
		{
			name:             "when maxUnhealthy is not an int or percentage",
			maxUnhealthy:     &intstr.IntOrString{Type: intstr.String, StrVal: "abcdef"},
			expectedMachines: int32(5),
			currentHealthy:   int32(2),
			allowed:          false,
		},
		{
			name:             "when maxUnhealthy is an int less than current unhealthy",
			maxUnhealthy:     &intstr.IntOrString{Type: intstr.Int, IntVal: int32(1)},
			expectedMachines: int32(3),
			currentHealthy:   int32(1),
			allowed:          false,
		},
		{
			name:             "when maxUnhealthy is an int equal to current unhealthy",
			maxUnhealthy:     &intstr.IntOrString{Type: intstr.Int, IntVal: int32(2)},
			expectedMachines: int32(3),
			currentHealthy:   int32(1),
			allowed:          true,
		},
		{
			name:             "when maxUnhealthy is an int greater than current unhealthy",
			maxUnhealthy:     &intstr.IntOrString{Type: intstr.Int, IntVal: int32(3)},
			expectedMachines: int32(3),
			currentHealthy:   int32(1),
			allowed:          true,
		},
		{
			name:             "when maxUnhealthy is a percentage less than current unhealthy",
			maxUnhealthy:     &intstr.IntOrString{Type: intstr.String, StrVal: "50%"},
			expectedMachines: int32(5),
			currentHealthy:   int32(2),
			allowed:          false,
		},
		{
			name:             "when maxUnhealthy is a percentage equal to current unhealthy",
			maxUnhealthy:     &intstr.IntOrString{Type: intstr.String, StrVal: "60%"},
			expectedMachines: int32(5),
			currentHealthy:   int32(2),
			allowed:          true,
		},
		{
			name:             "when maxUnhealthy is a percentage greater than current unhealthy",
			maxUnhealthy:     &intstr.IntOrString{Type: intstr.String, StrVal: "70%"},
			expectedMachines: int32(5),
			currentHealthy:   int32(2),
			allowed:          true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			mhc := &clusterv1.MachineHealthCheck{
				Spec: clusterv1.MachineHealthCheckSpec{
					MaxUnhealthy: tc.maxUnhealthy,
				},
				Status: clusterv1.MachineHealthCheckStatus{
					ExpectedMachines: tc.expectedMachines,
					CurrentHealthy:   tc.currentHealthy,
				},
			}

			g.Expect(isAllowedRemediation(mhc)).To(Equal(tc.allowed))
		})
	}
}

func none() []*clusterv1.Machine {
	return []*clusterv1.Machine{}
}

var _ = Describe("MachineSet remediation", func() {
	It("deletes machines marked with the MHC condition", func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mhc-test"}}
		Expect(testEnv.Create(context.Background(), namespace)).To(Succeed())
		defer cleanup(testEnv, namespace)

		cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, Name: "test-cluster"}}
		Expect(testEnv.Create(context.Background(), cluster)).To(Succeed())

		Expect(testEnv.CreateKubeconfigSecret(cluster)).To(Succeed())

		bootstrapTmpl := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"kind":       "BootstrapMachineTemplate",
				"apiVersion": "bootstrap.cluster.x-k8s.io/v1alpha3",
				"metadata": map[string]interface{}{
					"name":      "ms-template",
					"namespace": namespace.Name,
				},
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"kind":       "BootstrapMachine",
						"apiVersion": "bootstrap.cluster.x-k8s.io/v1alpha3",
						"metadata":   map[string]interface{}{},
					},
				},
			},
		}
		Expect(testEnv.Create(context.Background(), bootstrapTmpl)).To(Succeed())

		infraTmpl := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"kind":       "InfrastructureMachineTemplate",
				"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha3",
				"metadata": map[string]interface{}{
					"name":      "ms-template",
					"namespace": namespace.Name,
				},
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"kind":       "InfrastructureMachine",
						"apiVersion": "infrastructure.cluster.x-k8s.io/v1alpha3",
						"metadata":   map[string]interface{}{},
						"spec": map[string]interface{}{
							"size":       "3xlarge",
							"providerID": "test:////id",
						},
					},
				},
			},
		}
		Expect(testEnv.Create(context.Background(), infraTmpl)).To(Succeed())

		instance := &clusterv1.MachineSet{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "ms-",
				Namespace:    namespace.Name,
			},
			Spec: clusterv1.MachineSetSpec{
				ClusterName: cluster.Name,
				Replicas:    pointer.Int32Ptr(1),
				Selector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"cool": "true",
					},
				},
				Template: clusterv1.MachineTemplateSpec{
					ObjectMeta: clusterv1.ObjectMeta{
						Labels: map[string]string{
							"cool": "true",
						},
					},
					Spec: clusterv1.MachineSpec{
						ClusterName: cluster.Name,
						Version:     pointer.StringPtr("1.0.0"),
						Bootstrap: clusterv1.Bootstrap{
							ConfigRef: &corev1.ObjectReference{
								APIVersion: "bootstrap.cluster.x-k8s.io/v1alpha3",
								Kind:       "BootstrapMachineTemplate",
								Name:       bootstrapTmpl.GetName(),
							},
						},
						InfrastructureRef: corev1.ObjectReference{
							APIVersion: "infrastructure.cluster.x-k8s.io/v1alpha3",
							Kind:       "InfrastructureMachineTemplate",
							Name:       infraTmpl.GetName(),
						},
					},
				},
			},
		}
		Expect(testEnv.Create(context.Background(), instance)).To(Succeed())

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-1",
			},
		}
		Expect(testEnv.Create(context.Background(), node)).To(Succeed())
		defer cleanup(testEnv, node)

		mhc := &clusterv1.MachineHealthCheck{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mhc",
				Namespace: namespace.Name,
			},
			Spec: clusterv1.MachineHealthCheckSpec{
				ClusterName: cluster.Name,
				Selector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"cool": "true",
					},
				},
				UnhealthyConditions: []clusterv1.UnhealthyCondition{
					{
						Type:    corev1.NodeReady,
						Status:  corev1.ConditionUnknown,
						Timeout: metav1.Duration{Duration: 5 * time.Minute},
					},
				},
			},
		}
		mhc.Default()
		Expect(testEnv.Create(context.Background(), mhc)).To(Succeed())

		var nodes corev1.NodeList
		Expect(testEnv.List(context.Background(), &nodes)).To(Succeed())

		var found bool
		for i := range nodes.Items {
			n := nodes.Items[i]
			if n.Name == node.Name {
				found = true
				node = &n
			}
		}
		Expect(found).To(BeTrue())

		var machines clusterv1.MachineList
		Eventually(func() ([]clusterv1.Machine, error) {
			if err := testEnv.List(context.Background(), &machines, client.InNamespace(namespace.Name)); err != nil {
				return nil, err
			}

			return machines.Items, nil
		}, 10*time.Second).Should(HaveLen(1))

		machine := &machines.Items[0]
		machine.Status.NodeRef = &corev1.ObjectReference{
			APIVersion: node.APIVersion,
			Kind:       node.Kind,
			Name:       node.Name,
			UID:        node.UID,
		}
		failureReason := capierrors.MachineStatusError("foo")
		machine.Status.FailureReason = &failureReason
		Expect(testEnv.Status().Update(context.Background(), machine)).To(Succeed())

		// add a finalizer so we can observe the deletion
		patchHelper, err := patch.NewHelper(machine, testEnv)
		Expect(err).To(BeNil())
		machine.ObjectMeta.Finalizers = []string{"whatever"}
		Expect(patchHelper.Patch(context.Background(), machine)).To(Succeed())
		defer func() {
			// release finalizer
			patch := client.MergeFrom(machine.DeepCopy())
			machine.SetFinalizers(nil)
			Expect(testEnv.Patch(context.Background(), machine, patch)).To(Succeed())
		}()

		Eventually(func() (*metav1.Time, error) {
			if err := testEnv.Get(context.Background(), util.ObjectKey(machine), machine); err != nil {
				return nil, err
			}

			return machine.DeletionTimestamp, nil
		}, 20*time.Second).ShouldNot(BeNil())
	})
})

func cleanup(client client.Client, obj runtime.Object) {
	Expect(client.Delete(context.Background(), obj)).To(Succeed())
}
