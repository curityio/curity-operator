package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	databaseJobSuffix = "-db-init-job"
	// Matched by name in the Job's podFailurePolicy.
	databaseContainerName = "idsvr-db-init"
	// Image+spec hash, stamped on the Job to detect when to recreate it.
	databaseJobHashAnnotation = "curity.io/database-job-hash"
	// Hash of the connection Secrets' data; a change to it recovers a failed Job.
	connectionSecretsHashAnnotation = "curity.io/connection-secrets-hash"
	// Labels on the Job/pod, also used to route pod events back to the CR.
	databaseLabel          = "curity.io/database"
	componentLabel         = "curity.io/component"
	databaseComponentValue = "database-init"
	// Requeue interval to re-check a pod-admission denial without spinning.
	jobAdmissionRetryInterval = time.Minute
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
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch

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

	// CEL guarantees a source is declared; confirm every referenced Secret/key
	// exists before launching a Job that would otherwise fail.
	if reason, msg, err := r.checkConnectionSources(ctx, &db); err != nil {
		return ctrl.Result{}, err
	} else if reason != "" {
		setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
			reason, msg, db.Generation)
		return ctrl.Result{}, r.updateStatus(ctx, &db)
	}

	hash := computeDatabaseJobHash(&db, image)
	connHash, err := r.computeConnectionSecretsHash(ctx, db.Spec.Connection, db.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	jobName := db.Name + databaseJobSuffix
	db.Status.JobName = jobName
	db.Status.ObservedImage = image

	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: db.Namespace}, &job)
	if apierrors.IsNotFound(err) {
		// Job gone but this hash already completed: TTL cleaned it up — don't
		// re-run idsvr -I on an initialized DB. A new hash still recreates.
		if db.Status.CompletionTime != nil && db.Status.LastCompletedHash == hash {
			return ctrl.Result{}, r.updateStatus(ctx, &db)
		}
		return r.createJob(ctx, &db, &cluster, image, hash, connHash)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get Job %q: %w", jobName, err)
	}

	// Wait for an in-progress deletion to finish before recreating (the Owns
	// watch re-enqueues on delete; RequeueAfter is a backstop).
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
		clearPriorOutcome(&db)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue,
			v1alpha1.ReasonJobCreated, "image or spec changed; recreating Job", db.Generation)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
			v1alpha1.ReasonJobCreated, "image or spec changed; recreating Job", db.Generation)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, r.updateStatus(ctx, &db)
	}

	// Failed Job whose connection Secret has since changed: recreate to pick up
	// the fix. The stamped hash gates this to once per Secret change.
	if _, failed := jobFailureMessage(&job); failed && job.Annotations[connectionSecretsHashAnnotation] != connHash {
		log.Info("connection Secret changed, recreating failed database-init Job", "job", jobName)
		if delErr := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("failed to delete failed Job for secret-change recovery: %w", delErr)
		}
		r.Recorder.Eventf(&db, corev1.EventTypeNormal, "JobRecreated",
			"connection Secret changed; recreating database-init Job %q", jobName)
		clearPriorOutcome(&db)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue,
			v1alpha1.ReasonJobCreated, "connection Secret changed; recreating Job", db.Generation)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
			v1alpha1.ReasonJobCreated, "connection Secret changed; recreating Job", db.Generation)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, r.updateStatus(ctx, &db)
	}

	// Steady state — reflect the Job's outcome in status. While non-terminal,
	// surface a stuck pod's cause (the Job reports none of those).
	if r.applyJobStatus(&db, &job) {
		if err := r.surfacePodStartFailure(ctx, &db, &job); err != nil {
			// Persist the running status best-effort, then requeue on the list
			// error rather than reporting healthy progress while blind to the pod.
			_ = r.updateStatus(ctx, &db)
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, r.updateStatus(ctx, &db)
}

// checkConnectionSources pre-flights every declared connection source before a
// Job is launched: the required JDBC_URL, plus any per-field username/password
// secretKeyRef the user set. Inline values and unset fields are left alone — we
// only resolve what the user actually pointed at a Secret. Returns a
// (reason, message) for Ready=False, or empty when all sources resolve.
func (r *IdentityServerDatabaseReconciler) checkConnectionSources(ctx context.Context, db *v1alpha1.IdentityServerDatabase) (reason, message string, err error) {
	conn := db.Spec.Connection

	// JDBC_URL is the one required setting; check it whichever way it is sourced.
	if conn.URL == nil {
		// Sourced from the bundle secretRef Secret's JDBC_URL key.
		var secret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Name: conn.SecretRef, Namespace: db.Namespace}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return v1alpha1.ReasonConnectionSecretMissing,
					fmt.Sprintf("connection.secretRef Secret %q not found", conn.SecretRef), nil
			}
			return "", "", fmt.Errorf("failed to get connection Secret %q: %w", conn.SecretRef, err)
		}
		if _, ok := secret.Data["JDBC_URL"]; !ok {
			return v1alpha1.ReasonJDBCURLMissing,
				fmt.Sprintf("Secret %q has no JDBC_URL key; set connection.url or add the key", conn.SecretRef), nil
		}
	} else if skr := conn.URL.SecretKeyRef; skr != nil {
		var secret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Name: skr.Name, Namespace: db.Namespace}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return v1alpha1.ReasonConnectionSecretMissing,
					fmt.Sprintf("connection.url.secretKeyRef Secret %q not found", skr.Name), nil
			}
			return "", "", fmt.Errorf("failed to get connection.url Secret %q: %w", skr.Name, err)
		}
		if _, ok := secret.Data[skr.Key]; !ok {
			return v1alpha1.ReasonJDBCURLMissing,
				fmt.Sprintf("Secret %q has no key %q for JDBC_URL", skr.Name, skr.Key), nil
		}
	}

	// username/password are optional — only pre-flight a secretKeyRef the user set.
	if reason, msg, err := r.checkValueSourceSecret(ctx, db.Namespace, "connection.username", conn.Username); reason != "" || err != nil {
		return reason, msg, err
	}
	return r.checkValueSourceSecret(ctx, db.Namespace, "connection.password", conn.Password)
}

