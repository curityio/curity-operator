package controller_test

import (
	"fmt"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

var dbNSCounter atomic.Int64

func dbTestNamespace() string {
	ns := fmt.Sprintf("isdb-test-%d", dbNSCounter.Add(1))
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	return ns
}

func inlineURLSource(v string) *v1alpha1.ValueSource { return &v1alpha1.ValueSource{Value: v} }

func createDatabase(ns, name, clusterName string, conn v1alpha1.JDBCConnection, tmpl *v1alpha1.DatabaseJobTemplate) *v1alpha1.IdentityServerDatabase {
	db := &v1alpha1.IdentityServerDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.IdentityServerDatabaseSpec{
			IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
			Connection:               conn,
			JobTemplate:              tmpl,
		},
	}
	Expect(k8sClient.Create(ctx, db)).To(Succeed())
	return db
}

func getDatabaseJob(ns, dbName string) *batchv1.Job {
	var job batchv1.Job
	err := k8sClient.Get(ctx, types.NamespacedName{Name: dbName + "-db-init-job", Namespace: ns}, &job)
	if err != nil {
		return nil
	}
	return &job
}

func markJobComplete(job *batchv1.Job) {
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Succeeded = 1
	job.Status.CompletionTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "Completed", LastTransitionTime: now},
	}
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

