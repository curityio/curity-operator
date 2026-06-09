package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

// IdentityServerClusterReconciler reconciles an IdentityServerCluster object.
// It aggregates status from child IdentityServerNode resources, manages
// the admin credentials secret, and orchestrates cluster.xml generation.
type IdentityServerClusterReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Recorder   record.EventRecorder
	RestConfig *rest.Config
}

// +kubebuilder:rbac:groups=curity.io,resources=identityserverclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=curity.io,resources=identityserverclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=curity.io,resources=identityserverclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=curity.io,resources=identityservernodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get

// Reconcile handles a single reconciliation loop for an IdentityServerCluster.
func (r *IdentityServerClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// 1. Fetch the IdentityServerCluster
	var cluster v1alpha1.IdentityServerCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get IdentityServerCluster: %w", err)
	}

	// 2. Handle finalizer. The finalizer blocks cluster deletion while any
	// child IdentityServerNode still exists, so `kubectl delete cluster`
	// waits for nodes to be removed first. OwnerReferences on children give
	// additional cascade safety under Foreground propagation, but are not
	// what enforces the blocking ordering — the finalizer is.
	if cluster.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(&cluster, v1alpha1.ClusterFinalizer) {
			childNodes, err := r.listChildNodes(ctx, &cluster)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to list child nodes: %w", err)
			}
			if len(childNodes) > 0 {
				names := make([]string, 0, len(childNodes))
				for i := range childNodes {
					names = append(names, childNodes[i].Name)
				}
				log.Info("cluster deletion blocked by existing nodes", "nodes", names)
				r.Recorder.Eventf(&cluster, corev1.EventTypeWarning, "DeletionBlocked",
					"Cannot delete cluster: nodes still exist: %s", strings.Join(names, ", "))
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			controllerutil.RemoveFinalizer(&cluster, v1alpha1.ClusterFinalizer)
			if err := r.Update(ctx, &cluster); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
			}
			log.Info("cluster finalizer removed, deletion proceeding")
		}
		return ctrl.Result{}, nil
	}

	// Ensure the finalizer is present on live clusters (rationale above).
	if !controllerutil.ContainsFinalizer(&cluster, v1alpha1.ClusterFinalizer) {
		controllerutil.AddFinalizer(&cluster, v1alpha1.ClusterFinalizer)
		if err := r.Update(ctx, &cluster); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 3. Ensure admin credentials secret. Capture whether the name was
	// defaulted before the spec is mutated below.
	defaulted := cluster.Spec.AdminCredentials == nil
	if defaulted {
		cluster.Spec.AdminCredentials = defaultAdminCredentials(cluster.Name)
		log.Info("defaulting adminCredentials", "secretName", cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name)
	}
	if err := r.ensureAdminCredentialsSecret(ctx, &cluster, defaulted); err != nil {
		if res, handled, helperErr := r.handleAdminCredsSecretMissing(ctx, &cluster, err); handled {
			return res, helperErr
		}
		if res, handled, helperErr := r.handlePermanentWriteError(ctx, &cluster, err); handled {
			return res, helperErr
		}
		return ctrl.Result{}, err
	}

	// 4. List child IdentityServerNodes (reused by cluster config and status)
	childNodes, err := r.listChildNodes(ctx, &cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list child nodes: %w", err)
	}

	// 5. Ensure cluster config (cluster.xml via genclust Job)
	if err := r.ensureClusterConfig(ctx, &cluster, childNodes); err != nil {
		// Pre-flight dry-run Forbidden: SCC/PodSecurity/quota/webhook denied
		// the pod template. Surface as JobPodAdmissionForbidden with 5-minute
		// requeue — there is no Pod-Create event to wake us when the user
		// fixes the underlying issue.
		if res, handled, helperErr := r.handleAdmissionForbidden(ctx, &cluster, err); handled {
			return res, helperErr
		}
		if res, handled, helperErr := r.handlePermanentWriteError(ctx, &cluster, err); handled {
			return res, helperErr
		}
		return ctrl.Result{}, err
	}
	// If logs aren't readable yet, requeue with backoff instead of relying
	// on the implicit status-update loop which creates a hot polling cycle.
	if configCond := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady); configCond != nil && configCond.Reason == "LogsUnavailable" {
		// Drop the obsolete ConfigScopeIssues condition on the only path
		// that persists Status without the wholesale rebuild below. Remove
		// once CRs last reconciled before commit d18a0a0 are re-reconciled.
		if apimeta.RemoveStatusCondition(&cluster.Status.Conditions, "ConfigScopeIssues") {
			log.V(1).Info("removed obsolete ConfigScopeIssues condition during LogsUnavailable cleanup")
		}
		if err := r.Status().Update(ctx, &cluster); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to update cluster status: %w", err)
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// 6. Default config-type annotations, surface scope issues, discover managed configs
	if err := r.ensureManagedConfigDiscovery(ctx, &cluster, findAdminNodeName(childNodes) != ""); err != nil {
		return ctrl.Result{}, err
	}

	// 7. Compute and update status
	readyCount := int32(0)
	for i := range childNodes {
		readyCond := apimeta.FindStatusCondition(childNodes[i].Status.Conditions, v1alpha1.ConditionReady)
		if readyCond != nil && readyCond.Status == metav1.ConditionTrue {
			readyCount++
		}
	}

	// Preserve ClusterConfigReady before computeClusterConditions overwrites.
	configReadyCond := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
	conditions := computeClusterConditions(childNodes, cluster.Generation)
	cluster.Status.Conditions = conditions
	if configReadyCond != nil {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, *configReadyCond)
	}
	// ManagedConfigsValid is derived from cluster-level managedResourceIssues
	// (not child nodes), so it's applied here rather than in computeClusterConditions.
	// Omitted when there are no issues (the rebuild above drops any prior False).
	if mc, set := aggregateManagedConfigsValid(cluster.Status.ManagedResourceIssues); set {
		setCondition(&cluster.Status.Conditions, v1alpha1.ConditionManagedConfigsValid,
			mc.Status, mc.Reason, mc.Message, cluster.Generation)
	}
	cluster.Status.ObservedGeneration = cluster.Generation
	cluster.Status.NodeCount = int32(len(childNodes))
	cluster.Status.ReadyNodes = readyCount
	cluster.Status.Version = cluster.Spec.Version

	if err := r.Status().Update(ctx, &cluster); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to update cluster status: %w", err)
	}

	return ctrl.Result{}, nil
}

// listChildNodes returns all IdentityServerNodes that reference this cluster
// by spec OR by controller-ownerRef.UID. The dual-leg filter prevents a
// cascade-deletion race where the user concurrently deletes this cluster and
// patches a child node's spec.identityServerClusterRef to a peer: the spec
// leg returns 0 (spec already moved), but the UID leg keeps the node counted
// until the node reconciler migrates the ownerRef to the new cluster — so
// the finalizer holds long enough for the migration to complete and K8s GC
// does not cascade-delete the node via stale ownerRef.UID.
//
// The spec leg is retained as a fallback for newly created nodes that have
// not yet been reconciled and therefore have no ownerRef.
//
// See Tekton GHSA-w2h3-vvvq-3m53 for the same bug class fixed via UID-based
// child matching; Cluster API's MachineSet controller uses the same pattern.
func (r *IdentityServerClusterReconciler) listChildNodes(ctx context.Context, cluster *v1alpha1.IdentityServerCluster) ([]v1alpha1.IdentityServerNode, error) {
	var nodeList v1alpha1.IdentityServerNodeList
	if err := r.List(ctx, &nodeList, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}

	var children []v1alpha1.IdentityServerNode
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		if node.Spec.IdentityServerClusterRef.Name == cluster.Name || metav1.IsControlledBy(node, cluster) {
			children = append(children, *node)
		}
	}
	return children, nil
}

