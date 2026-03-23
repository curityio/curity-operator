package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// IdentityServerClusterReconciler reconciles an IdentityServerCluster object.
// It aggregates status from child IdentityServerNode resources and manages
// the admin credentials secret.
type IdentityServerClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=curity.io,resources=identityserverclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=curity.io,resources=identityserverclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=curity.io,resources=identityserverclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=curity.io,resources=identityservernodes,verbs=get;list;watch
// NOTE: Secret RBAC is intentionally create-only (no update/delete). Secrets
// have no OwnerReference so they survive cluster deletion. If a user changes
// adminCredentials.valueFrom.secretKeyRef.name, the old Secret is orphaned by
// design — credentials should never be auto-deleted. Add "update" verb here
// when secret rotation or cluster XML generation requires it.
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

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
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
			}
			log.Info("cluster finalizer removed, deletion proceeding")
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&cluster, v1alpha1.ClusterFinalizer) {
		controllerutil.AddFinalizer(&cluster, v1alpha1.ClusterFinalizer)
		if err := r.Update(ctx, &cluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 3. Ensure admin credentials secret
	if cluster.Spec.AdminCredentials != nil {
		if err := r.ensureAdminCredentialsSecret(ctx, &cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 4. List child IdentityServerNodes
	childNodes, err := r.listChildNodes(ctx, &cluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list child nodes: %w", err)
	}

	// 5. Compute and update status
	readyCount := int32(0)
	for i := range childNodes {
		readyCond := apimeta.FindStatusCondition(childNodes[i].Status.Conditions, v1alpha1.ConditionReady)
		if readyCond != nil && readyCond.Status == metav1.ConditionTrue {
			readyCount++
		}
	}

	conditions := computeClusterConditions(childNodes, cluster.Generation)
	cluster.Status.Conditions = conditions
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

// SetupWithManager registers the controller with the manager.
func (r *IdentityServerClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.IdentityServerCluster{}).
		Watches(
			&v1alpha1.IdentityServerNode{},
			handler.EnqueueRequestsFromMapFunc(r.findClusterForNode),
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
