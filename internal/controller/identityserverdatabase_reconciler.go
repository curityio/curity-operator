package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

const (
	// databaseJobSuffix is appended to the IdentityServerDatabase name to form
	// the managed Job name.
	databaseJobSuffix = "-db-init-job"
	// databaseContainerName is the name of the schema-management container. It
	// is referenced by the Job's podFailurePolicy.
	databaseContainerName = "idsvr-db-init"
	// databaseJobHashAnnotation stamps the trigger hash (image + spec) on the
	// Job so the controller can detect when to recreate it.
	databaseJobHashAnnotation = "curity.io/database-job-hash"
)

// IdentityServerDatabaseReconciler reconciles an IdentityServerDatabase object.
// It runs a one-shot Job (idsvr -I) that initializes the Curity database
// schema, using the same image as the referenced IdentityServerCluster, and
// recreates the Job whenever that image or the CR spec changes.
type IdentityServerDatabaseReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=curity.io,resources=identityserverdatabases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=curity.io,resources=identityserverdatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=curity.io,resources=identityserverdatabases/finalizers,verbs=update
// +kubebuilder:rbac:groups=curity.io,resources=identityserverclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

// Reconcile handles a single reconciliation loop for an IdentityServerDatabase.
func (r *IdentityServerDatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	var db v1alpha1.IdentityServerDatabase
	if err := r.Get(ctx, req.NamespacedName, &db); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get IdentityServerDatabase: %w", err)
	}

	db.Status.ObservedGeneration = db.Generation

	// Resolve the referenced cluster — it supplies the image and triggers
	// re-runs when its version/image override changes.
	var cluster v1alpha1.IdentityServerCluster
	clusterKey := client.ObjectKey{Name: db.Spec.IdentityServerClusterRef.Name, Namespace: db.Namespace}
	if err := r.Get(ctx, clusterKey, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
				v1alpha1.ReasonClusterNotFound,
				fmt.Sprintf("referenced IdentityServerCluster %q not found", db.Spec.IdentityServerClusterRef.Name),
				db.Generation)
			// A cluster create later re-enqueues this object via the watch.
			return ctrl.Result{}, r.updateStatus(ctx, &db)
		}
		return ctrl.Result{}, fmt.Errorf("failed to get IdentityServerCluster %q: %w", clusterKey.Name, err)
	}

	image := buildImage(&cluster)

	// When JDBC_URL has no inline/per-field source, it must come from the
	// referenced Secret. Verify the Secret exists and carries a JDBC_URL key
	// before launching a Job that would otherwise fail at runtime. CEL already
	// guarantees one of url/secretRef is declared.
	if db.Spec.Connection.URL == nil {
		secretName := db.Spec.Connection.SecretRef
		var secret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: db.Namespace}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
					v1alpha1.ReasonConnectionSecretMissing,
					fmt.Sprintf("connection.secretRef Secret %q not found", secretName), db.Generation)
				return ctrl.Result{}, r.updateStatus(ctx, &db)
			}
			return ctrl.Result{}, fmt.Errorf("failed to get connection Secret %q: %w", secretName, err)
		}
		if _, ok := secret.Data["JDBC_URL"]; !ok {
			setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
				v1alpha1.ReasonJDBCURLMissing,
				fmt.Sprintf("Secret %q has no JDBC_URL key; set connection.url or add the key", secretName), db.Generation)
			return ctrl.Result{}, r.updateStatus(ctx, &db)
		}
	}

	hash := computeDatabaseJobHash(&db, image)
	jobName := db.Name + databaseJobSuffix
	db.Status.JobName = jobName
	db.Status.ObservedImage = image
	db.Status.ObservedHash = hash

	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: db.Namespace}, &job)
	if apierrors.IsNotFound(err) {
		return r.createJob(ctx, &db, &cluster, image, hash)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get Job %q: %w", jobName, err)
	}

	// A Job mid-deletion (foreground GC after a hash change) is not yet ready
	// to recreate; wait for it to disappear (the Owns watch re-enqueues on the
	// delete event, the RequeueAfter is a backstop).
	if !job.DeletionTimestamp.IsZero() {
		setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue,
			v1alpha1.ReasonJobCreated, "waiting for the previous Job to be deleted before recreating", db.Generation)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.updateStatus(ctx, &db)
	}

	// Re-trigger: the resolved image or the spec changed since this Job ran.
	if job.Annotations[databaseJobHashAnnotation] != hash {
		log.Info("image or spec changed, recreating database-init Job", "job", jobName)
		if delErr := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("failed to delete outdated Job: %w", delErr)
		}
		r.Recorder.Eventf(&db, corev1.EventTypeNormal, "JobRecreated",
			"recreating database-init Job %q after image/spec change", jobName)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue,
			v1alpha1.ReasonJobCreated, "image or spec changed; recreating Job", db.Generation)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, r.updateStatus(ctx, &db)
	}

	// Steady state — reflect the Job's outcome in status.
	r.applyJobStatus(&db, &job)
	return ctrl.Result{}, r.updateStatus(ctx, &db)
}