// ensureAdminCredentialsSecret creates the Secret with random values when the
// name was defaulted. A missing user-provided name returns
// errAdminCredsSecretMissing so the operator waits rather than fabricating one.
// Never overwrites an existing Secret.
func (r *IdentityServerClusterReconciler) ensureAdminCredentialsSecret(ctx context.Context, cluster *v1alpha1.IdentityServerCluster, defaulted bool) error {
	log := ctrl.LoggerFrom(ctx)
	secretName := cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name

	var existing corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, &existing)
	if err == nil {
		// Secret already exists — do nothing
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check admin credentials secret: %w", err)
	}

	// A valid user-provided name is an explicit reference another controller
	// will create — wait, don't fabricate. An invalid name can never exist, so
	// fall through to Create and surface the apiserver's rejection as InvalidSpec.
	if !defaulted && isValidSecretName(secretName) {
		return errAdminCredsSecretMissing
	}

	// Generate random values
	adminPassword, err := generateRandomAlphanumeric(16)
	if err != nil {
		return fmt.Errorf("failed to generate admin password: %w", err)
	}
	encryptionKey, err := generateRandomHex(32)
	if err != nil {
		return fmt.Errorf("failed to generate encryption key: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "curity-operator",
				"curity.io/cluster":            cluster.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ADMIN_PASSWORD":        []byte(adminPassword),
			"CONFIG_ENCRYPTION_KEY": []byte(encryptionKey),
		},
	}

	// NOTE: Intentionally no OwnerReference — secret survives cluster deletion
	if err := r.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // Race condition: another reconcile created it
		}
		return fmt.Errorf("failed to create admin credentials secret: %w", err)
	}

	log.Info("created admin credentials secret", "name", secretName)
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "SecretCreated",
		"Created admin credentials secret %q with auto-generated values", secretName)

	return nil
}

// defaultAdminCredentials returns a CredentialsSource with conventional naming
// when the user does not specify adminCredentials on the cluster spec.
func defaultAdminCredentials(clusterName string) *v1alpha1.CredentialsSource {
	return &v1alpha1.CredentialsSource{
		ValueFrom: v1alpha1.CredentialsValueFrom{
			SecretKeyRef: v1alpha1.SecretKeyRefSource{
				Name: clusterName + "-admin-creds",
				Items: []v1alpha1.KeyToPath{
					{Key: "ADMIN_PASSWORD", Path: "PASSWORD"},
					{Key: "CONFIG_ENCRYPTION_KEY", Path: "CONFIG_ENCRYPTION_KEY"},
				},
			},
		},
	}
}

// SetupWithManager registers the controller with the manager.
func (r *IdentityServerClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.IdentityServerCluster{},
			builder.WithPredicates(predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.LabelChangedPredicate{},
				predicate.AnnotationChangedPredicate{},
			))).
		Watches(
			&v1alpha1.IdentityServerNode{},
			// Union old+new clusterRef on Update events so the abandoned
			// cluster reconciles when a node moves a→b — the bare
			// EnqueueRequestsFromMapFunc would only see e.ObjectNew.
			newUnionScopeHandler(r.findClusterForNode),
			builder.WithPredicates(nodeStatusConditionsChangedPredicate{}),
		).
		Watches(
			&batchv1.Job{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterForJob),
		).
		Watches(
			&corev1.Event{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterForJobFailedCreateEvent),
			builder.WithPredicates(jobFailedCreateEventPredicate{}),
		).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterForJobPod),
			builder.WithPredicates(clusterConfigPodChangedPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterForSecret),
			builder.WithPredicates(adminCredsCandidatePredicate{}),
		).
		Watches(
			&corev1.ConfigMap{},
			newClusterManagedConfigHandler(r.findClustersForManagedConfig),
			builder.WithPredicates(managedConfigPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			newClusterManagedConfigHandler(r.findClustersForManagedConfig),
			builder.WithPredicates(managedConfigPredicate{}),
		).
		Complete(r)
}

// findClusterForNode maps an IdentityServerNode change to reconcile requests
// for the cluster pointed at by spec.identityServerClusterRef and (when set
// and different) the cluster pointed at by the controller-ownerRef. The
// ownerRef leg is what wakes an abandoned cluster up when the node reconciler
// migrates a child node's ownerRef to a peer — the union scope handler then
// has the abandoned cluster in its old/new union so the deletion gate runs.
func (r *IdentityServerClusterReconciler) findClusterForNode(ctx context.Context, obj client.Object) []ctrl.Request {
	node, ok := obj.(*v1alpha1.IdentityServerNode)
	if !ok {
		return nil
	}

	clusterRef := node.Spec.IdentityServerClusterRef
	requests := []ctrl.Request{
		{NamespacedName: client.ObjectKey{Name: clusterRef.Name, Namespace: node.Namespace}},
	}

	if owner := metav1.GetControllerOf(node); owner != nil &&
		owner.APIVersion == v1alpha1.GroupVersion.String() &&
		owner.Kind == "IdentityServerCluster" &&
		owner.Name != clusterRef.Name {
		requests = append(requests, ctrl.Request{
			NamespacedName: client.ObjectKey{Name: owner.Name, Namespace: node.Namespace},
		})
	}

	return requests
}

// --- Cluster config (cluster.xml) generation via genclust Job ---

const (
	clusterConfigSecretSuffix = "-cluster-config"
	clusterConfigJobSuffix    = "-cluster-config-job"
	jobBackoffLimit           = int32(3)
	clusterConfigKey          = "cluster.xml"
	clusterConfigPlaceholder  = "placeholder"
)

// clusterConfigSecretName returns the deterministic Secret name for a cluster's config.
func clusterConfigSecretName(clusterName string) string {
	return clusterName + clusterConfigSecretSuffix
}

// findAdminNodeName returns the name of the first admin node, or "" if none exists.
func findAdminNodeName(nodes []v1alpha1.IdentityServerNode) string {
	for i := range nodes {
		if nodes[i].Spec.Type == v1alpha1.NodeTypeAdmin {
			return nodes[i].Name
		}
	}
	return ""
}

// findAdminConfigPort returns the admin node's resolved config-service port, or
// the default when there's no admin node. Threaded into genclust so cluster.xml
// (and the runtime nodes that read it) agree with the admin Service's config port.
func findAdminConfigPort(nodes []v1alpha1.IdentityServerNode) int32 {
	for i := range nodes {
		if nodes[i].Spec.Type == v1alpha1.NodeTypeAdmin {
			return resolveConfigPort(&nodes[i])
		}
	}
	return portConfig
}

// isClusterConfigReady returns true if the Secret contains real cluster.xml data.
func isClusterConfigReady(secret *corev1.Secret) bool {
	data, ok := secret.Data[clusterConfigKey]
	return ok && len(data) > 0 && string(data) != clusterConfigPlaceholder
}

// computeEncryptionKeyHash returns the SHA256 hex of CONFIG_ENCRYPTION_KEY, or
// ("", nil) when there's no key. A read error returns ("", err) so callers requeue
// rather than create an un-annotated Job that strands key-drift recovery.
func (r *IdentityServerClusterReconciler) computeEncryptionKeyHash(ctx context.Context, cluster *v1alpha1.IdentityServerCluster) (string, error) {
	if cluster.Spec.AdminCredentials == nil {
		return "", nil
	}
	secretName := cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name
	var credSecret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, &credSecret); err != nil {
		return "", err
	}
	key, ok := credSecret.Data["CONFIG_ENCRYPTION_KEY"]
	if !ok || len(key) == 0 {
		return "", nil
	}
	h := sha256.Sum256(key)
	return hex.EncodeToString(h[:]), nil
}

// computeClusterConfigHash hashes the inputs that trigger a genclust re-run:
// image, imagePullSecret, admin node name, packages, and a non-default config
// port. Packages/config-port append only when non-default, so existing clusters
// keep the same hash (no upgrade regen). The encryption key is omitted — a
// first-reconcile cache miss would spuriously regen (Branch A).
func computeClusterConfigHash(cluster *v1alpha1.IdentityServerCluster, adminNodeName string, configPort int32) string {
	var b strings.Builder
	const sep = "\x00"
	b.WriteString("v1")
	b.WriteString(sep)
	b.WriteString(buildImage(cluster))
	b.WriteString(sep)
	b.WriteString(cluster.Spec.ImagePullSecret)
	b.WriteString(sep)
	b.WriteString(adminNodeName)
	if pkgHash := computePackagesHash(cluster.Spec.Packages); pkgHash != "" {
		b.WriteString(sep)
		b.WriteString(pkgHash)
	}
	if configPort != portConfig {
		b.WriteString(sep)
		b.WriteString(fmt.Sprintf("cfgport=%d", configPort))
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])
}

