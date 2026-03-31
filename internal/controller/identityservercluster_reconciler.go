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
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

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
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list
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

	// 2. Handle finalizer
	if cluster.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(&cluster, v1alpha1.ClusterFinalizer) {
			// Check for child nodes — block deletion if any exist
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

	// 3. Ensure admin credentials secret (default when not specified)
	if cluster.Spec.AdminCredentials == nil {
		cluster.Spec.AdminCredentials = defaultAdminCredentials(cluster.Name)
		log.Info("defaulting adminCredentials", "secretName", cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name)
	}
	if err := r.ensureAdminCredentialsSecret(ctx, &cluster); err != nil {
		return ctrl.Result{}, err
	}

	// 4. List child IdentityServerNodes (reused by cluster config and status)
	childNodes, err := r.listChildNodes(ctx, &cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list child nodes: %w", err)
	}

	// 5. Ensure cluster config (cluster.xml via genclust Job)
	if err := r.ensureClusterConfig(ctx, &cluster, childNodes); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Compute and update status
	readyCount := int32(0)
	for i := range childNodes {
		readyCond := apimeta.FindStatusCondition(childNodes[i].Status.Conditions, v1alpha1.ConditionReady)
		if readyCond != nil && readyCond.Status == metav1.ConditionTrue {
			readyCount++
		}
	}

	// Preserve ClusterConfigReady condition (set by ensureClusterConfig above)
	// before overwriting with computed node conditions.
	configReadyCond := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
	conditions := computeClusterConditions(childNodes, cluster.Generation)
	cluster.Status.Conditions = conditions
	if configReadyCond != nil {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, *configReadyCond)
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

// listChildNodes returns all IdentityServerNodes that reference this cluster.
// Uses spec-based filtering (not label-based) to guarantee correctness for
// deletion blocking — a newly created node may not have the curity.io/cluster
// label yet if it hasn't been reconciled. The node reconciler sets the label
// on first reconcile, enabling server-side filtering in other call sites.
func (r *IdentityServerClusterReconciler) listChildNodes(ctx context.Context, cluster *v1alpha1.IdentityServerCluster) ([]v1alpha1.IdentityServerNode, error) {
	var nodeList v1alpha1.IdentityServerNodeList
	if err := r.List(ctx, &nodeList, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}

	var children []v1alpha1.IdentityServerNode
	for i := range nodeList.Items {
		if nodeList.Items[i].Spec.IdentityServerClusterRef.Name == cluster.Name {
			children = append(children, nodeList.Items[i])
		}
	}
	return children, nil
}

// ensureAdminCredentialsSecret creates the admin credentials secret with random
// values if it does not already exist. Never overwrites existing secrets.
func (r *IdentityServerClusterReconciler) ensureAdminCredentialsSecret(ctx context.Context, cluster *v1alpha1.IdentityServerCluster) error {
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

	// Generate random values
	adminPassword, err := generateRandomAlphanumeric(16)
	if err != nil {
		return fmt.Errorf("failed to generate admin password: %w", err)
	}
	encryptionKey, err := generateRandomHex(32)
	if err != nil {
		return fmt.Errorf("failed to generate encryption key: %w", err)
	}
	keystorePassword, err := generateRandomAlphanumeric(16)
	if err != nil {
		return fmt.Errorf("failed to generate keystore password: %w", err)
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
			"KEYSTORE_PASSWORD":     []byte(keystorePassword),
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
					{Key: "KEYSTORE_PASSWORD", Path: "KEYSTORE_PASSWORD"},
				},
			},
		},
	}
}

// SetupWithManager registers the controller with the manager.
func (r *IdentityServerClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.IdentityServerCluster{}).
		Watches(
			&v1alpha1.IdentityServerNode{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterForNode),
			builder.WithPredicates(nodeStatusConditionsChangedPredicate{}),
		).
		Watches(
			&batchv1.Job{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterForJob),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterForSecret),
		).
		Complete(r)
}

