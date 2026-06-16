package controller

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

func testDatabase(name string, conn v1alpha1.JDBCConnection, tmpl *v1alpha1.DatabaseJobTemplate) *v1alpha1.IdentityServerDatabase {
	return &v1alpha1.IdentityServerDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: v1alpha1.IdentityServerDatabaseSpec{
			IdentityServerClusterRef: v1alpha1.ObjectReference{Name: "demo"},
			Connection:               conn,
			JobTemplate:              tmpl,
		},
	}
}

func inlineURL(v string) *v1alpha1.ValueSource { return &v1alpha1.ValueSource{Value: v} }

func condIs(conds []metav1.Condition, condType string, status metav1.ConditionStatus) bool {
	for _, c := range conds {
		if c.Type == condType && c.Status == status {
			return true
		}
	}
	return false
}

func TestBuildDatabaseJob_CommandAndImage(t *testing.T) {
	cluster := &v1alpha1.IdentityServerCluster{Spec: v1alpha1.IdentityServerClusterSpec{Version: "11.0"}}
	image := buildImage(cluster)
	db := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:postgresql://db/acct")}, nil)

	job := buildDatabaseJob(db, cluster, image, "hash123")

	if got := job.Name; got != "acct-db-init-job" {
		t.Fatalf("job name = %q, want acct-db-init-job", got)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Name != databaseContainerName {
		t.Errorf("container name = %q, want %q", c.Name, databaseContainerName)
	}
	if c.Image != image {
		t.Errorf("image = %q, want %q", c.Image, image)
	}
	wantCmd := []string{"/opt/idsvr/bin/idsvr", "-I"}
	if len(c.Command) != 2 || c.Command[0] != wantCmd[0] || c.Command[1] != wantCmd[1] {
		t.Errorf("command = %v, want %v", c.Command, wantCmd)
	}
	if job.Annotations[databaseJobHashAnnotation] != "hash123" {
		t.Errorf("hash annotation = %q, want hash123", job.Annotations[databaseJobHashAnnotation])
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy = %q, want Never", job.Spec.Template.Spec.RestartPolicy)
	}
	if *job.Spec.BackoffLimit != jobBackoffLimit {
		t.Errorf("backoffLimit = %d, want %d", *job.Spec.BackoffLimit, jobBackoffLimit)
	}
}

func TestBuildDatabaseJob_ImageOverride(t *testing.T) {
	cluster := &v1alpha1.IdentityServerCluster{Spec: v1alpha1.IdentityServerClusterSpec{
		Version: "11.0",
		Image:   "registry.example.com/curity:custom",
	}}
	image := buildImage(cluster)
	db := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:x")}, nil)
	job := buildDatabaseJob(db, cluster, image, "h")
	if job.Spec.Template.Spec.Containers[0].Image != "registry.example.com/curity:custom" {
		t.Errorf("image = %q, want override", job.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestBuildJDBCEnv_InlineValues(t *testing.T) {
	conn := v1alpha1.JDBCConnection{
		URL:      inlineURL("jdbc:postgresql://db/acct"),
		Username: &v1alpha1.ValueSource{Value: "sa"},
		Password: &v1alpha1.ValueSource{Value: "secret"},
	}
	env := buildJDBCEnvVars(conn)
	want := map[string]string{"JDBC_URL": "jdbc:postgresql://db/acct", "JDBC_USERNAME": "sa", "JDBC_PASSWORD": "secret"}
	if len(env) != 3 {
		t.Fatalf("got %d env vars, want 3: %+v", len(env), env)
	}
	for _, e := range env {
		if want[e.Name] != e.Value {
			t.Errorf("env %s = %q, want %q", e.Name, e.Value, want[e.Name])
		}
	}
	if buildJDBCEnvFrom(conn) != nil {
		t.Error("envFrom should be nil when no secretRef")
	}
}

func TestBuildJDBCEnv_SecretKeyRef(t *testing.T) {
	conn := v1alpha1.JDBCConnection{
		Password: &v1alpha1.ValueSource{ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
				Key:                  "password",
			},
		}},
		URL: inlineURL("jdbc:x"),
	}
	env := buildJDBCEnvVars(conn)
	var pw *corev1.EnvVar
	for i := range env {
		if env[i].Name == "JDBC_PASSWORD" {
			pw = &env[i]
		}
	}
	if pw == nil || pw.ValueFrom == nil || pw.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("JDBC_PASSWORD should source from secretKeyRef: %+v", env)
	}
	if pw.ValueFrom.SecretKeyRef.Name != "db-creds" || pw.ValueFrom.SecretKeyRef.Key != "password" {
		t.Errorf("secretKeyRef = %+v", pw.ValueFrom.SecretKeyRef)
	}
}

func TestBuildJDBCEnv_SecretRefEnvFrom(t *testing.T) {
	conn := v1alpha1.JDBCConnection{SecretRef: "db-conn"}
	// No per-field env when only secretRef.
	if env := buildJDBCEnvVars(conn); len(env) != 0 {
		t.Errorf("expected no explicit env vars, got %+v", env)
	}
	from := buildJDBCEnvFrom(conn)
	if len(from) != 1 || from[0].SecretRef == nil || from[0].SecretRef.Name != "db-conn" {
		t.Fatalf("envFrom = %+v, want secretRef db-conn", from)
	}
	if from[0].SecretRef.Optional == nil || !*from[0].SecretRef.Optional {
		t.Error("envFrom secretRef should be optional")
	}
}

func TestBuildJDBCEnv_PerFieldOverridesSecretRef(t *testing.T) {
	// secretRef supplies the bulk; an explicit url field must override it. The
	// explicit env var is appended AFTER envFrom, so Kubernetes gives it
	// precedence — assert ordering reflects that.
	conn := v1alpha1.JDBCConnection{
		SecretRef: "db-conn",
		URL:       inlineURL("jdbc:override"),
	}
	cluster := &v1alpha1.IdentityServerCluster{Spec: v1alpha1.IdentityServerClusterSpec{Version: "11.0"}}
	job := buildDatabaseJob(testDatabase("acct", conn, nil), cluster, buildImage(cluster), "h")
	c := job.Spec.Template.Spec.Containers[0]
	if len(c.EnvFrom) != 1 {
		t.Fatalf("expected envFrom from secretRef, got %+v", c.EnvFrom)
	}
	found := false
	for _, e := range c.Env {
		if e.Name == "JDBC_URL" && e.Value == "jdbc:override" {
			found = true
		}
	}
	if !found {
		t.Errorf("explicit JDBC_URL override missing from env: %+v", c.Env)
	}
}

func TestBuildDatabaseJob_AppliesJobTemplate(t *testing.T) {
	cluster := &v1alpha1.IdentityServerCluster{Spec: v1alpha1.IdentityServerClusterSpec{Version: "11.0"}}
	tmpl := &v1alpha1.DatabaseJobTemplate{
		BackoffLimit:            ptr.To(int32(0)),
		ActiveDeadlineSeconds:   ptr.To(int64(600)),
		TTLSecondsAfterFinished: ptr.To(int32(120)),
		ServiceAccountName:      "db-migrator",
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		},
		ContainerSecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true)},
		SecurityContext:          &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true)},
		ImagePullPolicy:          corev1.PullAlways,
		ImagePullSecret:          "tmpl-pull",
		NodeSelector:             map[string]string{"disktype": "ssd"},
		PriorityClassName:        "high",
		Env:                      []corev1.EnvVar{{Name: "EXTRA", Value: "1"}},
		PodLabels:                map[string]string{"team": "iam"},
	}
	db := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:x")}, tmpl)
	job := buildDatabaseJob(db, cluster, buildImage(cluster), "h")

	ps := job.Spec.Template.Spec
	c := ps.Containers[0]
	if *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %d, want 0", *job.Spec.BackoffLimit)
	}
	if *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Errorf("activeDeadlineSeconds = %d, want 600", *job.Spec.ActiveDeadlineSeconds)
	}
	if *job.Spec.TTLSecondsAfterFinished != 120 {
		t.Errorf("ttl = %d, want 120", *job.Spec.TTLSecondsAfterFinished)
	}
	if ps.ServiceAccountName != "db-migrator" {
		t.Errorf("serviceAccountName = %q", ps.ServiceAccountName)
	}
	if c.Resources.Requests.Memory().String() != "256Mi" {
		t.Errorf("resources not applied: %+v", c.Resources)
	}
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("containerSecurityContext not applied")
	}
	if ps.SecurityContext == nil || ps.SecurityContext.RunAsNonRoot == nil || !*ps.SecurityContext.RunAsNonRoot {
		t.Error("pod securityContext not applied")
	}
	if c.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("imagePullPolicy = %q, want Always", c.ImagePullPolicy)
	}
	if len(ps.ImagePullSecrets) != 1 || ps.ImagePullSecrets[0].Name != "tmpl-pull" {
		t.Errorf("imagePullSecret = %+v, want tmpl-pull", ps.ImagePullSecrets)
	}
	if ps.NodeSelector["disktype"] != "ssd" {
		t.Errorf("nodeSelector not applied: %+v", ps.NodeSelector)
	}
	if ps.PriorityClassName != "high" {
		t.Errorf("priorityClassName = %q", ps.PriorityClassName)
	}
	// EXTRA env appended after JDBC_URL.
	lastNamed := c.Env[len(c.Env)-1]
	if lastNamed.Name != "EXTRA" {
		t.Errorf("extra env not appended last: %+v", c.Env)
	}
	if job.Spec.Template.Labels["team"] != "iam" {
		t.Error("pod label not applied")
	}
	if job.Spec.Template.Labels["curity.io/database"] != "acct" {
		t.Error("operator-owned pod label missing")
	}
}