// checkValueSourceSecret pre-flights an optional per-field connection
// secretKeyRef. A nil field or an inline value needs no check — we resolve only
// what the user set. Returns a (reason, message) for Ready=False, or empty.
func (r *IdentityServerDatabaseReconciler) checkValueSourceSecret(ctx context.Context, namespace, field string, vs *v1alpha1.ValueSource) (reason, message string, err error) {
	if vs == nil || vs.SecretKeyRef == nil {
		return "", "", nil
	}
	skr := vs.SecretKeyRef
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: skr.Name, Namespace: namespace}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return v1alpha1.ReasonConnectionSecretMissing,
				fmt.Sprintf("%s.secretKeyRef Secret %q not found", field, skr.Name), nil
		}
		return "", "", fmt.Errorf("failed to get %s Secret %q: %w", field, skr.Name, err)
	}
	if _, ok := secret.Data[skr.Key]; !ok {
		return v1alpha1.ReasonConnectionKeyMissing,
			fmt.Sprintf("Secret %q has no key %q for %s", skr.Name, skr.Key, field), nil
	}
	return "", "", nil
}

func (r *IdentityServerDatabaseReconciler) createJob(ctx context.Context, db *v1alpha1.IdentityServerDatabase, cluster *v1alpha1.IdentityServerCluster, image, hash, connHash string) (ctrl.Result, error) {
	job := buildDatabaseJob(db, cluster, image, hash, connHash)
	if err := controllerutil.SetControllerReference(db, job, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set owner ref on database Job: %w", err)
	}
	// Catch an admission denial before the Job exists, instead of leaving it
	// unable to ever create a pod with no cause on the CR.
	if err := r.dryRunPodAdmission(ctx, db, job); err != nil {
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			msg := truncateMessage(fmt.Sprintf("pod admission rejected: %s", err.Error()), 256)
			clearPriorOutcome(db)
			setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue,
				v1alpha1.ReasonJobPodNotStarting, msg, db.Generation)
			setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
				v1alpha1.ReasonJobPodNotStarting, msg, db.Generation)
			// RequeueAfter (not an error) self-heals once fixed without log spam.
			return ctrl.Result{RequeueAfter: jobAdmissionRetryInterval}, r.updateStatus(ctx, db)
		}
		return ctrl.Result{}, fmt.Errorf("pre-flight pod admission check failed: %w", err)
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
	clearPriorOutcome(db)
	setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue,
		v1alpha1.ReasonJobCreated, "database-init Job created", db.Generation)
	setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
		v1alpha1.ReasonJobRunning, "database-init Job created and not yet complete", db.Generation)
	return ctrl.Result{}, r.updateStatus(ctx, db)
}