// findClusterForNode maps an IdentityServerNode change to a reconcile request
// for the referenced IdentityServerCluster.
func (r *IdentityServerClusterReconciler) findClusterForNode(ctx context.Context, obj client.Object) []ctrl.Request {
	node, ok := obj.(*v1alpha1.IdentityServerNode)
	if !ok {
		return nil
	}

	clusterRef := node.Spec.IdentityServerClusterRef
	ns := clusterRef.Namespace
	if ns == "" {
		ns = node.Namespace
	}

	return []ctrl.Request{
		{NamespacedName: client.ObjectKey{Name: clusterRef.Name, Namespace: ns}},
	}
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

// isClusterConfigReady returns true if the Secret contains real cluster.xml data.
func isClusterConfigReady(secret *corev1.Secret) bool {
	data, ok := secret.Data[clusterConfigKey]
	return ok && len(data) > 0 && string(data) != clusterConfigPlaceholder
}

// computeEncryptionKeyHash returns the SHA256 hex hash of the CONFIG_ENCRYPTION_KEY
// from the admin credentials Secret. Returns "" if not configured.
func (r *IdentityServerClusterReconciler) computeEncryptionKeyHash(ctx context.Context, cluster *v1alpha1.IdentityServerCluster) string {
	if cluster.Spec.AdminCredentials == nil {
		return ""
	}
	secretName := cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name
	var credSecret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, &credSecret); err != nil {
		return ""
	}
	key, ok := credSecret.Data["CONFIG_ENCRYPTION_KEY"]
	if !ok || len(key) == 0 {
		return ""
	}
	h := sha256.Sum256(key)
	return hex.EncodeToString(h[:])
}