// buildClusterConfigSecret creates the Secret that holds cluster.xml.
func buildClusterConfigSecret(cluster *v1alpha1.IdentityServerCluster, clusterXML []byte, adminNodeName, encryptionKeyHash, configHash string) *corev1.Secret {
	data := []byte(clusterConfigPlaceholder)
	if len(clusterXML) > 0 {
		data = clusterXML
	}
	annotations := map[string]string{
		"argocd.argoproj.io/compare-options": "IgnoreExtraneous",
	}
	if adminNodeName != "" {
		annotations["curity.io/admin-node"] = adminNodeName
	}
	if encryptionKeyHash != "" {
		annotations["curity.io/encryption-key-hash"] = encryptionKeyHash
	}
	if configHash != "" {
		annotations["curity.io/cluster-config-hash"] = configHash
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterConfigSecretName(cluster.Name),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "curity-operator",
				"curity.io/cluster":            cluster.Name,
				"curity.io/component":          "cluster-config",
			},
			Annotations: annotations,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			clusterConfigKey: data,
		},
	}
}

// dryRunPodAdmission runs the apiserver's full admission chain (SCC,
// PodSecurity, ResourceQuota, validating webhooks) against the Job's pod
// template without persisting anything. Returns the apiserver error verbatim
// — IsForbidden on SCC/PSA/quota denial, IsInvalid on spec violations, or
// transient errors that bubble.
//
// Uses a deterministic Name (not GenerateName) so the apiserver's Forbidden
// message text is stable across reconciles — the pod name appears in the
// error message and a randomized name would defeat the condition-stability
// gate that prevents Event spam.
func (r *IdentityServerClusterReconciler) dryRunPodAdmission(
	ctx context.Context, cluster *v1alpha1.IdentityServerCluster, job *batchv1.Job,
) error {
	dryRunPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        job.Name + "-dryrun-preflight",
			Namespace:   cluster.Namespace,
			Labels:      job.Spec.Template.Labels,
			Annotations: job.Spec.Template.Annotations,
		},
		Spec: *job.Spec.Template.Spec.DeepCopy(),
	}
	dryRunPod.Spec.RestartPolicy = corev1.RestartPolicyNever
	return r.Create(ctx, dryRunPod, client.DryRunAll)
}

// jobCompletionTime returns the time the Job completed, or nil if not yet complete.
func jobCompletionTime(job *batchv1.Job) *metav1.Time {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return &cond.LastTransitionTime
		}
	}
	return nil
}

// buildClusterConfigJob creates the Job that runs genclust to generate cluster.xml.
// configHash is stamped as an annotation so step 4 of ensureClusterConfig can
// detect input drift (spec change while a Job is in flight) and recreate.
func buildClusterConfigJob(cluster *v1alpha1.IdentityServerCluster, adminNodeName, configHash string, configPort int32) *batchv1.Job {
	backoff := jobBackoffLimit
	annotations := map[string]string{}
	if configHash != "" {
		annotations["curity.io/cluster-config-hash"] = configHash
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name + clusterConfigJobSuffix,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "curity-operator",
				"curity.io/cluster":            cluster.Name,
				"curity.io/component":          "cluster-config",
			},
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			// Let infra disruptions retry without counting; fail fast on a genclust
			// logic error. Silently dropped on clusters that predate podFailurePolicy.
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
							ContainerName: ptr.To("genclust"),
							Operator:      batchv1.PodFailurePolicyOnExitCodesOpIn,
							Values:        []int32{1},
						},
					},
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"curity.io/cluster":   cluster.Name,
						"curity.io/component": "cluster-config",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:  ptr.To(int64(10001)),
						RunAsGroup: ptr.To(int64(10000)),
						FSGroup:    ptr.To(int64(10000)),
					},
					Containers: []corev1.Container{
						{
							Name:  "genclust",
							Image: buildImage(cluster),
							// Surface genclust's log tail in terminated.message so the
							// failure cause is readable from status without streaming logs.
							TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
							Command: []string{
								"/bin/sh", "-c",
								"/opt/idsvr/bin/genclust -c $CONFIG_SERVICE_HOST -p $CONFIG_SERVICE_PORT",
							},
							Env: []corev1.EnvVar{
								{Name: "CONFIG_SERVICE_HOST", Value: OwnedResourceName(cluster.Name, adminNodeName)},
								{Name: "CONFIG_SERVICE_PORT", Value: fmt.Sprintf("%d", configPort)},
							},
						},
					},
				},
			},
		},
	}

	// Add CONFIG_ENCRYPTION_KEY from admin credentials if configured
	if cluster.Spec.AdminCredentials != nil {
		secretName := cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name
		job.Spec.Template.Spec.Containers[0].Env = append(
			job.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{
				Name: "CONFIG_ENCRYPTION_KEY",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "CONFIG_ENCRYPTION_KEY",
						Optional:             ptr.To(true),
					},
				},
			},
		)
	}

	// Inherit ImagePullSecret from cluster
	if cluster.Spec.ImagePullSecret != "" {
		job.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: cluster.Spec.ImagePullSecret},
		}
	}

	// Inherit scheduling constraints from cluster (Scenario 10)
	if cluster.Spec.NodeSelector != nil {
		job.Spec.Template.Spec.NodeSelector = cluster.Spec.NodeSelector
	}
	if len(cluster.Spec.Tolerations) > 0 {
		job.Spec.Template.Spec.Tolerations = cluster.Spec.Tolerations
	}
	if cluster.Spec.Affinity != nil {
		job.Spec.Template.Spec.Affinity = cluster.Spec.Affinity
	}
	if len(cluster.Spec.TopologySpreadConstraints) > 0 {
		job.Spec.Template.Spec.TopologySpreadConstraints = cluster.Spec.TopologySpreadConstraints
	}

	return job
}