// dryRunPodAdmission runs the pod template through the apiserver admission chain
// without persisting. Deterministic Name (not GenerateName) keeps the Forbidden
// message — which embeds the pod name — stable across reconciles.
func (r *IdentityServerDatabaseReconciler) dryRunPodAdmission(ctx context.Context, db *v1alpha1.IdentityServerDatabase, job *batchv1.Job) error {
	return r.Create(ctx, buildDryRunPod(db, job), client.DryRunAll)
}

func buildDryRunPod(db *v1alpha1.IdentityServerDatabase, job *batchv1.Job) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        job.Name + "-dryrun-preflight",
			Namespace:   db.Namespace,
			Labels:      job.Spec.Template.Labels,
			Annotations: job.Spec.Template.Annotations,
		},
		Spec: *job.Spec.Template.Spec.DeepCopy(),
	}
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	return pod
}

// clearPriorOutcome drops a prior run's terminal status before (re)creating the
// Job, so a stale Complete/Failed isn't reported alongside the new run.
func clearPriorOutcome(db *v1alpha1.IdentityServerDatabase) {
	db.Status.CompletionTime = nil
	apimeta.RemoveStatusCondition(&db.Status.Conditions, v1alpha1.ConditionComplete)
	apimeta.RemoveStatusCondition(&db.Status.Conditions, v1alpha1.ConditionFailed)
}

// applyJobStatus maps the Job's conditions onto the CR. Returns true when the
// Job is non-terminal, so the caller can layer a stuck-pod cause on top.
func (r *IdentityServerDatabaseReconciler) applyJobStatus(db *v1alpha1.IdentityServerDatabase, job *batchv1.Job) (running bool) {
	gen := db.Generation
	if ct := jobCompletionTime(job); ct != nil {
		db.Status.CompletionTime = ct
		db.Status.LastCompletedHash = job.Annotations[databaseJobHashAnnotation]
		setCondition(&db.Status.Conditions, v1alpha1.ConditionComplete, metav1.ConditionTrue,
			v1alpha1.ReasonJobComplete, "database schema initialized successfully", gen)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionTrue,
			v1alpha1.ReasonJobComplete, "database schema initialized successfully", gen)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse,
			v1alpha1.ReasonJobComplete, "Job complete", gen)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionFailed, metav1.ConditionFalse,
			v1alpha1.ReasonJobComplete, "Job complete", gen)
		return false
	}
	if msg, failed := jobFailureMessage(job); failed {
		setCondition(&db.Status.Conditions, v1alpha1.ConditionFailed, metav1.ConditionTrue,
			v1alpha1.ReasonJobFailed, msg, gen)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
			v1alpha1.ReasonJobFailed, msg, gen)
		setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse,
			v1alpha1.ReasonJobFailed, "Job failed", gen)
		return false
	}
	setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue,
		v1alpha1.ReasonJobRunning, "database-init Job is running", gen)
	setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
		v1alpha1.ReasonJobRunning, "database-init Job has not completed", gen)
	return true
}

