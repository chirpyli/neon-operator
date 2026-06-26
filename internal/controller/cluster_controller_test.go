/*
Copyright 2025.

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

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	neonv1alpha1 "oltp.molnett.org/neon-operator/api/v1alpha1"
	safekeeperspec "oltp.molnett.org/neon-operator/specs/safekeeper"
	"oltp.molnett.org/neon-operator/test/fixtures"
)

var _ = Describe("Cluster Controller", func() {
	const clusterName = "cluster-suite"
	var namespace string

	BeforeEach(func() {
		namespace = newTestNamespace()
		Expect(k8sClient.Create(ctx, fixtures.NewBucketCredsSecret(clusterName, namespace))).To(Succeed())
		Expect(k8sClient.Create(ctx, fixtures.NewStorcondDBSecret(clusterName, namespace))).To(Succeed())
		Expect(k8sClient.Create(ctx, fixtures.NewCluster(clusterName, namespace))).To(Succeed())
	})

	AfterEach(func() {
		cluster := &neonv1alpha1.Cluster{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, cluster); err == nil {
			Expect(k8sClient.Delete(ctx, cluster)).To(Succeed())
		}
	})

	It("reconciles a Cluster into JWT secret + storage-controller + storage-broker", func() {
		Eventually(func(g Gomega) {
			jwt := &corev1.Secret{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "cluster-" + clusterName + "-jwt",
				Namespace: namespace,
			}, jwt)).To(Succeed())
			g.Expect(jwt.Data).To(HaveKey("private.pem"))
			g.Expect(jwt.Data).To(HaveKey("public.pem"))

			storcon := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      clusterName + "-storage-controller",
				Namespace: namespace,
			}, storcon)).To(Succeed())

			broker := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      clusterName + "-storage-broker",
				Namespace: namespace,
			}, broker)).To(Succeed())
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("auto-creates Safekeeper CRs according to NumSafekeepers", func() {
		// The fixture creates a Cluster with NumSafekeepers=3.
		// Expect IDs 1, 2, 3 to be created.
		Eventually(func(g Gomega) {
			var list neonv1alpha1.SafekeeperList
			g.Expect(k8sClient.List(ctx, &list,
				client.InNamespace(namespace),
				client.MatchingLabels{
					safekeeperspec.ClusterLabel: clusterName,
				},
			)).To(Succeed())
			g.Expect(list.Items).To(HaveLen(3))
			ids := make(map[uint32]bool)
			for _, sk := range list.Items {
				ids[sk.Spec.ID] = true
				g.Expect(sk.Spec.Cluster).To(Equal(clusterName))
			}
			g.Expect(ids).To(HaveKey(uint32(1)))
			g.Expect(ids).To(HaveKey(uint32(2)))
			g.Expect(ids).To(HaveKey(uint32(3)))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
	})
})