// ensureClusterConfig orchestrates cluster.xml generation via a genclust Job.
// It creates a placeholder Secret, runs the Job, reads the output from pod logs,
// and populates the Secret with the real cluster.xml content.
//
// Decision tree once the Secret has real cluster.xml data
// (gated on isClusterConfigReady, evaluated in source order):
//
//   - Branch A      — encryption-key rotation. Stored key hash differs from
//     current. Full regen. Empty-guards tolerate the credentials-
//     Secret cache miss on first reconcile.
//   - Steady state  — storedConfigHash matches current. Zero API calls.
//   - Backfill      — storedConfigHash empty but storedAdmin matches. Pre-PR
//     Secret; stamp the new annotation without regen.
//   - Branch B      — only adminNodeName changed. Rewrite <host> in XML
//     in-place; falls through to Branch C on malformed XML.
//   - Branch C      — anything else (multi-input change). Full regen.
//
// When the Secret is in placeholder state (Job in flight or never ran), step 4
// below ("input drift") detects spec changes that landed after Job creation
// and recreates the Job against the latest spec. Branches A/B/C never run in
// that state.
func (r *IdentityServerClusterReconciler) ensureClusterConfig(ctx context.Context, cluster *v1alpha1.IdentityServerCluster, childNodes []v1alpha1.IdentityServerNode) error {
	log := ctrl.LoggerFrom(ctx)
	secretName := clusterConfigSecretName(cluster.Name)
	jobName := cluster.Name + clusterConfigJobSuffix

	// 1. Find admin node
	adminNodeName := findAdminNodeName(childNodes)
	if adminNodeName == "" {
		if err := r.announceAdminDeparted(ctx, cluster); err != nil {
			return err
		}
		setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
			metav1.ConditionFalse, "WaitingForAdmin",
			"No admin node found for this cluster", cluster.Generation)
		return nil
	}
	adminConfigPort := findAdminConfigPort(childNodes)

	// 2. Check if Secret exists and is ready
	var configSecret corev1.Secret
	secretExists := false
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, &configSecret); err == nil {
		secretExists = true
		if isClusterConfigReady(&configSecret) {
			currentKeyHash, hashErr := r.computeEncryptionKeyHash(ctx, cluster)
			if hashErr != nil {
				return fmt.Errorf("reading encryption key for rotation check: %w", hashErr)
			}
			currentConfigHash := computeClusterConfigHash(cluster, adminNodeName, adminConfigPort)
			storedConfigHash := configSecret.Annotations["curity.io/cluster-config-hash"]
			storedAdmin := configSecret.Annotations["curity.io/admin-node"]

			// Branch A: encryption-key rotation. Empty-guards on both hashes
			// tolerate the credentials-Secret cache miss on first reconcile.
			if storedHash, ok := configSecret.Annotations["curity.io/encryption-key-hash"]; ok && storedHash != "" && currentKeyHash != "" && storedHash != currentKeyHash {
				log.Info("encryption key changed, resetting cluster config for regeneration")

				setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
					metav1.ConditionFalse, "Regenerating",
					"Encryption key rotated; regenerating cluster.xml", cluster.Generation)
				if err := r.Status().Update(ctx, cluster); err != nil {
					return fmt.Errorf("failed to flip ClusterConfigReady=False for key rotation: %w", err)
				}

				configSecret.Data[clusterConfigKey] = []byte(clusterConfigPlaceholder)
				configSecret.Annotations["curity.io/encryption-key-hash"] = currentKeyHash
				configSecret.Annotations["curity.io/cluster-config-hash"] = currentConfigHash
				if err := r.Update(ctx, &configSecret); err != nil {
					return fmt.Errorf("failed to reset cluster config for key rotation: %w", err)
				}
				var oldJob batchv1.Job
				if err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: cluster.Namespace}, &oldJob); err == nil {
					if delErr := r.Delete(ctx, &oldJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
						return fmt.Errorf("failed to delete stale Job during key rotation: %w", delErr)
					}
				}
				r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ClusterConfigRegenerating",
					"Encryption key rotated; regenerating cluster.xml via genclust Job")
				return nil
			}

			// Steady state — zero API calls.
			if storedConfigHash != "" && storedConfigHash == currentConfigHash {
				cluster.Status.ClusterConfigSecretName = secretName
				setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
					metav1.ConditionTrue, "SecretReady",
					"Cluster config secret is populated", cluster.Generation)
				return nil
			}

			// Backfill — pre-PR Secret with admin match. Stamp without regen.
			if storedConfigHash == "" && storedAdmin == adminNodeName {
				configSecret.Annotations["curity.io/cluster-config-hash"] = currentConfigHash
				if err := r.Update(ctx, &configSecret); err != nil {
					return fmt.Errorf("failed to backfill cluster-config-hash: %w", err)
				}
				cluster.Status.ClusterConfigSecretName = secretName
				setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
					metav1.ConditionTrue, "SecretReady",
					"Cluster config secret is populated", cluster.Generation)
				return nil
			}

			// Branch B: cheap admin-rename. Contains-guard prevents
			// strings.Replace from silently no-oping on malformed XML.
			if storedConfigHash != "" && storedAdmin != "" && storedAdmin != adminNodeName {
				hashWithOldAdmin := computeClusterConfigHash(cluster, storedAdmin, adminConfigPort)
				if storedConfigHash == hashWithOldAdmin {
					oldHost := OwnedResourceName(cluster.Name, storedAdmin)
					newHost := OwnedResourceName(cluster.Name, adminNodeName)
					oldXML := string(configSecret.Data[clusterConfigKey])
					oldHostTag := "<host>" + oldHost + "</host>"
					if !strings.Contains(oldXML, oldHostTag) {
						log.Info("admin rename: XML missing expected <host> tag, falling through to full regen",
							"old", storedAdmin, "new", adminNodeName)
					} else {
						log.Info("admin node changed, updating cluster config in-place",
							"old", storedAdmin, "new", adminNodeName)
						newXML := strings.Replace(oldXML, oldHostTag, "<host>"+newHost+"</host>", 1)
						configSecret.Data[clusterConfigKey] = []byte(newXML)
						configSecret.Annotations["curity.io/admin-node"] = adminNodeName
						configSecret.Annotations["curity.io/cluster-config-hash"] = currentConfigHash
						if err := r.Update(ctx, &configSecret); err != nil {
							return fmt.Errorf("failed to update cluster config for admin rename: %w", err)
						}
						cluster.Status.ClusterConfigSecretName = secretName
						setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
							metav1.ConditionTrue, "SecretReady",
							"Cluster config secret is populated", cluster.Generation)
						return nil
					}
				}
				// More than just admin changed — fall through to full regen.
			}

			// Branch C: full regen. Status flips to False BEFORE the Secret
			// data reset — otherwise a concurrent node reconcile would stamp
			// sha256("placeholder") onto Deployment templates.
			log.Info("cluster.xml inputs changed, regenerating",
				"stored", storedConfigHash, "current", currentConfigHash)

			setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
				metav1.ConditionFalse, "Regenerating",
				"Cluster.xml inputs changed; regenerating", cluster.Generation)
			if err := r.Status().Update(ctx, cluster); err != nil {
				return fmt.Errorf("failed to flip ClusterConfigReady=False for regen: %w", err)
			}

			configSecret.Data[clusterConfigKey] = []byte(clusterConfigPlaceholder)
			configSecret.Annotations["curity.io/cluster-config-hash"] = currentConfigHash
			configSecret.Annotations["curity.io/admin-node"] = adminNodeName
			if currentKeyHash != "" {
				configSecret.Annotations["curity.io/encryption-key-hash"] = currentKeyHash
			}
			if err := r.Update(ctx, &configSecret); err != nil {
				return fmt.Errorf("failed to reset cluster config for regen: %w", err)
			}

			var oldJob batchv1.Job
			if err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: cluster.Namespace}, &oldJob); err == nil {
				if delErr := r.Delete(ctx, &oldJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
					return fmt.Errorf("failed to delete stale Job during regen: %w", delErr)
				}
			} else if !apierrors.IsNotFound(err) {
				log.Error(err, "stale Job lookup failed during regen; will retry")
				return fmt.Errorf("failed to look up stale Job during regen: %w", err)
			}

			r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ClusterConfigRegenerating",
				"Cluster.xml inputs changed; regenerating via genclust Job")
			// Fall through to Job creation below.
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get cluster config secret: %w", err)
	}

	// 3. Ensure placeholder Secret exists
	if !secretExists {
		keyHash, hashErr := r.computeEncryptionKeyHash(ctx, cluster)
		if hashErr != nil {
			return fmt.Errorf("reading encryption key for placeholder Secret: %w", hashErr)
		}
		placeholder := buildClusterConfigSecret(cluster, nil, adminNodeName, keyHash, computeClusterConfigHash(cluster, adminNodeName, adminConfigPort))
		if err := r.Create(ctx, placeholder); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create placeholder cluster config secret: %w", err)
			}
		} else {
			log.Info("created placeholder cluster config secret", "name", secretName)
		}
	}

	// 4. Check for existing Job
	var job batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: cluster.Namespace}, &job); err == nil {
		// Detect input drift: spec changed while a Job was already in flight.
		// Branches A/B/C only run when the Secret has real data; in placeholder
		// state, a stale Job would otherwise run to completion (or burn through
		// BackoffLimit) with the old inputs. Done before the completion check
		// so we never populate the Secret with cluster.xml from stale inputs
		// (e.g. wrong admin host baked into <host>).
		jobConfigHash := job.Annotations["curity.io/cluster-config-hash"]
		currentConfigHash := computeClusterConfigHash(cluster, adminNodeName, adminConfigPort)
		jobKeyHash := job.Annotations["curity.io/encryption-key-hash"]
		currentKeyHash, hashErr := r.computeEncryptionKeyHash(ctx, cluster)
		if hashErr != nil {
			return fmt.Errorf("reading encryption key for drift check: %w", hashErr)
		}
		// Empty-guard both hashes so a pre-annotation Job (operator upgrade) isn't
		// spuriously recreated. The key-hash arm recovers a failed cluster after a fix.
		configDrift := jobConfigHash != "" && jobConfigHash != currentConfigHash
		keyDrift := jobKeyHash != "" && jobKeyHash != currentKeyHash
		if configDrift || keyDrift {
			log.Info("input drift detected on in-flight Job, deleting to recreate with current spec",
				"jobConfigHash", jobConfigHash, "currentConfigHash", currentConfigHash,
				"jobKeyHash", jobKeyHash, "currentKeyHash", currentKeyHash)
			if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete drifted Job: %w", err)
			}
			r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ClusterConfigRegenerating",
				"Cluster config inputs changed during Job execution; recreating Job")
			setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
				metav1.ConditionFalse, "Regenerating",
				"Cluster config inputs changed; recreating Job", cluster.Generation)
			return nil
		}

		// Check Job completion
		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				// Job completed — read logs and populate Secret
				clusterXML, err := r.readJobPodLogs(ctx, &job)
				if err != nil {
					// Transient: cache lag, API error — retry in place.
					// If the pod is genuinely gone (>30s since completion), fall back
					// to deleting the Job so a new one gets created.
					if ct := jobCompletionTime(&job); ct != nil && time.Since(ct.Time) > 30*time.Second {
						log.Error(err, "pod logs unavailable after 30s, deleting Job to force recreation")
						if delErr := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
							return fmt.Errorf("failed to delete stale Job: %w", delErr)
						}
					} else {
						log.Error(err, "Job completed but pod logs not yet available, will retry")
					}
					setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
						metav1.ConditionFalse, "LogsUnavailable",
						"Job completed but logs could not be read, retrying", cluster.Generation)
					return nil
				}
				if len(clusterXML) == 0 {
					// Potentially permanent: genclust produced no output.
					// Delete the Job so a new one runs on the next reconcile.
					log.Error(fmt.Errorf("genclust produced empty output"), "empty cluster.xml from Job logs, deleting Job to retry")
					r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "EmptyClusterConfig",
						"genclust Job %q produced empty output, recreating", jobName)
					if delErr := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
						return fmt.Errorf("failed to delete empty-output Job: %w", delErr)
					}
					setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
						metav1.ConditionFalse, "EmptyOutput",
						"genclust Job produced empty cluster.xml, retrying", cluster.Generation)
					return nil
				}

				// Update Secret with real data
				keyHash, hashErr := r.computeEncryptionKeyHash(ctx, cluster)
				if hashErr != nil {
					return fmt.Errorf("reading encryption key for Secret update: %w", hashErr)
				}
				updatedSecret := buildClusterConfigSecret(cluster, clusterXML, adminNodeName, keyHash, computeClusterConfigHash(cluster, adminNodeName, adminConfigPort))
				var existing corev1.Secret
				if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, &existing); err != nil {
					return fmt.Errorf("failed to get cluster config secret for update: %w", err)
				}
				existing.Data = updatedSecret.Data
				// Merge instead of replace, otherwise kubectl.kubernetes.io/last-applied-configuration
				// and external tracking annotations get clobbered every Job completion.
				if existing.Annotations == nil {
					existing.Annotations = map[string]string{}
				}
				for k, v := range updatedSecret.Annotations {
					existing.Annotations[k] = v
				}
				if err := r.Update(ctx, &existing); err != nil {
					return fmt.Errorf("failed to update cluster config secret: %w", err)
				}

				// Clean up Job
				if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
					log.Error(err, "failed to delete completed Job")
				}

				cluster.Status.ClusterConfigSecretName = secretName
				setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
					metav1.ConditionTrue, "SecretReady",
					"Cluster config secret is populated", cluster.Generation)
				log.Info("cluster config generated successfully", "secret", secretName)
				r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ClusterConfigReady",
					"Cluster config secret %q populated via genclust Job", secretName)
				return nil
			}

			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				// Terminal: surface the cause and STOP (no delete+recreate — that
				// oscillated). Recovery is the drift check above when the user fixes
				// spec or the key.
				podCause := ""
				if pods, perr := r.listClusterConfigPods(ctx, cluster.Name, cluster.Namespace); perr != nil {
					log.Error(perr, "listing cluster-config pods for failure cause; using fallback message")
				} else if podCause = extractGenclustFailureMessage(pods); podCause == "" {
					log.Info("no genclust failure cause in pod status; using fallback message")
				}
				// Don't regress the captured cause if the pod is later GC'd.
				existing := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
				msg := chooseJobFailedMessage(podCause, existing, cond.Message)
				changed := apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
					Type:               v1alpha1.ConditionClusterConfigReady,
					Status:             metav1.ConditionFalse,
					Reason:             "JobFailed",
					Message:            msg,
					ObservedGeneration: cluster.Generation,
				})
				if changed {
					log.Info("cluster config Job failed", "message", msg)
					r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "ClusterConfigJobFailed", "%s", msg)
				}
				return nil
			}
		}

		// Job still running — classify pod admission/scheduling state before
		// defaulting to JobRunning so users see SCC/quota/scheduling/imagepull
		// failures within seconds rather than waiting for BackoffLimit.
		pods, listErr := r.listClusterConfigPods(ctx, cluster.Name, cluster.Namespace)
		if listErr != nil {
			log.Error(listErr, "list cluster-config pods failed; falling back to JobRunning")
		} else {
			var failedCreateMsg string
			if len(pods) == 0 {
				failedCreateMsg = r.latestJobFailedCreateMessage(ctx, job.UID, cluster.Namespace)
			}
			res := translateJobPodAdmission(pods, failedCreateMsg)
			if res.set {
				changed := apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
					Type:               v1alpha1.ConditionClusterConfigReady,
					Status:             res.status,
					Reason:             res.reason,
					Message:            res.message,
					ObservedGeneration: cluster.Generation,
				})
				if changed {
					r.Recorder.Eventf(cluster, corev1.EventTypeWarning, res.reason,
						"Cluster config Job pod problem: %s", res.message)
				}
				return nil
			}
		}

		// Job still running (no admission/scheduling failure detected)
		setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
			metav1.ConditionFalse, "JobRunning",
			"genclust Job is running", cluster.Generation)
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get cluster config Job: %w", err)
	}

	// 5. Create the Job. Job has owner ref to cluster CR (GC on delete).
	// cluster-config Secret intentionally does NOT — it outlives cluster
	// deletion so a recreated cluster can re-use the existing keystore.
	{
		newJob := buildClusterConfigJob(cluster, adminNodeName, computeClusterConfigHash(cluster, adminNodeName, adminConfigPort), adminConfigPort)
		// Stamp the key hash so the drift check recreates the Job once the user
		// fixes CONFIG_ENCRYPTION_KEY.
		keyHash, hashErr := r.computeEncryptionKeyHash(ctx, cluster)
		if hashErr != nil {
			return fmt.Errorf("reading encryption key before creating genclust Job: %w", hashErr)
		}
		if keyHash != "" {
			newJob.Annotations["curity.io/encryption-key-hash"] = keyHash
		}
		if err := controllerutil.SetControllerReference(cluster, newJob, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner ref on genclust Job: %w", err)
		}
		// Pre-flight dry-run against the apiserver's admission chain catches
		// SCC/PodSecurity/ResourceQuota denial before any Job/Pod exists,
		// turning a multi-minute BackoffLimit wait into a ~50 ms condition flip.
		if err := r.dryRunPodAdmission(ctx, cluster, newJob); err != nil {
			return fmt.Errorf("pre-flight pod admission check failed: %w", err)
		}
		if err := r.Create(ctx, newJob); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil // Race condition: another reconcile created it
			}
			return fmt.Errorf("failed to create genclust Job: %w", err)
		}
		log.Info("created genclust Job", "name", jobName)
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ClusterConfigJobCreated",
			"Created genclust Job %q to generate cluster.xml", jobName)
		setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
			metav1.ConditionFalse, "JobCreated",
			"genclust Job created, waiting for completion", cluster.Generation)
	}

	return nil
}