// surfacePodStartFailure reports why the Job's pod can't start, or — when no pod
// was created at all — why creation was denied. Returns errors so the caller requeues.
func (r *IdentityServerDatabaseReconciler) surfacePodStartFailure(ctx context.Context, db *v1alpha1.IdentityServerDatabase, job *batchv1.Job) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(db.Namespace),
		client.MatchingLabels{databaseLabel: db.Name, componentLabel: databaseComponentValue}); err != nil {
		return fmt.Errorf("failed to list database-init pods: %w", err)
	}
	// Only this Job's pods — a not-yet-GC'd pod from a prior run must not be
	// mistaken for the current attempt.
	var owned []corev1.Pod
	for i := range pods.Items {
		if metav1.IsControlledBy(&pods.Items[i], job) {
			owned = append(owned, pods.Items[i])
		}
	}
	msg := translateDatabaseInitPod(owned)
	if msg == "" && len(owned) == 0 {
		// No pod created at all — surface the admission denial from the Event.
		evMsg, err := r.latestJobFailedCreateMessage(ctx, job.UID, db.Namespace)
		if err != nil {
			return err
		}
		if evMsg != "" {
			msg = truncateMessage(fmt.Sprintf("pod cannot be created: %s", evMsg), 256)
		}
	}
	if msg == "" {
		return nil
	}
	setCondition(&db.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
		v1alpha1.ReasonJobPodNotStarting, msg, db.Generation)
	setCondition(&db.Status.Conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue,
		v1alpha1.ReasonJobPodNotStarting, msg, db.Generation)
	return nil
}

// translateDatabaseInitPod returns a concise cause when the most recent init pod
// is stuck before running, or "" when nothing is wrong.
func translateDatabaseInitPod(pods []corev1.Pod) string {
	pod := mostRecentPod(pods)
	if pod == nil || pod.DeletionTimestamp != nil {
		return ""
	}
	if pod.Status.Phase == corev1.PodPending {
		for i := range pod.Status.Conditions {
			c := &pod.Status.Conditions[i]
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
				return truncateMessage(fmt.Sprintf("pod cannot be scheduled: %s", c.Message), 256)
			}
		}
	}
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name != databaseContainerName {
			continue
		}
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				return truncateMessage(fmt.Sprintf("image pull failed: %s", w.Message), 256)
			case "CreateContainerConfigError":
				return truncateMessage(fmt.Sprintf("container config error: %s", w.Message), 256)
			case "CrashLoopBackOff":
				// The crash detail is in the previous termination, not Waiting.
				return truncateMessage(fmt.Sprintf("container crash-looping: %s", terminationDetail(cs.LastTerminationState.Terminated)), 256)
			}
		}
		// Started then exited non-zero (wrong creds, unreachable DB) — the Job
		// stays non-terminal through its backoffLimit retries.
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return truncateMessage(fmt.Sprintf("container failed: %s", terminationDetail(t)), 256)
		}
	}
	return ""
}

// terminationDetail renders a terminated container's cause, preferring the
// message (FallbackToLogsOnError fills it) then the reason, always with the code.
func terminationDetail(t *corev1.ContainerStateTerminated) string {
	if t == nil {
		return "container terminated"
	}
	switch {
	case t.Message != "":
		return fmt.Sprintf("%s (exit %d)", strings.TrimSpace(t.Message), t.ExitCode)
	case t.Reason != "":
		return fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
	default:
		return fmt.Sprintf("exit %d", t.ExitCode)
	}
}

// mostRecentPod picks the newest pod by CreationTimestamp; ties are broken by
// name so the choice is deterministic.
func mostRecentPod(pods []corev1.Pod) *corev1.Pod {
	if len(pods) == 0 {
		return nil
	}
	winner := &pods[0]
	for i := 1; i < len(pods); i++ {
		p := &pods[i]
		if p.CreationTimestamp.After(winner.CreationTimestamp.Time) ||
			(p.CreationTimestamp.Equal(&winner.CreationTimestamp) && p.Name > winner.Name) {
			winner = p
		}
	}
	return winner
}