func TestBuildDatabaseJob_SecurityContextDefaults(t *testing.T) {
	cluster := &v1alpha1.IdentityServerCluster{Spec: v1alpha1.IdentityServerClusterSpec{Version: "11.0"}}
	db := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:x")}, nil)
	job := buildDatabaseJob(db, cluster, buildImage(cluster), "h")
	sc := job.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 10001 {
		t.Errorf("runAsUser default = %v, want 10001", sc)
	}
	if *sc.RunAsGroup != 10000 || *sc.FSGroup != 10000 {
		t.Errorf("group defaults wrong: %+v", sc)
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken should default to false")
	}
}

func TestBuildDatabaseJob_ImagePullSecretFallsBackToCluster(t *testing.T) {
	cluster := &v1alpha1.IdentityServerCluster{Spec: v1alpha1.IdentityServerClusterSpec{
		Version:         "11.0",
		ImagePullSecret: "cluster-pull",
	}}
	db := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:x")}, nil)
	job := buildDatabaseJob(db, cluster, buildImage(cluster), "h")
	ps := job.Spec.Template.Spec.ImagePullSecrets
	if len(ps) != 1 || ps[0].Name != "cluster-pull" {
		t.Errorf("imagePullSecret = %+v, want cluster-pull", ps)
	}
}