// createJob builds and creates the database-init Job and sets initial status.
func (r *IdentityServerDatabaseReconciler) createJob(ctx context.Context, db *v1alpha1.IdentityServerDatabase, cluster *v1alpha1.IdentityServerCluster, image, hash string) (ctrl.Result, error) {
	job := buildDatabaseJob(db, cluster, image, hash)
	if err := controllerutil.SetControllerReference(db, job, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner ref on database Job: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost a race; the next reconcile will observe it.
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to create database Job: %w", err)
	}
	r.Recorder.Eventf(db, corev1.EventTypeNormal, "JobCreated",
		"created database-init Job %q (image %q)", job.Name, image)
	setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue,
		v1alpha1.ReasonJobCreated, "database-init Job created", db.Generation)
	setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
		v1alpha1.ReasonJobRunning, "database-init Job created and not yet complete", db.Generation)
	return ctrl.Result{}, r.updateStatus(ctx, db)
}

// applyJobStatus maps the Job's batch conditions onto the CR conditions.
func (r *IdentityServerDatabaseReconciler) applyJobStatus(db *v1alpha1.IdentityServerDatabase, job *batchv1.Job) {
	gen := db.Generation
	if ct := jobCompletionTime(job); ct != nil {
		db.Status.CompletionTime = ct
		setCondition(&db.Status.Conditions, v1alpha1.ConditionComplete, metav1.ConditionTrue,
			v1alpha1.ReasonJobComplete, "database schema initialized successfully", gen)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue,
			v1alpha1.ReasonJobComplete, "database schema initialized successfully", gen)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse,
			v1alpha1.ReasonJobComplete, "Job complete", gen)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionFailed, metav1.ConditionFalse,
			v1alpha1.ReasonJobComplete, "Job complete", gen)
		return
	}
	if msg, failed := jobFailureMessage(job); failed {
		setCondition(&db.Status.Conditions, v1alpha1.ConditionFailed, metav1.ConditionTrue,
			v1alpha1.ReasonJobFailed, msg, gen)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
			v1alpha1.ReasonJobFailed, msg, gen)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse,
			v1alpha1.ReasonJobFailed, "Job failed", gen)
		return
	}
	setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue,
		v1alpha1.ReasonJobRunning, "database-init Job is running", gen)
	setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
		v1alpha1.ReasonJobRunning, "database-init Job has not completed", gen)
}

func (r *IdentityServerDatabaseReconciler) updateStatus(ctx context.Context, db *v1alpha1.IdentityServerDatabase) error {
	if err := r.Status().Update(ctx, db); err != nil {
		return fmt.Errorf("failed to update IdentityServerDatabase status: %w", err)
	}
	return nil
}