func (r *IdentityServerDatabaseReconciler) updateStatus(ctx context.Context, db *v1alpha1.IdentityServerDatabase) error {
	if err := r.Status().Update(ctx, db); err != nil {
		if apierrors.IsConflict(err) {
			// Benign optimistic-lock conflict; the conflicting write re-reconciles.
			return nil
		}
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

// connectionSecretNames returns the deduplicated, sorted names of every Secret
// the connection references (secretRef plus any per-field secretKeyRef).
func connectionSecretNames(conn v1alpha1.JDBCConnection) []string {
	seen := map[string]struct{}{}
	var names []string
	add := func(n string) {
		if n == "" {
			return
		}
		if _, dup := seen[n]; dup {
			return
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	add(conn.SecretRef)
	for _, vs := range []*v1alpha1.ValueSource{conn.URL, conn.Username, conn.Password} {
		if vs != nil && vs.SecretKeyRef != nil {
			add(vs.SecretKeyRef.Name)
		}
	}
	sort.Strings(names)
	return names
}

func databaseReferencesSecret(conn v1alpha1.JDBCConnection, name string) bool {
	for _, n := range connectionSecretNames(conn) {
		if n == name {
			return true
		}
	}
	return false
}

// computeConnectionSecretsHash hashes the data content of the referenced
// connection Secrets, so a credential change is detectable while a metadata-only
// edit (a label/annotation) is not. Missing Secrets contribute their name only.
func (r *IdentityServerDatabaseReconciler) computeConnectionSecretsHash(ctx context.Context, conn v1alpha1.JDBCConnection, namespace string) (string, error) {
	names := connectionSecretNames(conn)
	if len(names) == 0 {
		return "", nil
	}
	h := sha256.New()
	for _, name := range names {
		h.Write([]byte(name))
		h.Write([]byte{0})
		var s corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &s); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("getting Secret %q for connection hash: %w", name, err)
		}
		keys := make([]string, 0, len(s.Data))
		for k := range s.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write(s.Data[k])
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// buildDatabaseJob constructs the one-shot Job that runs `idsvr -I`.
func buildDatabaseJob(db *v1alpha1.IdentityServerDatabase, cluster *v1alpha1.IdentityServerCluster, image, hash, connHash string) *batchv1.Job {
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
	podLabels[databaseLabel] = db.Name
	podLabels[componentLabel] = databaseComponentValue

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      db.Name + databaseJobSuffix,
			Namespace: db.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "curity-operator",
				databaseLabel:                  db.Name,
				componentLabel:                 databaseComponentValue,
			},
			Annotations: map[string]string{
				databaseJobHashAnnotation:       hash,
				connectionSecretsHashAnnotation: connHash,
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

// buildJDBCEnvFrom loads the whole connection Secret via envFrom when secretRef
// is set. Optional, so missing keys are tolerated.
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

// buildJDBCEnvVars maps the per-field connection settings to env vars, appended
// after envFrom so an explicit field overrides the matching secretRef key.
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
	if vs.SecretKeyRef != nil {
		e.ValueFrom = &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: vs.SecretKeyRef.Name},
			Key:                  vs.SecretKeyRef.Key,
		}}
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

// findDatabasesForCluster enqueues the databases referencing a changed cluster,
// so a version/image bump re-triggers the Job.
func (r *IdentityServerDatabaseReconciler) findDatabasesForCluster(ctx context.Context, obj client.Object) []ctrl.Request {
	cluster, ok := obj.(*v1alpha1.IdentityServerCluster)
	if !ok {
		return nil
	}
	var list v1alpha1.IdentityServerDatabaseList
	if err := r.List(ctx, &list, client.InNamespace(cluster.Namespace)); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to list databases for cluster watch; an image change may not re-trigger the init Job until the next resync",
			"cluster", cluster.Name, "namespace", cluster.Namespace)
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

// findDatabasesForConnectionSecret enqueues the databases referencing a changed
// connection Secret, so a fixed Secret re-runs a failed init Job.
func (r *IdentityServerDatabaseReconciler) findDatabasesForConnectionSecret(ctx context.Context, obj client.Object) []ctrl.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	var list v1alpha1.IdentityServerDatabaseList
	if err := r.List(ctx, &list, client.InNamespace(secret.Namespace)); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to list databases for connection-secret watch",
			"secret", secret.Name, "namespace", secret.Namespace)
		return nil
	}
	var reqs []ctrl.Request
	for i := range list.Items {
		if databaseReferencesSecret(list.Items[i].Spec.Connection, secret.Name) {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKey{
				Name:      list.Items[i].Name,
				Namespace: list.Items[i].Namespace,
			}})
		}
	}
	return reqs
}