func TestComputeDatabaseJobHash(t *testing.T) {
	db := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:x")}, nil)
	h1 := computeDatabaseJobHash(db, "img:1")
	h2 := computeDatabaseJobHash(db, "img:1")
	if h1 != h2 {
		t.Error("hash not stable for equal inputs")
	}
	if computeDatabaseJobHash(db, "img:2") == h1 {
		t.Error("hash must change when image changes")
	}
	db2 := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:other")}, nil)
	if computeDatabaseJobHash(db2, "img:1") == h1 {
		t.Error("hash must change when connection changes")
	}
	db3 := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:x")},
		&v1alpha1.DatabaseJobTemplate{ServiceAccountName: "sa"})
	if computeDatabaseJobHash(db3, "img:1") == h1 {
		t.Error("hash must change when jobTemplate changes")
	}
}

func TestResolveDatabaseImagePullPolicy(t *testing.T) {
	if got := resolveDatabaseImagePullPolicy(&v1alpha1.DatabaseJobTemplate{ImagePullPolicy: corev1.PullNever}, "img:1"); got != corev1.PullNever {
		t.Errorf("explicit policy not honored: %q", got)
	}
	if got := resolveDatabaseImagePullPolicy(&v1alpha1.DatabaseJobTemplate{}, "img:latest"); got != corev1.PullAlways {
		t.Errorf(":latest default = %q, want Always", got)
	}
	if got := resolveDatabaseImagePullPolicy(&v1alpha1.DatabaseJobTemplate{}, "img:1.2.3"); got != corev1.PullIfNotPresent {
		t.Errorf("tagged default = %q, want IfNotPresent", got)
	}
}

func TestApplyJobStatus_Conditions(t *testing.T) {
	r := &IdentityServerDatabaseReconciler{}

	complete := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
	}}}
	db := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:x")}, nil)
	r.applyJobStatus(db, complete)
	if !condIs(db.Status.Conditions, v1alpha1.ConditionComplete, metav1.ConditionTrue) {
		t.Error("Complete should be True")
	}
	if !condIs(db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue) {
		t.Error("Ready should be True on completion")
	}
	if db.Status.CompletionTime == nil {
		t.Error("CompletionTime should be set")
	}

	failed := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "boom"},
	}}}
	db2 := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:x")}, nil)
	r.applyJobStatus(db2, failed)
	if !condIs(db2.Status.Conditions, v1alpha1.ConditionFailed, metav1.ConditionTrue) {
		t.Error("Failed should be True")
	}
	if !condIs(db2.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse) {
		t.Error("Ready should be False on failure")
	}

	running := &batchv1.Job{Status: batchv1.JobStatus{Active: 1}}
	db3 := testDatabase("acct", v1alpha1.JDBCConnection{URL: inlineURL("jdbc:x")}, nil)
	r.applyJobStatus(db3, running)
	if !condIs(db3.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue) {
		t.Error("Progressing should be True while running")
	}
}