// announceAdminDeparted emits an AdminDeparted event when the prior admin
// has moved to a different cluster or changed away from spec.type=admin.
//
// Deliberately does NOT mutate Data["cluster.xml"] or its annotations:
// identityservernode_reconciler.go's config-hash injector stamps a SHA256 of
// this Secret onto every node's Deployment pod template, so any data change
// would force a rolling restart of surviving runtime pods. Their existing
// pods boot reading "placeholder" content and crashloop. Holding the data
// keeps subPath-mounted runtimes alive; Branch B handles XML rewrite when a
// new admin returns under any name.
//
// The cache-lag guard (clusterRef==here AND type==admin) excludes the
// in-cluster admin-rename window where listChildNodes hasn't caught up yet.
func (r *IdentityServerClusterReconciler) announceAdminDeparted(ctx context.Context, cluster *v1alpha1.IdentityServerCluster) error {
	log := ctrl.LoggerFrom(ctx)
	secretName := clusterConfigSecretName(cluster.Name)

	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting cluster-config secret to announce admin-departed: %w", err)
	}
	if !isClusterConfigReady(&secret) {
		return nil
	}

	storedAdmin := secret.Annotations["curity.io/admin-node"]
	if storedAdmin == "" {
		return nil
	}
	var priorAdmin v1alpha1.IdentityServerNode
	err := r.Get(ctx, client.ObjectKey{Name: storedAdmin, Namespace: cluster.Namespace}, &priorAdmin)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("looking up prior admin node %q to announce admin-departed: %w", storedAdmin, err)
	}
	if priorAdmin.Spec.IdentityServerClusterRef.Name == cluster.Name &&
		priorAdmin.Spec.Type == v1alpha1.NodeTypeAdmin {
		return nil
	}

	movedAway := priorAdmin.Spec.IdentityServerClusterRef.Name != cluster.Name
	log.Info("prior admin no longer admins this cluster",
		"cluster", cluster.Name, "priorAdmin", storedAdmin,
		"priorAdminClusterRef", priorAdmin.Spec.IdentityServerClusterRef.Name,
		"priorAdminType", priorAdmin.Spec.Type, "movedAway", movedAway)

	if movedAway {
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "AdminDeparted",
			"Admin node %q moved to cluster %q; cluster-config Secret retained so surviving runtime nodes keep working",
			storedAdmin, priorAdmin.Spec.IdentityServerClusterRef.Name)
	} else {
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "AdminDeparted",
			"Admin node %q is no longer of type=admin; cluster-config Secret retained so surviving runtime nodes keep working",
			storedAdmin)
	}
	return nil
}