// findDatabaseForPod enqueues the owning database of an init Pod, so a stuck pod
// re-triggers a reconcile that surfaces the cause.
func (r *IdentityServerDatabaseReconciler) findDatabaseForPod(_ context.Context, obj client.Object) []ctrl.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Labels[componentLabel] != databaseComponentValue {
		return nil
	}
	name, ok := pod.Labels[databaseLabel]
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: name, Namespace: pod.Namespace}}}
}

// findDatabaseForJobFailedCreateEvent maps a FailedCreate Event on a Job to its
// owning database via the Job's curity.io/database label.
func (r *IdentityServerDatabaseReconciler) findDatabaseForJobFailedCreateEvent(ctx context.Context, obj client.Object) []ctrl.Request {
	ev, ok := obj.(*corev1.Event)
	if !ok {
		return nil
	}
	var job batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Name: ev.InvolvedObject.Name, Namespace: ev.InvolvedObject.Namespace}, &job); err != nil {
		return nil
	}
	name, ok := job.Labels[databaseLabel]
	if !ok {
		return nil
	}
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: name, Namespace: job.Namespace}}}
}

// latestJobFailedCreateMessage returns the newest cached FailedCreate Event
// message for the Job UID, or "" if none. A List error is returned (not
// swallowed) so the caller requeues rather than reporting no cause.
func (r *IdentityServerDatabaseReconciler) latestJobFailedCreateMessage(ctx context.Context, jobUID types.UID, namespace string) (string, error) {
	var events corev1.EventList
	if err := r.List(ctx, &events, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("failed to list FailedCreate events for Job %q: %w", jobUID, err)
	}
	var newest *corev1.Event
	for i := range events.Items {
		ev := &events.Items[i]
		if ev.InvolvedObject.UID != jobUID {
			continue
		}
		if newest == nil {
			newest = ev
			continue
		}
		newestStamp := newest.LastTimestamp
		if newestStamp.IsZero() {
			newestStamp = metav1.NewTime(newest.EventTime.Time)
		}
		evStamp := ev.LastTimestamp
		if evStamp.IsZero() {
			evStamp = metav1.NewTime(ev.EventTime.Time)
		}
		if evStamp.After(newestStamp.Time) {
			newest = ev
		}
	}
	if newest == nil {
		return "", nil
	}
	return newest.Message, nil
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
		Owns(&batchv1.Job{}, builder.WithPredicates(jobStatusChangedPredicate{})).
		Watches(
			&v1alpha1.IdentityServerCluster{},
			handler.EnqueueRequestsFromMapFunc(r.findDatabasesForCluster),
			// Only the cluster's resolved image (a spec change) affects us; skip
			// the frequent status-only writes.
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.findDatabaseForPod),
			builder.WithPredicates(databaseInitPodChangedPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findDatabasesForConnectionSecret),
			// Opaque only and only on a .data change, so SA-token/TLS churn and
			// metadata-only edits are ignored; the mapfunc confirms the reference.
			builder.WithPredicates(connectionSecretChangedPredicate{}),
		).
		Watches(
			// FailedCreate Events surface a pod-admission denial after Job create.
			&corev1.Event{},
			handler.EnqueueRequestsFromMapFunc(r.findDatabaseForJobFailedCreateEvent),
			builder.WithPredicates(jobFailedCreateEventPredicate{}),
		).
		Complete(r)
}