// jobFailureMessage returns the Job's failure message and true when the Job has
// a Failed=True condition.
func jobFailureMessage(job *batchv1.Job) (string, bool) {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			if cond.Message != "" {
				return fmt.Sprintf("database-init Job failed: %s (%s)", cond.Message, cond.Reason), true
			}
			return fmt.Sprintf("database-init Job failed: %s", cond.Reason), true
		}
	}
	return "", false
}

// computeDatabaseJobHash derives the re-trigger key from the resolved image and
// the parts of the spec that shape the Job. A change recreates the Job.
func computeDatabaseJobHash(db *v1alpha1.IdentityServerDatabase, image string) string {
	payload := struct {
		Image       string                        `json:"image"`
		Connection  v1alpha1.JDBCConnection       `json:"connection"`
		JobTemplate *v1alpha1.DatabaseJobTemplate `json:"jobTemplate,omitempty"`
	}{
		Image:       image,
		Connection:  db.Spec.Connection,
		JobTemplate: db.Spec.JobTemplate,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// json.Marshal of these plain structs does not fail in practice; fall
		// back to the image so the hash is still defined.
		b = []byte(image)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// buildDatabaseJob constructs the one-shot Job that runs `idsvr -I`.
func buildDatabaseJob(db *v1alpha1.IdentityServerDatabase, cluster *v1alpha1.IdentityServerCluster, image, hash string) *batchv1.Job {
	tmpl := db.Spec.JobTemplate
	if tmpl == nil {
		tmpl = &v1alpha1.DatabaseJobTemplate{}
	}

	backoff := jobBackoffLimit
	if tmpl.BackoffLimit != nil {
		backoff = *tmpl.BackoffLimit
	}

	container := corev1.Container{
		Name:            databaseContainerName,
		Image:           image,
		ImagePullPolicy: resolveDatabaseImagePullPolicy(tmpl, image),
		Command:         []string{"/opt/idsvr/bin/idsvr", "-I"},
		// Surface the failure cause in terminated.message without streaming logs.
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		Env:                      append(buildJDBCEnvVars(db.Spec.Connection), tmpl.Env...),
		EnvFrom:                  buildJDBCEnvFrom(db.Spec.Connection),
		SecurityContext:          tmpl.ContainerSecurityContext,
	}
	if tmpl.Resources != nil {
		container.Resources = *tmpl.Resources
	}

	automount := ptr.To(false)
	if tmpl.AutomountServiceAccountToken != nil {
		automount = tmpl.AutomountServiceAccountToken
	}

	podSpec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		AutomountServiceAccountToken:  automount,
		ServiceAccountName:            tmpl.ServiceAccountName,
		SecurityContext:               mergePodSecurityContext(tmpl.SecurityContext),
		Containers:                    []corev1.Container{container},
		NodeSelector:                  tmpl.NodeSelector,
		Tolerations:                   tmpl.Tolerations,
		Affinity:                      tmpl.Affinity,
		TopologySpreadConstraints:     tmpl.TopologySpreadConstraints,
		PriorityClassName:             tmpl.PriorityClassName,
		TerminationGracePeriodSeconds: tmpl.TerminationGracePeriodSeconds,
	}

	pullSecret := tmpl.ImagePullSecret
	if pullSecret == "" {
		pullSecret = cluster.Spec.ImagePullSecret
	}
	if pullSecret != "" {
		podSpec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: pullSecret}}
	}

	// Pod template labels: user labels first, operator-owned keys overlaid last
	// so they can never be shadowed (CEL also forbids them in podLabels).
	podLabels := map[string]string{}
	for k, v := range tmpl.PodLabels {
		podLabels[k] = v
	}
	podLabels["curity.io/database"] = db.Name
	podLabels["curity.io/component"] = "database-init"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      db.Name + databaseJobSuffix,
			Namespace: db.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "curity-operator",
				"curity.io/database":           db.Name,
				"curity.io/component":          "database-init",
			},
			Annotations: map[string]string{
				databaseJobHashAnnotation: hash,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   tmpl.ActiveDeadlineSeconds,
			TTLSecondsAfterFinished: tmpl.TTLSecondsAfterFinished,
			// Retry through infra disruptions; fail fast on a real idsvr error.
			PodFailurePolicy: &batchv1.PodFailurePolicy{
				Rules: []batchv1.PodFailurePolicyRule{
					{
						Action: batchv1.PodFailurePolicyActionIgnore,
						OnPodConditions: []batchv1.PodFailurePolicyOnPodConditionsPattern{
							{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue},
						},
					},
					{
						Action: batchv1.PodFailurePolicyActionFailJob,
						OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
							ContainerName: ptr.To(databaseContainerName),
							Operator:      batchv1.PodFailurePolicyOnExitCodesOpNotIn,
							Values:        []int32{0},
						},
					},
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: tmpl.PodAnnotations,
				},
				Spec: podSpec,
			},
		},
	}

	return job
}