// readJobPodLogs reads the logs from the first completed pod of a Job.
func (r *IdentityServerClusterReconciler) readJobPodLogs(ctx context.Context, job *batchv1.Job) ([]byte, error) {
	clientset, err := kubernetes.NewForConfig(r.RestConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	// Find pods belonging to this Job
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"curity.io/cluster": job.Labels["curity.io/cluster"], "curity.io/component": "cluster-config"},
	); err != nil {
		return nil, fmt.Errorf("failed to list Job pods: %w", err)
	}

	// Find a succeeded pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodSucceeded {
			continue
		}
		// Verify this pod belongs to our Job via the job-name label
		if pod.Labels["job-name"] != job.Name {
			continue
		}

		logReq := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: "genclust",
		})
		logStream, err := logReq.Stream(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to stream pod logs: %w", err)
		}
		defer func() { _ = logStream.Close() }()

		var buf bytes.Buffer
		if _, err := io.Copy(&buf, logStream); err != nil {
			return nil, fmt.Errorf("failed to read pod logs: %w", err)
		}
		return buf.Bytes(), nil
	}

	return nil, fmt.Errorf("no succeeded pod found for Job %s", job.Name)
}

// --- Managed config discovery (scope-issue surfacing, discovery) ---

// ensureManagedConfigDiscovery emits per-resource Warning Events for managed
// CM/Secret problems and populates status.managedResourceIssues with the
// subset that affects this cluster's mount behaviour (UnknownConfigType,
// DuplicateConfigKey). Scope-only issues (UnknownClusterInScope,
// EmptyClusterScope) are Events only — they don't change what mounts here.
func (r *IdentityServerClusterReconciler) ensureManagedConfigDiscovery(ctx context.Context, cluster *v1alpha1.IdentityServerCluster, adminExists bool) error {
	log := ctrl.LoggerFrom(ctx)

	// One ScanFailed event per reconcile: the three List paths below share a
	// root cause (RBAC drop, API outage), so a single user-visible signal is
	// enough.
	scanFailedEmitted := false
	emitScanFailed := func() {
		if scanFailedEmitted {
			return
		}
		r.Recorder.Eventf(cluster, corev1.EventTypeWarning, EventReasonScanFailed,
			"Failed to scan managed ConfigMaps/Secrets; see operator logs for details")
		scanFailedEmitted = true
	}

	if err := r.emitScopeAnnotationEvents(ctx, cluster); err != nil {
		emitScanFailed()
		return fmt.Errorf("emitting managed-config events: %w", err)
	}

	configs, skipped, err := discoverManagedResources(ctx, r.Client, cluster.Namespace, cluster.Name)
	if err != nil {
		emitScanFailed()
		return fmt.Errorf("discovering managed configs: %w", err)
	}

	// postCommitScript scripts run only on the admin (ConfigD) node. If one is
	// applicable here but no admin node exists, it mounts nowhere — warn on the
	// cluster. Event only: the config itself is valid, so no managedResourceIssue
	// and no ManagedConfigsValid flip (an admin node may yet be added).
	if !adminExists {
		for _, cfg := range configs {
			if cfg.ConfigType != ConfigTypePostCommitScript {
				continue
			}
			kind := "ConfigMap"
			if cfg.IsSecret {
				kind = "Secret"
			}
			log.Info("postCommitScript has no admin node to run it; not mounted",
				"kind", kind, "name", cfg.Name)
			r.Recorder.Eventf(cluster, corev1.EventTypeWarning, EventReasonPostCommitScriptNoAdmin,
				"%s/%s is a postCommitScript but cluster %q has no admin node; post-commit scripts run only on the admin (ConfigD) node and will not be mounted. Add an admin IdentityServerNode.",
				kind, cfg.Name, cluster.Name)
		}
	}

	issues := make([]v1alpha1.ManagedResourceIssue, 0)

	for _, sk := range skipped {
		msg := fmt.Sprintf("Skipped %s/%s: %s", sk.Kind, sk.Name, sk.Reason)
		if sk.Object != nil {
			r.Recorder.Eventf(sk.Object, corev1.EventTypeWarning, EventReasonUnknownConfigType, "%s", msg)
		} else {
			// Producers in config_discovery.go always populate Object —
			// log loud if a future regression returns nil rather than
			// silently dropping the user-visible event.
			log.Error(nil, "BUG: SkippedResource has nil Object; UnknownConfigType event not emitted",
				"kind", sk.Kind, "name", sk.Name)
		}
		issues = append(issues, v1alpha1.ManagedResourceIssue{
			Kind:    sk.Kind,
			Name:    sk.Name,
			Reason:  EventReasonUnknownConfigType,
			Message: msg,
		})
	}

	for _, w := range detectDuplicateKeys(configs) {
		log.Info("duplicate config data key detected", "warning", w.Message)
		for _, owner := range w.Owners {
			if owner.Object != nil {
				r.Recorder.Eventf(owner.Object, corev1.EventTypeWarning, EventReasonDuplicateConfigKey, "%s", w.Message)
			} else {
				log.Error(nil, "BUG: DuplicateKeyWarning owner has nil Object; DuplicateConfigKey event not emitted",
					"kind", owner.Kind, "name", owner.Name)
			}
			issues = append(issues, v1alpha1.ManagedResourceIssue{
				Kind:    owner.Kind,
				Name:    owner.Name,
				Reason:  EventReasonDuplicateConfigKey,
				Message: w.Message,
			})
		}
	}

	// Filtered slice is discarded here; the node reconciler runs the same
	// call to drive mount filtering.
	_, loggingIssues := applyLoggingValidations(configs)
	for _, li := range loggingIssues {
		log.Info("logging config rejected",
			"kind", li.Kind, "name", li.Name, "reason", li.EventReason)
		if li.Object != nil {
			r.Recorder.Eventf(li.Object, corev1.EventTypeWarning, li.EventReason, "%s", li.Message)
		} else {
			log.Error(nil, "BUG: LoggingValidationIssue has nil Object; event not emitted",
				"kind", li.Kind, "name", li.Name, "reason", li.EventReason)
		}
		issues = append(issues, v1alpha1.ManagedResourceIssue{
			Kind:    li.Kind,
			Name:    li.Name,
			Reason:  li.EventReason,
			Message: li.Message,
		})
	}

	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Kind != issues[j].Kind {
			return issues[i].Kind < issues[j].Kind
		}
		if issues[i].Name != issues[j].Name {
			return issues[i].Name < issues[j].Name
		}
		return issues[i].Reason < issues[j].Reason
	})
	cluster.Status.ManagedResourceIssues = issues
	cluster.Status.ManagedResourceIssueCount = len(issues)

	return nil
}