// buildClusterConfigSecret creates the Secret that holds cluster.xml.
func buildClusterConfigSecret(cluster *v1alpha1.IdentityServerCluster, clusterXML []byte, adminNodeName, encryptionKeyHash string) *corev1.Secret {
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

// buildClusterConfigJob creates the Job that runs genclust to generate cluster.xml.
func buildClusterConfigJob(cluster *v1alpha1.IdentityServerCluster, adminNodeName string) *batchv1.Job {
	backoff := jobBackoffLimit
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name + clusterConfigJobSuffix,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "curity-operator",
				"curity.io/cluster":            cluster.Name,
				"curity.io/component":          "cluster-config",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
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
							Command: []string{
								"/bin/sh", "-c",
								"/opt/idsvr/bin/genclust -c $CONFIG_SERVICE_HOST -p $CONFIG_SERVICE_PORT",
							},
							Env: []corev1.EnvVar{
								{Name: "CONFIG_SERVICE_HOST", Value: adminNodeName},
								{Name: "CONFIG_SERVICE_PORT", Value: fmt.Sprintf("%d", portConfig)},
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
func (r *IdentityServerClusterReconciler) ensureClusterConfig(ctx context.Context, cluster *v1alpha1.IdentityServerCluster, childNodes []v1alpha1.IdentityServerNode) error {
	log := ctrl.LoggerFrom(ctx)
	secretName := clusterConfigSecretName(cluster.Name)
	jobName := cluster.Name + clusterConfigJobSuffix

	// 1. Find admin node
	adminNodeName := findAdminNodeName(childNodes)
	if adminNodeName == "" {
		setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
			metav1.ConditionFalse, "WaitingForAdmin",
			"No admin node found for this cluster", cluster.Generation)
		return nil
	}

	// 2. Check if Secret exists and is ready
	var configSecret corev1.Secret
	secretExists := false
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, &configSecret); err == nil {
		secretExists = true
		if isClusterConfigReady(&configSecret) {
			// Check if admin node name changed (Scenario 5)
			// Update the XML in-place — same key, new hostname — no Job needed.
			if storedAdmin, ok := configSecret.Annotations["curity.io/admin-node"]; ok && storedAdmin != adminNodeName {
				log.Info("admin node changed, updating cluster config in-place",
					"old", storedAdmin, "new", adminNodeName)
				oldXML := string(configSecret.Data[clusterConfigKey])
				newXML := strings.Replace(oldXML,
					"<host>"+storedAdmin+"</host>",
					"<host>"+adminNodeName+"</host>", 1)
				configSecret.Data[clusterConfigKey] = []byte(newXML)
				configSecret.Annotations["curity.io/admin-node"] = adminNodeName
				if err := r.Update(ctx, &configSecret); err != nil {
					return fmt.Errorf("failed to update cluster config for admin rename: %w", err)
				}
				cluster.Status.ClusterConfigSecretName = secretName
				setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
					metav1.ConditionTrue, "SecretReady",
					"Cluster config secret is populated", cluster.Generation)
				return nil
			}

			// Check if encryption key changed (Scenario 11)
			// Reset to placeholder so the Job flow regenerates with the new key.
			// Only trigger regeneration when both hashes are non-empty — an empty
			// stored hash means the config was generated without an encryption key.
			currentKeyHash := r.computeEncryptionKeyHash(ctx, cluster)
			if storedHash, ok := configSecret.Annotations["curity.io/encryption-key-hash"]; ok && storedHash != "" && currentKeyHash != "" && storedHash != currentKeyHash {
				log.Info("encryption key changed, resetting cluster config for regeneration")
				configSecret.Data[clusterConfigKey] = []byte(clusterConfigPlaceholder)
				configSecret.Annotations["curity.io/encryption-key-hash"] = currentKeyHash
				if err := r.Update(ctx, &configSecret); err != nil {
					return fmt.Errorf("failed to reset cluster config for key rotation: %w", err)
				}
				// Fall through to Job creation below
			} else {
				// Secret is ready and up-to-date
				cluster.Status.ClusterConfigSecretName = secretName
				setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
					metav1.ConditionTrue, "SecretReady",
					"Cluster config secret is populated", cluster.Generation)
				return nil
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get cluster config secret: %w", err)
	}

	// 3. Ensure placeholder Secret exists
	if !secretExists {
		placeholder := buildClusterConfigSecret(cluster, nil, adminNodeName, r.computeEncryptionKeyHash(ctx, cluster))
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
		// Check Job completion
		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				// Job completed — read logs and populate Secret
				clusterXML, err := r.readJobPodLogs(ctx, &job)
				if err != nil || len(clusterXML) == 0 {
					// Scenario 9: logs unavailable — delete Job and retry
					log.Info("failed to read Job logs, will retry", "error", err)
					if delErr := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
						return fmt.Errorf("failed to delete Job after log read failure: %w", delErr)
					}
					setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
						metav1.ConditionFalse, "LogsUnavailable",
						"Job completed but logs could not be read, retrying", cluster.Generation)
					return nil
				}

				// Update Secret with real data
				updatedSecret := buildClusterConfigSecret(cluster, clusterXML, adminNodeName, r.computeEncryptionKeyHash(ctx, cluster))
				var existing corev1.Secret
				if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: cluster.Namespace}, &existing); err != nil {
					return fmt.Errorf("failed to get cluster config secret for update: %w", err)
				}
				existing.Data = updatedSecret.Data
				existing.Annotations = updatedSecret.Annotations
				if err := r.Update(ctx, &existing); err != nil {
					return fmt.Errorf("failed to update cluster config secret: %w", err)
				}

				// Clean up Job
				if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
					log.Info("failed to delete completed Job", "error", err)
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
				// Job failed — delete and retry on next reconcile
				log.Info("cluster config Job failed", "message", cond.Message)
				if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("failed to delete failed Job: %w", err)
				}
				setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
					metav1.ConditionFalse, "JobFailed",
					fmt.Sprintf("genclust Job failed: %s", cond.Message), cluster.Generation)
				r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "ClusterConfigJobFailed",
					"genclust Job failed: %s", cond.Message)
				return nil
			}
		}

		// Job still running
		setCondition(&cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady,
			metav1.ConditionFalse, "JobRunning",
			"genclust Job is running", cluster.Generation)
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get cluster config Job: %w", err)
	}

	// 5. Create the Job (no existing Job found — we only reach here if Get returned NotFound)
	{
		newJob := buildClusterConfigJob(cluster, adminNodeName)
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

// findClusterForSecret maps a Secret change to a reconcile request for the
// owning cluster. Only triggers for Secrets with the curity.io/cluster label
// (operator-managed admin credentials and cluster config Secrets).
func (r *IdentityServerClusterReconciler) findClusterForSecret(ctx context.Context, obj client.Object) []ctrl.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	clusterName, exists := secret.Labels["curity.io/cluster"]
	if !exists {
		return nil
	}
	return []ctrl.Request{
		{NamespacedName: client.ObjectKey{Name: clusterName, Namespace: secret.Namespace}},
	}
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
