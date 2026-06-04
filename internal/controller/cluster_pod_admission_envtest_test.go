package controller_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// Envtest scenarios for the genclust Job pod-admission visibility feature.
//
// Scope: Layer 3 (Pod watch + translator) end-to-end on a real apiserver.
//   - Layer 1 (dry-run pre-flight) requires SCC/PodSecurity admission which
//     envtest's apiserver does not run — covered by unit tests in
//     permanent_error_test.go (TestHandleAdmissionForbidden_*) and the Kind
//     smoke kit at demo/genclust-pod-admission-smoke/.
//   - Layer 2 (scoped Event informer + cached FailedCreate read) is covered
//     by unit tests in cluster_admission_wiring_test.go
//     (TestFindClusterForJobFailedCreateEvent, TestLatestJobFailedCreateMessage,
//     TestJobFailedCreateEventPredicate). End-to-end exercise of the cached
//     Event read requires the manager's cache scope wiring (cmd/manager/main.go)
//     which envtest doesn't construct — the Kind smoke kit covers it.

var _ = Describe("IdentityServerCluster pod-admission visibility", func() {
	const timeout = 30 * time.Second
	const interval = 250 * time.Millisecond

	var (
		ns          string
		clusterName string
	)

	BeforeEach(func() {
		ns = nodeTestNamespace()
		clusterName = "pa-cluster"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		})).To(Succeed())

		cluster := &v1alpha1.IdentityServerCluster{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: ns},
			Spec:       v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

		admin := &v1alpha1.IdentityServerNode{
			ObjectMeta: metav1.ObjectMeta{Name: "admin", Namespace: ns},
			Spec: v1alpha1.IdentityServerNodeSpec{
				Type:                     v1alpha1.NodeTypeAdmin,
				Role:                     "admin-role",
				IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
				Service:                  v1alpha1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Port: 6789},
			},
		}
		Expect(k8sClient.Create(ctx, admin)).To(Succeed())

		// Wait for the operator to create the genclust Job (envtest's Pod
		// will never start since there's no kubelet, but the Job object
		// exists, which is enough for Layer 3 to be reachable).
		Eventually(func(g Gomega) {
			var job batchv1.Job
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: clusterName + "-cluster-config-job", Namespace: ns,
			}, &job)).To(Succeed())
		}, timeout, interval).Should(Succeed())
	})

	It("sets JobPodSchedulingFailed when a Pod is Pending/Unschedulable", func() {
		// Create the cluster-config-labeled Pod, then patch its Status with
		// PodScheduled=False/Unschedulable to simulate scheduler denial.
		pod := makeClusterConfigPod(ns, clusterName, "p-schedfail")
		pod.Status.Phase = corev1.PodPending
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())

		pod.Status.Conditions = []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  corev1.PodReasonUnschedulable,
			Message: "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector",
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		Eventually(func(g Gomega) {
			var fresh v1alpha1.IdentityServerCluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: ns}, &fresh)).To(Succeed())
			cond := apimeta.FindStatusCondition(fresh.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("JobPodSchedulingFailed"))
			g.Expect(cond.Message).To(ContainSubstring("didn't match"))
		}, timeout, interval).Should(Succeed())
	})
})

// makeClusterConfigPod builds a Pod with the labels the operator's translator
// keys on. The genclust container Image is intentionally set to a runnable
// reference so envtest stores the Pod cleanly (envtest doesn't actually pull
// or start anything).
func makeClusterConfigPod(ns, clusterName, podName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels: map[string]string{
				"curity.io/cluster":   clusterName,
				"curity.io/component": "cluster-config",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "genclust",
				Image: "ghcr.io/curityio/curity-server:11.0",
			}},
		},
	}
}

func ptrTo[T any](v T) *T {
	return &v
}