// findClustersForManagedConfig maps a managed ConfigMap/Secret change to
// reconcile requests for IdentityServerClusters in the same namespace that the
// curity.io/cluster annotation puts in scope.
func (r *IdentityServerClusterReconciler) findClustersForManagedConfig(ctx context.Context, obj client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)

	// Defensive label check. managedConfigPredicate passes Updates through
	// when EITHER the old or new object is labeled (so label removal still
	// fires), and managedConfigHandler then invokes this mapper once per
	// side. The object we receive here may therefore not itself be labeled —
	// e.g. the new side of a label-removal Update. Without this check, an
	// unlabeled object (whose scope annotation is also typically missing)
	// would fan out to every cluster in the namespace.
	if !hasManagedLabel(obj) {
		return nil
	}

	var clusterList v1alpha1.IdentityServerClusterList
	if err := r.List(ctx, &clusterList, client.InNamespace(obj.GetNamespace())); err != nil {
		log.Error(err, "failed to list clusters for managed config watch", "resource", obj.GetName())
		return nil
	}

	annotations := obj.GetAnnotations()
	requests := make([]ctrl.Request, 0, len(clusterList.Items))
	for i := range clusterList.Items {
		cl := &clusterList.Items[i]
		if !appliesToCluster(annotations, cl.Name) {
			continue
		}
		requests = append(requests, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(cl),
		})
	}

	// Annotation present but matched no cluster: fan out so at least one
	// reconcile emits the per-CM scope event. UID dedup collapses N
	// emissions on the same CM into a single series.
	if _, scoped := annotations[AnnotationClusterScope]; scoped && len(requests) == 0 {
		for i := range clusterList.Items {
			requests = append(requests, ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(&clusterList.Items[i]),
			})
		}
	}
	return requests
}

// findClusterForJobPod maps a Pod event labeled curity.io/component=cluster-config
// to a reconcile request for the owning cluster (curity.io/cluster label).
// Returns nil on a missing label.
func (r *IdentityServerClusterReconciler) findClusterForJobPod(
	_ context.Context, obj client.Object,
) []ctrl.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	clusterName, exists := pod.Labels["curity.io/cluster"]
	if !exists {
		return nil
	}
	return []ctrl.Request{
		{NamespacedName: client.ObjectKey{Name: clusterName, Namespace: pod.Namespace}},
	}
}

// translateJobPodAdmissionResult is the contract between the translator and
// its caller. set=false means "no failure detected; caller should fall
// through to JobRunning". set=true means "use these condition fields".
type translateJobPodAdmissionResult struct {
	status  metav1.ConditionStatus
	reason  string
	message string
	set     bool
}

// translateJobPodAdmission classifies the most-recent cluster-config Pod's
// status into a sharp ClusterConfigReady reason, or returns set=false to let
// the caller default to JobRunning. When no Pod exists, falls back to the
// Layer 2 signal (cached Warning FailedCreate Event Message).
//
// Precedence: Pod-exists (Layer 3) trumps Event-only (Layer 2). A stale
// FailedCreate Event from a prior failed attempt is ignored when a healthy
// Pod now exists.
func translateJobPodAdmission(
	pods []corev1.Pod, failedCreateMsg string,
) translateJobPodAdmissionResult {
	if len(pods) == 0 {
		if failedCreateMsg != "" {
			return translateJobPodAdmissionResult{
				status:  metav1.ConditionFalse,
				reason:  "JobPodAdmissionFailed",
				message: fmt.Sprintf("genclust pod admission failed: %s", failedCreateMsg),
				set:     true,
			}
		}
		return translateJobPodAdmissionResult{set: false}
	}

	current := mostRecentClusterConfigPod(pods)
	if current == nil || current.DeletionTimestamp != nil {
		return translateJobPodAdmissionResult{set: false}
	}

	if res := classifyClusterConfigPodScheduling(current); res.set {
		return res
	}
	if res := classifyGenclustContainerStatus(current); res.set {
		return res
	}
	return translateJobPodAdmissionResult{set: false}
}

// mostRecentClusterConfigPod picks the newest pod by CreationTimestamp;
// ties broken by Name lexical (deterministic, since metav1.Time has 1s
// resolution and ties on simultaneously-created pods are not uncommon).
func mostRecentClusterConfigPod(pods []corev1.Pod) *corev1.Pod {
	if len(pods) == 0 {
		return nil
	}
	winner := &pods[0]
	for i := 1; i < len(pods); i++ {
		p := &pods[i]
		switch {
		case p.CreationTimestamp.After(winner.CreationTimestamp.Time):
			winner = p
		case p.CreationTimestamp.Equal(&winner.CreationTimestamp) && p.Name < winner.Name:
			winner = p
		}
	}
	return winner
}

// extractGenclustFailureMessage returns a concise failure cause from the most
// recent cluster-config Pod's failed genclust container, or "" if none is found.
func extractGenclustFailureMessage(pods []corev1.Pod) string {
	pod := mostRecentClusterConfigPod(pods)
	if pod == nil {
		return ""
	}
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name != "genclust" || cs.State.Terminated == nil || cs.State.Terminated.ExitCode == 0 {
			continue
		}
		if m := summarizeTerminationMessage(cs.State.Terminated.Message); m != "" {
			return m
		}
		return cs.State.Terminated.Reason // e.g. "Error", "OOMKilled"
	}
	return ""
}

// summarizeTerminationMessage reduces a log tail (often a Java stack trace) to a
// single bounded line. Java prints the root cause in the LAST "Caused by:" line,
// so that wins; then the first Exception/Error line; then the first non-frame line.
func summarizeTerminationMessage(raw string) string {
	const maxLen = 256
	var causedBy, exception, firstNonFrame string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "at ") || strings.HasPrefix(line, "...") {
			continue // stack frame
		}
		switch {
		case strings.HasPrefix(line, "Caused by:"):
			causedBy = line // keep the last (deepest) one
		case exception == "" && (strings.Contains(line, "Exception") || strings.Contains(line, "Error")):
			exception = line
		case firstNonFrame == "":
			firstNonFrame = line
		}
	}
	switch {
	case causedBy != "":
		return truncateMessage(humanizeJavaMessage(causedBy), maxLen)
	case exception != "":
		return truncateMessage(humanizeJavaMessage(exception), maxLen)
	case firstNonFrame != "":
		return truncateMessage(firstNonFrame, maxLen)
	default:
		return truncateMessage(strings.TrimSpace(raw), maxLen)
	}
}

// humanizeJavaMessage drops a leading "Caused by:" and fully-qualified throwable
// class so status shows the actionable cause, not Java/Guava internals.
func humanizeJavaMessage(line string) string {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Caused by:"))
	if i := strings.Index(line, ": "); i > 0 && isJavaThrowableName(line[:i]) {
		if msg := strings.TrimSpace(line[i+2:]); msg != "" {
			return msg
		}
	}
	if isJavaThrowableName(line) {
		if j := strings.LastIndexAny(line, ".$"); j >= 0 {
			return line[j+1:]
		}
	}
	return line
}

// isJavaThrowableName reports whether s is a fully-qualified Java throwable
// (dotted, no spaces, *Exception/*Error) — the only prefix we strip.
func isJavaThrowableName(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t") || !strings.Contains(s, ".") {
		return false
	}
	return strings.Contains(s, "Exception") || strings.Contains(s, "Error")
}

func truncateMessage(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// chooseJobFailedMessage picks a non-regressing JobFailed message. The genclust
// cause lives only in the failed pod's terminated.message, which PodGC may remove,
// so once captured it must not regress to the generic form (which re-fires the
// Warning). Order: fresh pod cause; else the captured JobFailed message; else generic.
func chooseJobFailedMessage(podCause string, existing *metav1.Condition, jobCondMessage string) string {
	if podCause != "" {
		return "genclust failed: " + podCause
	}
	if existing != nil && existing.Status == metav1.ConditionFalse &&
		existing.Reason == "JobFailed" && existing.Message != "" {
		return existing.Message
	}
	return "genclust Job failed: " + jobCondMessage
}

// classifyClusterConfigPodScheduling returns JobPodSchedulingFailed when the
// pod is Pending with PodScheduled=False/Unschedulable.
func classifyClusterConfigPodScheduling(pod *corev1.Pod) translateJobPodAdmissionResult {
	if pod.Status.Phase != corev1.PodPending {
		return translateJobPodAdmissionResult{set: false}
	}
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type != corev1.PodScheduled {
			continue
		}
		if c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return translateJobPodAdmissionResult{
				status:  metav1.ConditionFalse,
				reason:  "JobPodSchedulingFailed",
				message: fmt.Sprintf("genclust Pod cannot be scheduled: %s", truncateReplicaFailureMessage(c.Message)),
				set:     true,
			}
		}
	}
	return translateJobPodAdmissionResult{set: false}
}