// buildJDBCEnvFrom loads the whole connection Secret as env vars when secretRef
// is set, so its JDBC_URL/JDBC_USERNAME/JDBC_PASSWORD keys appear in the
// container. Optional so missing keys are tolerated.
func buildJDBCEnvFrom(conn v1alpha1.JDBCConnection) []corev1.EnvFromSource {
	if conn.SecretRef == "" {
		return nil
	}
	return []corev1.EnvFromSource{
		{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: conn.SecretRef},
				Optional:             ptr.To(true),
			},
		},
	}
}

// buildJDBCEnvVars maps the per-field connection settings onto explicit env
// vars. They are appended after envFrom, so an explicit field overrides the
// matching key sourced from secretRef.
func buildJDBCEnvVars(conn v1alpha1.JDBCConnection) []corev1.EnvVar {
	var env []corev1.EnvVar
	if e, ok := valueSourceToEnvVar("JDBC_URL", conn.URL); ok {
		env = append(env, e)
	}
	if e, ok := valueSourceToEnvVar("JDBC_USERNAME", conn.Username); ok {
		env = append(env, e)
	}
	if e, ok := valueSourceToEnvVar("JDBC_PASSWORD", conn.Password); ok {
		env = append(env, e)
	}
	return env
}

func valueSourceToEnvVar(name string, vs *v1alpha1.ValueSource) (corev1.EnvVar, bool) {
	if vs == nil {
		return corev1.EnvVar{}, false
	}
	e := corev1.EnvVar{Name: name}
	if vs.ValueFrom != nil {
		e.ValueFrom = vs.ValueFrom
	} else {
		e.Value = vs.Value
	}
	return e, true
}

func resolveDatabaseImagePullPolicy(tmpl *v1alpha1.DatabaseJobTemplate, image string) corev1.PullPolicy {
	if tmpl.ImagePullPolicy != "" {
		return tmpl.ImagePullPolicy
	}
	return defaultPullPolicy(image)
}

// findDatabasesForCluster maps an IdentityServerCluster change to the
// IdentityServerDatabase objects that reference it, so a version/image bump
// re-triggers the Job.
func (r *IdentityServerDatabaseReconciler) findDatabasesForCluster(ctx context.Context, obj client.Object) []ctrl.Request {
	cluster, ok := obj.(*v1alpha1.IdentityServerCluster)
	if !ok {
		return nil
	}
	var list v1alpha1.IdentityServerDatabaseList
	if err := r.List(ctx, &list, client.InNamespace(cluster.Namespace)); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		if list.Items[i].Spec.IdentityServerClusterRef.Name == cluster.Name {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKey{
				Name:      list.Items[i].Name,
				Namespace: list.Items[i].Namespace,
			}})
		}
	}
	return reqs
}

// SetupWithManager registers the controller.
func (r *IdentityServerDatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.IdentityServerDatabase{},
			builder.WithPredicates(predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.LabelChangedPredicate{},
				predicate.AnnotationChangedPredicate{},
			))).
		Owns(&batchv1.Job{}).
		Watches(
			&v1alpha1.IdentityServerCluster{},
			handler.EnqueueRequestsFromMapFunc(r.findDatabasesForCluster),
		).
		Complete(r)
}