var _ = Describe("IdentityServerDatabase", func() {
	Context("Job lifecycle", func() {
		It("creates a database-init Job running idsvr -I with the cluster image", func() {
			ns := dbTestNamespace()
			testCreateCluster(ns, "demo")
			createDatabase(ns, "acct", "demo",
				v1alpha1.JDBCConnection{URL: inlineURLSource("jdbc:postgresql://db/acct")}, nil)

			Eventually(func(g Gomega) {
				job := getDatabaseJob(ns, "acct")
				g.Expect(job).NotTo(BeNil())
				c := job.Spec.Template.Spec.Containers[0]
				g.Expect(c.Command).To(Equal([]string{"/opt/idsvr/bin/idsvr", "-I"}))
				g.Expect(c.Image).To(Equal("curity.azurecr.io/curity/idsvr:11.0"))
				g.Expect(job.Annotations).To(HaveKey("curity.io/database-job-hash"))
				// Owned by the IdentityServerDatabase.
				g.Expect(job.OwnerReferences).To(HaveLen(1))
				g.Expect(job.OwnerReferences[0].Kind).To(Equal("IdentityServerDatabase"))
				// JDBC_URL projected.
				var foundURL bool
				for _, e := range c.Env {
					if e.Name == "JDBC_URL" && e.Value == "jdbc:postgresql://db/acct" {
						foundURL = true
					}
				}
				g.Expect(foundURL).To(BeTrue(), "JDBC_URL env should be set")
			}).Should(Succeed())

			Eventually(func(g Gomega) {
				var db v1alpha1.IdentityServerDatabase
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "acct", Namespace: ns}, &db)).To(Succeed())
				g.Expect(db.Status.JobName).To(Equal("acct-db-init-job"))
				g.Expect(db.Status.ObservedImage).To(Equal("curity.azurecr.io/curity/idsvr:11.0"))
				g.Expect(hasCondition(db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)).To(BeTrue())
			}).Should(Succeed())
		})

		It("re-triggers the Job when the cluster image override changes", func() {
			ns := dbTestNamespace()
			testCreateCluster(ns, "demo")
			createDatabase(ns, "acct", "demo",
				v1alpha1.JDBCConnection{URL: inlineURLSource("jdbc:x")}, nil)

			var oldHash string
			Eventually(func(g Gomega) {
				job := getDatabaseJob(ns, "acct")
				g.Expect(job).NotTo(BeNil())
				oldHash = job.Annotations["curity.io/database-job-hash"]
				g.Expect(oldHash).NotTo(BeEmpty())
			}).Should(Succeed())

			// Bump the cluster image override — this changes the resolved image.
			var cluster v1alpha1.IdentityServerCluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "demo", Namespace: ns}, &cluster)).To(Succeed())
			cluster.Spec.Image = "registry.example.com/curity:11.0-patched"
			Expect(k8sClient.Update(ctx, &cluster)).To(Succeed())

			Eventually(func(g Gomega) {
				job := getDatabaseJob(ns, "acct")
				g.Expect(job).NotTo(BeNil())
				g.Expect(job.Annotations["curity.io/database-job-hash"]).NotTo(Equal(oldHash))
				g.Expect(job.Spec.Template.Spec.Containers[0].Image).To(Equal("registry.example.com/curity:11.0-patched"))
			}).Should(Succeed())
		})

		It("reports Complete/Ready once the Job succeeds", func() {
			ns := dbTestNamespace()
			testCreateCluster(ns, "demo")
			createDatabase(ns, "acct", "demo",
				v1alpha1.JDBCConnection{URL: inlineURLSource("jdbc:x")}, nil)

			Eventually(func() *batchv1.Job { return getDatabaseJob(ns, "acct") }).ShouldNot(BeNil())
			markJobComplete(getDatabaseJob(ns, "acct"))

			Eventually(func(g Gomega) {
				var db v1alpha1.IdentityServerDatabase
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "acct", Namespace: ns}, &db)).To(Succeed())
				g.Expect(hasCondition(db.Status.Conditions, v1alpha1.ConditionComplete, metav1.ConditionTrue)).To(BeTrue())
				g.Expect(hasCondition(db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue)).To(BeTrue())
				g.Expect(db.Status.CompletionTime).NotTo(BeNil())
			}).Should(Succeed())
		})
	})

	Context("connection sourcing", func() {
		It("loads the connection Secret via envFrom and reports JDBCURLMissing when JDBC_URL absent", func() {
			ns := dbTestNamespace()
			testCreateCluster(ns, "demo")
			// Secret without a JDBC_URL key.
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "db-conn", Namespace: ns},
				Data:       map[string][]byte{"JDBC_USERNAME": []byte("sa")},
			})).To(Succeed())
			createDatabase(ns, "acct", "demo", v1alpha1.JDBCConnection{SecretRef: "db-conn"}, nil)

			Eventually(func(g Gomega) {
				var db v1alpha1.IdentityServerDatabase
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "acct", Namespace: ns}, &db)).To(Succeed())
				g.Expect(conditionReason(db.Status.Conditions, v1alpha1.ConditionReady)).To(Equal(v1alpha1.ReasonJDBCURLMissing))
			}).Should(Succeed())
			Expect(getDatabaseJob(ns, "acct")).To(BeNil(), "no Job should be created without a JDBC_URL source")
		})

		It("creates the Job from a secretRef that carries JDBC_URL", func() {
			ns := dbTestNamespace()
			testCreateCluster(ns, "demo")
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "db-conn", Namespace: ns},
				Data: map[string][]byte{
					"JDBC_URL":      []byte("jdbc:postgresql://db/acct"),
					"JDBC_USERNAME": []byte("sa"),
					"JDBC_PASSWORD": []byte("pw"),
				},
			})).To(Succeed())
			createDatabase(ns, "acct", "demo", v1alpha1.JDBCConnection{SecretRef: "db-conn"}, nil)

			Eventually(func(g Gomega) {
				job := getDatabaseJob(ns, "acct")
				g.Expect(job).NotTo(BeNil())
				from := job.Spec.Template.Spec.Containers[0].EnvFrom
				g.Expect(from).To(HaveLen(1))
				g.Expect(from[0].SecretRef.Name).To(Equal("db-conn"))
			}).Should(Succeed())
		})
	})

	Context("referenced cluster missing", func() {
		It("sets Ready=False with ClusterNotFound", func() {
			ns := dbTestNamespace()
			createDatabase(ns, "acct", "ghost",
				v1alpha1.JDBCConnection{URL: inlineURLSource("jdbc:x")}, nil)
			Eventually(func(g Gomega) {
				var db v1alpha1.IdentityServerDatabase
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "acct", Namespace: ns}, &db)).To(Succeed())
				g.Expect(conditionReason(db.Status.Conditions, v1alpha1.ConditionReady)).To(Equal(v1alpha1.ReasonClusterNotFound))
			}).Should(Succeed())
		})
	})

	Context("admission validation", func() {
		It("rejects a connection with neither url nor secretRef", func() {
			ns := dbTestNamespace()
			err := k8sClient.Create(ctx, &v1alpha1.IdentityServerDatabase{
				ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns},
				Spec: v1alpha1.IdentityServerDatabaseSpec{
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "demo"},
					Connection:               v1alpha1.JDBCConnection{},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue())
		})

		It("rejects a ValueSource with both value and valueFrom", func() {
			ns := dbTestNamespace()
			err := k8sClient.Create(ctx, &v1alpha1.IdentityServerDatabase{
				ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns},
				Spec: v1alpha1.IdentityServerDatabaseSpec{
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "demo"},
					Connection: v1alpha1.JDBCConnection{URL: &v1alpha1.ValueSource{
						Value: "jdbc:x",
						ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k",
						}},
					}},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue())
		})

		It("rejects env that redefines a JDBC_* variable", func() {
			ns := dbTestNamespace()
			err := k8sClient.Create(ctx, &v1alpha1.IdentityServerDatabase{
				ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: ns},
				Spec: v1alpha1.IdentityServerDatabaseSpec{
					IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "demo"},
					Connection:               v1alpha1.JDBCConnection{URL: inlineURLSource("jdbc:x")},
					JobTemplate: &v1alpha1.DatabaseJobTemplate{
						Env: []corev1.EnvVar{{Name: "JDBC_URL", Value: "evil"}},
					},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue())
		})
	})
})