// classifyGenclustContainerStatus returns one of JobPodImagePullFailed or
// JobPodCreateConfigError when the genclust container is in a definitive
// failure-mode Waiting state. Transient Waiting reasons (PodInitializing,
// ContainerCreating) return set=false.
func classifyGenclustContainerStatus(pod *corev1.Pod) translateJobPodAdmissionResult {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name != "genclust" {
			continue
		}
		if cs.State.Waiting == nil {
			return translateJobPodAdmissionResult{set: false}
		}
		switch cs.State.Waiting.Reason {
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
			return translateJobPodAdmissionResult{
				status:  metav1.ConditionFalse,
				reason:  "JobPodImagePullFailed",
				message: fmt.Sprintf("genclust image pull failed: %s", truncateReplicaFailureMessage(cs.State.Waiting.Message)),
				set:     true,
			}
		case "CreateContainerConfigError":
			return translateJobPodAdmissionResult{
				status:  metav1.ConditionFalse,
				reason:  "JobPodCreateConfigError",
				message: fmt.Sprintf("genclust container config error: %s", truncateReplicaFailureMessage(cs.State.Waiting.Message)),
				set:     true,
			}
		}
		return translateJobPodAdmissionResult{set: false}
	}
	return translateJobPodAdmissionResult{set: false}
}

// listClusterConfigPods returns the pods carrying the cluster-config Job's
// label set for the named cluster. Cache-backed; same selector as
// readJobPodLogs (lines 980-987) so the two stay in lockstep.
func (r *IdentityServerClusterReconciler) listClusterConfigPods(
	ctx context.Context, clusterName, namespace string,
) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(namespace),
		client.MatchingLabels{
			"curity.io/cluster":   clusterName,
			"curity.io/component": "cluster-config",
		},
	); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

// findClusterForJobFailedCreateEvent maps a Warning FailedCreate Event on a
// Job to a reconcile request for the owning cluster. The predicate has
// already filtered to Type=Warning, Reason=FailedCreate, InvolvedObject.Kind=Job
// (and the server-side cache scope guarantees the same). Resolves the cluster
// name from the Job's curity.io/cluster label (cache-backed Get). Returns nil
// on any miss — the Event diagnostic is lost but no condition is set.
func (r *IdentityServerClusterReconciler) findClusterForJobFailedCreateEvent(
	ctx context.Context, obj client.Object,
) []ctrl.Request {
	ev, ok := obj.(*corev1.Event)
	if !ok {
		return nil
	}
	var job batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{
		Name:      ev.InvolvedObject.Name,
		Namespace: ev.InvolvedObject.Namespace,
	}, &job); err != nil {
		return nil
	}
	clusterName, exists := job.Labels["curity.io/cluster"]
	if !exists {
		return nil
	}
	return []ctrl.Request{
		{NamespacedName: client.ObjectKey{Name: clusterName, Namespace: job.Namespace}},
	}
}

// latestJobFailedCreateMessage returns the Message of the most-recent
// Warning FailedCreate Event whose InvolvedObject.UID equals the given Job
// UID. Reads from the controller-runtime cache populated by the scoped Event
// informer registered in cmd/manager/main.go; returns "" on cache miss,
// empty list, or any error.
//
// kube-controller-manager keeps Event.Message stable across the failure
// Series (Series.Count increments while Message stays put), so this helper
// produces identical output across reconciles when the underlying admission
// denial is the same — satisfying the condition-stability requirement N4.
func (r *IdentityServerClusterReconciler) latestJobFailedCreateMessage(
	ctx context.Context, jobUID types.UID, namespace string,
) string {
	var events corev1.EventList
	if err := r.List(ctx, &events, client.InNamespace(namespace)); err != nil {
		return ""
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
		// Prefer the newer Event by LastTimestamp; fall back to EventTime when
		// the older v1 timestamp field is unset (modern emitters use EventTime).
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
		return ""
	}
	return truncateReplicaFailureMessage(newest.Message)
}

// findClusterForJob maps a Job change to a reconcile request for the owning cluster.
func (r *IdentityServerClusterReconciler) findClusterForJob(ctx context.Context, obj client.Object) []ctrl.Request {
	job, ok := obj.(*batchv1.Job)
	if !ok {
		return nil
	}
	clusterName, exists := job.Labels["curity.io/cluster"]
	if !exists {
		return nil
	}
	return []ctrl.Request{
		{NamespacedName: client.ObjectKey{Name: clusterName, Namespace: job.Namespace}},
	}
}

// findClusterForSecret maps a Secret change to its owning cluster(s).
// Operator-managed Secrets carry the curity.io/cluster label (fast path).
// Externally-provided admin-credentials Secrets are matched by name against
// every cluster's spec.adminCredentials.valueFrom.secretKeyRef.
func (r *IdentityServerClusterReconciler) findClusterForSecret(ctx context.Context, obj client.Object) []ctrl.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	if clusterName, labeled := secret.Labels["curity.io/cluster"]; labeled {
		return []ctrl.Request{
			{NamespacedName: client.ObjectKey{Name: clusterName, Namespace: secret.Namespace}},
		}
	}
	// Mapping functions can't return errors; log so RBAC/network failures
	// don't silently break externally-provided admin-creds Secret watching.
	var clusterList v1alpha1.IdentityServerClusterList
	if err := r.List(ctx, &clusterList, client.InNamespace(secret.Namespace)); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list clusters for Secret watch failed; admin-creds rotations may be missed until next resync",
			"secret", client.ObjectKeyFromObject(secret).String())
		return nil
	}
	var requests []ctrl.Request
	for i := range clusterList.Items {
		c := &clusterList.Items[i]
		if c.Spec.AdminCredentials == nil {
			continue
		}
		if c.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name == secret.Name {
			requests = append(requests, ctrl.Request{
				NamespacedName: client.ObjectKey{Name: c.Name, Namespace: c.Namespace},
			})
		}
	}
	return requests
}

// emitScopeAnnotationEvents emits Warning Events on managed ConfigMaps/Secrets
// whose curity.io/cluster annotation is empty or names non-existent clusters.
// UnknownConfigType and DuplicateConfigKey are emitted by the caller; those
// come from already-discovered resources, not from a scope scan.
func (r *IdentityServerClusterReconciler) emitScopeAnnotationEvents(ctx context.Context, cluster *v1alpha1.IdentityServerCluster) error {
	var clusterList v1alpha1.IdentityServerClusterList
	if err := r.List(ctx, &clusterList, client.InNamespace(cluster.Namespace)); err != nil {
		return fmt.Errorf("listing clusters for scope scan: %w", err)
	}
	existing := make(map[string]struct{}, len(clusterList.Items))
	for i := range clusterList.Items {
		existing[clusterList.Items[i].Name] = struct{}{}
	}

	issues, err := scanConfigScopeIssues(ctx, r.Client, cluster.Namespace, existing)
	if err != nil {
		return err
	}

	log := ctrl.LoggerFrom(ctx)
	for _, is := range issues {
		if is.Object == nil {
			log.Error(nil, "BUG: ScopeIssue has nil Object; scope-annotation event not emitted",
				"empty", is.Empty, "unknown", is.Unknown)
			continue
		}
		// Empty and Unknown are mutually exclusive at construction; a
		// switch prevents a malformed value from double-emitting.
		switch {
		case is.Empty:
			r.Recorder.Eventf(is.Object, corev1.EventTypeWarning,
				EventReasonEmptyClusterScope, "%s", formatEmptyClusterScopeMessage())
		case len(is.Unknown) > 0:
			r.Recorder.Eventf(is.Object, corev1.EventTypeWarning,
				EventReasonUnknownClusterInScope, "%s",
				formatUnknownClusterMessage(is.Unknown, is.Applied))
		}
	}
	return nil
}

// generateRandomAlphanumeric generates a random alphanumeric string of the given length.
func generateRandomAlphanumeric(length int) (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		result[i] = chars[n.Int64()]
	}
	return string(result), nil
}

// generateRandomHex generates a random hex string of the given byte length.
func generateRandomHex(byteLength int) (string, error) {
	b := make([]byte, byteLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
