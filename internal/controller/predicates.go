package controller

import (
	"bytes"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// nodeStatusConditionsChangedPredicate passes update events when either:
//   - the object's generation changed (spec was modified), or
//   - the object's status conditions changed (Type, Status, Reason, or Message), or
//   - the object's controller-ownerRef changed (UID flip from old cluster to new).
//
// Create, Delete, and Generic events always pass through.
//
// This is used by the cluster controller's watch on IdentityServerNode resources,
// where the cluster needs to react to node spec changes AND node status condition
// changes (for aggregating cluster-level health), but should ignore updates that
// only change non-material fields like resourceVersion or LastTransitionTime.
//
// The controller-ownerRef leg ensures that an abandoned cluster wakes up after
// the node reconciler migrates the ownerRef to a peer cluster — without it, the
// abandoned cluster's deletion gate (which counts nodes by ownerRef.UID via
// listChildNodes) would not see the migration until the next resync.
type nodeStatusConditionsChangedPredicate struct {
	predicate.Funcs
}

func (p nodeStatusConditionsChangedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return true
	}

	// Always pass through generation changes (spec was modified).
	if e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration() {
		return true
	}

	// Pass through controller-ownerRef changes so the abandoned cluster wakes
	// up after the node reconciler migrates the ownerRef.UID to a peer.
	if controllerOwnerRefChanged(e.ObjectOld, e.ObjectNew) {
		return true
	}

	// Check if status conditions changed.
	return !conditionsEqual(extractConditions(e.ObjectOld), extractConditions(e.ObjectNew))
}

// extractConditions returns the status conditions from an IdentityServerNode,
// or nil if the object is not an IdentityServerNode.
func extractConditions(obj client.Object) []metav1.Condition {
	if node, ok := obj.(*v1alpha1.IdentityServerNode); ok {
		return node.Status.Conditions
	}
	return nil
}

// clusterSpecOrNodeCountChangedPredicate passes update events when either:
//   - the object's generation changed (spec was modified), or
//   - the cluster's Status.NodeCount changed (a node was added/removed).
//
// Create, Delete, and Generic events always pass through.
//
// This is used by the node controller's watch on IdentityServerCluster resources.
// Nodes need to react to cluster spec changes (image, config, etc.) AND to
// peer node additions/removals (for duplicate admin/role detection), which are
// signalled by the cluster's NodeCount status field changing.
type clusterSpecOrNodeCountChangedPredicate struct {
	predicate.Funcs
}

func (p clusterSpecOrNodeCountChangedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return true
	}

	// Always pass through generation changes (spec was modified).
	if e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration() {
		return true
	}

	// Check if NodeCount changed (a node was added or removed).
	oldCluster, oldOK := e.ObjectOld.(*v1alpha1.IdentityServerCluster)
	newCluster, newOK := e.ObjectNew.(*v1alpha1.IdentityServerCluster)
	if !oldOK || !newOK {
		return true
	}
	if oldCluster.Status.NodeCount != newCluster.Status.NodeCount {
		return true
	}

	// Check if ClusterConfigReady condition changed. Nodes gate Deployment
	// creation on this condition and need to reconcile when it transitions.
	if clusterConditionStatus(oldCluster, v1alpha1.ConditionClusterConfigReady) !=
		clusterConditionStatus(newCluster, v1alpha1.ConditionClusterConfigReady) {
		return true
	}

	return false
}

// managedConfigPredicate passes events for ConfigMaps/Secrets that have (or had)
// the curity.io/managed=true label. For updates, it checks both old and new objects
// so that label removal also triggers reconciliation (to un-mount the config).
type managedConfigPredicate struct {
	predicate.Funcs
}

func (p managedConfigPredicate) Create(e event.CreateEvent) bool {
	return hasManagedLabel(e.Object)
}

func (p managedConfigPredicate) Update(e event.UpdateEvent) bool {
	return hasManagedLabel(e.ObjectNew) || hasManagedLabel(e.ObjectOld)
}

func (p managedConfigPredicate) Delete(e event.DeleteEvent) bool {
	return hasManagedLabel(e.Object)
}

// hasManagedLabel returns true if the object carries curity.io/managed=true.
func hasManagedLabel(obj client.Object) bool {
	if obj == nil {
		return false
	}
	return obj.GetLabels()[LabelManagedConfig] == "true"
}

// clusterSecretWatchPredicate filters Secret events down to the kinds a cluster
// can reference, so findClusterForSecret's cluster List doesn't run on every
// system Secret churn (SA tokens, dockercfg, release records). Two kinds qualify: Opaque (the
// admin-credentials Secret) and kubernetes.io/tls (convertKeystore sources, so a
// cert rotation re-converts). findClusterForSecret still confirms an actual
// spec reference before enqueuing, so non-referenced TLS Secrets cost only a List.
type clusterSecretWatchPredicate struct {
	predicate.Funcs
}

func (p clusterSecretWatchPredicate) Create(e event.CreateEvent) bool {
	return isClusterWatchedSecret(e.Object)
}

func (p clusterSecretWatchPredicate) Update(e event.UpdateEvent) bool {
	return isClusterWatchedSecret(e.ObjectOld) || isClusterWatchedSecret(e.ObjectNew)
}

func (p clusterSecretWatchPredicate) Delete(e event.DeleteEvent) bool {
	return isClusterWatchedSecret(e.Object)
}

func isClusterWatchedSecret(obj client.Object) bool {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret == nil {
		return false
	}
	return secret.Type == "" || secret.Type == corev1.SecretTypeOpaque || secret.Type == corev1.SecretTypeTLS
}

// clusterConditionStatus returns the Status field of the named condition, or
// "" if the condition is not present. Used by the cluster predicate to detect
// ClusterConfigReady transitions.
func clusterConditionStatus(c *v1alpha1.IdentityServerCluster, condType string) metav1.ConditionStatus {
	for _, cond := range c.Status.Conditions {
		if cond.Type == condType {
			return cond.Status
		}
	}
	return ""
}

// controllerOwnerRefChanged returns true if the controller-ownerRef differs
// between old and new — including transitions to/from nil. UID is the only
// field compared because it uniquely identifies the parent object; Name and
// Kind cannot shift independently of UID for a controller reference.
func controllerOwnerRefChanged(oldObj, newObj client.Object) bool {
	oldRef := metav1.GetControllerOf(oldObj)
	newRef := metav1.GetControllerOf(newObj)
	if oldRef == nil || newRef == nil {
		return oldRef != newRef
	}
	return oldRef.UID != newRef.UID
}

// conditionsEqual returns true if two condition slices are semantically equal.
// It compares Type, Status, Reason, and Message — intentionally skipping
// LastTransitionTime and ObservedGeneration, which change on every reconcile
// even when the logical condition state is unchanged.
func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type ||
			a[i].Status != b[i].Status ||
			a[i].Reason != b[i].Reason ||
			a[i].Message != b[i].Message {
			return false
		}
	}
	return true
}

// packageInitContainerStatusChangedPredicate filters Pod events down to those
// that affect the PackagesReady condition on an IdentityServerNode. Goals:
//   - Skip Create events. A freshly-created pod has no InitContainerStatuses
//     entries yet, so there is nothing for the translator to act on. Kubelet
//     will emit an Update once the kubelet has populated the status.
//   - Skip pods not owned by this operator (no managed-by label). Other
//     workloads in the same namespace must not fan out reconciles.
//   - Skip Update events that do not affect the status of any
//     `package-fetch-*` init container. Without this filter, every pod
//     heartbeat update (resourceVersion bump, condition timestamp tick)
//     would wake the reconciler.
//
// Pass through Delete events so that a pod going away is treated as a
// recovery signal — the node reconciler recomputes from the surviving pods.
type packageInitContainerStatusChangedPredicate struct {
	predicate.Funcs
}

func (p packageInitContainerStatusChangedPredicate) Create(_ event.CreateEvent) bool {
	return false
}

func (p packageInitContainerStatusChangedPredicate) Delete(_ event.DeleteEvent) bool {
	return true
}

func (p packageInitContainerStatusChangedPredicate) Generic(_ event.GenericEvent) bool {
	return false
}

func (p packageInitContainerStatusChangedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}
	newPod, ok := e.ObjectNew.(*corev1.Pod)
	if !ok {
		return false
	}
	if newPod.GetLabels()[labelManagedBy] != labelManagedByCurityOperator {
		return false
	}
	oldPod, ok := e.ObjectOld.(*corev1.Pod)
	if !ok {
		return false
	}
	// Fire on a package-fetch status change (PackagesReady) OR any container
	// failure-state transition (the Degraded pod-failure overlay).
	return packageInitContainerStatusDiffers(oldPod, newPod) || containerFailureStateDiffers(oldPod, newPod)
}

// labelManagedBy and labelManagedByCurityOperator name the standard label
// pair stamped on every pod template by buildLabels (resources.go).
const (
	labelManagedBy               = "app.kubernetes.io/managed-by"
	labelManagedByCurityOperator = "curity-operator"
)

// packageSecretCandidatePredicate filters Secret events down to ones that
// could plausibly affect a package fetch. It does NOT verify the Secret is
// actually referenced — that resolution lives in the mapFunc which has the
// client. The predicate's job is the cheap pre-filter:
//
//   - Type filter: only Opaque / TLS / basic-auth Secrets are valid package
//     refs. SA tokens, dockercfg, bootstrap-token are dropped to avoid
//     waking the reconciler on unrelated churn (cert-manager TLS rotation
//     is the heaviest source).
//   - Update content filter: drop events where neither .Data nor .Type
//     changed. Cosmetic label/annotation churn from sync controllers
//     (external-secrets, sealed-secrets) does not warrant a reconcile.
//   - Generic events: dropped (no information).
//
// Create and Delete events for acceptable Secret types pass unconditionally
// — the mapFunc decides relevance.
type packageSecretCandidatePredicate struct {
	predicate.Funcs
}

func (p packageSecretCandidatePredicate) Create(e event.CreateEvent) bool {
	return isAcceptablePackageSecretType(e.Object)
}

func (p packageSecretCandidatePredicate) Delete(e event.DeleteEvent) bool {
	return isAcceptablePackageSecretType(e.Object)
}

func (p packageSecretCandidatePredicate) Generic(_ event.GenericEvent) bool {
	return false
}

func (p packageSecretCandidatePredicate) Update(e event.UpdateEvent) bool {
	oldSecret, oldOK := e.ObjectOld.(*corev1.Secret)
	newSecret, newOK := e.ObjectNew.(*corev1.Secret)
	if !oldOK || !newOK || oldSecret == nil || newSecret == nil {
		return false
	}
	if !isAcceptablePackageSecretType(newSecret) {
		return false
	}
	if oldSecret.Type != newSecret.Type {
		return true
	}
	return !secretDataEqual(oldSecret.Data, newSecret.Data)
}

// isAcceptablePackageSecretType returns true for Secret types that are
// valid carriers of a package-fetch credential or TLS material. Empty type
// is treated as Opaque per the API server's defaulting.
func isAcceptablePackageSecretType(obj client.Object) bool {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret == nil {
		return false
	}
	switch secret.Type {
	case "", corev1.SecretTypeOpaque, corev1.SecretTypeTLS, corev1.SecretTypeBasicAuth:
		return true
	}
	return false
}

// secretDataEqual compares two Secret .Data maps for byte-level equality.
// Using equality.Semantic.DeepEqual would also work but pulls in apimachinery's
// reflection-based path; the maps are small enough that an explicit walk is
// both faster and easier to reason about.
func secretDataEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !bytes.Equal(va, vb) {
			return false
		}
	}
	return true
}

// packageFetchContainerNamePrefix is the prefix used by buildPackageInitContainers
// for every package-fetch init container — see packageInitContainerName.
const packageFetchContainerNamePrefix = "package-fetch-"

// packageInitContainerStatusDiffers reports whether any init container whose
// name starts with packageFetchContainerNamePrefix has a different State or
// LastState (or RestartCount) between the old and new pod. The comparison
// is index-based on container Name so re-orderings inside the status slice
// (which kubelet does not do today, but spec it out defensively) cannot mask
// a real status change.
func packageInitContainerStatusDiffers(oldPod, newPod *corev1.Pod) bool {
	oldByName := indexInitStatusesByName(oldPod.Status.InitContainerStatuses)
	for i := range newPod.Status.InitContainerStatuses {
		newStat := &newPod.Status.InitContainerStatuses[i]
		if !strings.HasPrefix(newStat.Name, packageFetchContainerNamePrefix) {
			continue
		}
		oldStat, found := oldByName[newStat.Name]
		if !found {
			// Container appeared between events — definitely a status change.
			return true
		}
		if !equalInitContainerStatus(oldStat, newStat) {
			return true
		}
	}
	return false
}

// indexInitStatusesByName returns a name-keyed view of an
// InitContainerStatuses slice. Used to compare pre/post states without
// assuming positional stability.
func indexInitStatusesByName(statuses []corev1.ContainerStatus) map[string]*corev1.ContainerStatus {
	out := make(map[string]*corev1.ContainerStatus, len(statuses))
	for i := range statuses {
		out[statuses[i].Name] = &statuses[i]
	}
	return out
}

// equalInitContainerStatus reports whether two ContainerStatus snapshots
// agree on every field the translator cares about: current State,
// LastState, and RestartCount. Other fields (Image, ContainerID, etc.)
// are ignored to avoid spurious wakeups on cosmetic kubelet updates.
func equalInitContainerStatus(a, b *corev1.ContainerStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.RestartCount != b.RestartCount {
		return false
	}
	if !equalContainerState(a.State, b.State) {
		return false
	}
	return equalContainerState(a.LastTerminationState, b.LastTerminationState)
}

// databaseInitPodChangedPredicate passes database-init Pod events the translator
// cares about: drops Create (no status yet), passes Delete, passes Update only
// when Phase/PodScheduled/container-state changed — no kubelet-heartbeat churn.
type databaseInitPodChangedPredicate struct {
	predicate.Funcs
}

func (p databaseInitPodChangedPredicate) Create(_ event.CreateEvent) bool { return false }

func (p databaseInitPodChangedPredicate) Generic(_ event.GenericEvent) bool { return false }

func (p databaseInitPodChangedPredicate) Delete(e event.DeleteEvent) bool {
	return isDatabaseInitPod(e.Object)
}

func (p databaseInitPodChangedPredicate) Update(e event.UpdateEvent) bool {
	newPod, ok := e.ObjectNew.(*corev1.Pod)
	if !ok || !isDatabaseInitPod(newPod) {
		return false
	}
	oldPod, ok := e.ObjectOld.(*corev1.Pod)
	if !ok {
		return false
	}
	return databaseInitPodStatusDiffers(oldPod, newPod)
}

func isDatabaseInitPod(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod == nil {
		return false
	}
	return pod.Labels[componentLabel] == databaseComponentValue
}

// connectionSecretChangedPredicate passes Opaque connection-Secret events whose
// .data (or type) changed, so metadata-only edits don't wake a reconcile.
// Create/Delete pass; the mapfunc confirms the reference.
type connectionSecretChangedPredicate struct {
	predicate.Funcs
}

func (p connectionSecretChangedPredicate) Create(e event.CreateEvent) bool {
	return isConnectionSecretCandidate(e.Object)
}

func (p connectionSecretChangedPredicate) Delete(e event.DeleteEvent) bool {
	return isConnectionSecretCandidate(e.Object)
}

func (p connectionSecretChangedPredicate) Generic(_ event.GenericEvent) bool { return false }

func (p connectionSecretChangedPredicate) Update(e event.UpdateEvent) bool {
	oldSecret, oldOK := e.ObjectOld.(*corev1.Secret)
	newSecret, newOK := e.ObjectNew.(*corev1.Secret)
	if !oldOK || !newOK || oldSecret == nil || newSecret == nil {
		return false
	}
	if !isConnectionSecretCandidate(newSecret) {
		return false
	}
	if oldSecret.Type != newSecret.Type {
		return true
	}
	return !secretDataEqual(oldSecret.Data, newSecret.Data)
}

// isConnectionSecretCandidate passes Opaque/untyped Secrets — the kind a JDBC
// connection uses — so SA-token/dockercfg/TLS churn is skipped.
func isConnectionSecretCandidate(obj client.Object) bool {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret == nil {
		return false
	}
	return secret.Type == "" || secret.Type == corev1.SecretTypeOpaque
}

// databaseInitPodStatusDiffers mirrors clusterConfigPodStatusDiffers for the
// database-init container: true only when Phase, the PodScheduled condition, or
// the init container's state changed.
func databaseInitPodStatusDiffers(oldPod, newPod *corev1.Pod) bool {
	if oldPod.Status.Phase != newPod.Status.Phase {
		return true
	}
	if !equalPodScheduledCondition(oldPod.Status.Conditions, newPod.Status.Conditions) {
		return true
	}
	oldByName := indexContainerStatusesByName(oldPod.Status.ContainerStatuses)
	for i := range newPod.Status.ContainerStatuses {
		newStat := &newPod.Status.ContainerStatuses[i]
		if newStat.Name != databaseContainerName {
			continue
		}
		oldStat, found := oldByName[newStat.Name]
		if !found {
			return true
		}
		return !equalInitContainerStatus(oldStat, newStat)
	}
	return false
}

// clusterConfigPodChangedPredicate filters Pod events down to genclust Job
// pods (label curity.io/component=cluster-config) whose status delta
// matters for admission/scheduling/pull classification. Drops Create (no
// status yet), passes Delete (Pod going away is recovery-relevant), passes
// Update only when Phase, PodScheduled condition, container Waiting/
// Terminated state, or RestartCount transitioned.
type clusterConfigPodChangedPredicate struct {
	predicate.Funcs
}

func (p clusterConfigPodChangedPredicate) Create(_ event.CreateEvent) bool {
	return false
}

func (p clusterConfigPodChangedPredicate) Delete(e event.DeleteEvent) bool {
	return isClusterConfigPod(e.Object)
}

func (p clusterConfigPodChangedPredicate) Generic(_ event.GenericEvent) bool {
	return false
}

func (p clusterConfigPodChangedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return false
	}
	newPod, ok := e.ObjectNew.(*corev1.Pod)
	if !ok {
		return false
	}
	if !isClusterConfigPod(newPod) {
		return false
	}
	oldPod, ok := e.ObjectOld.(*corev1.Pod)
	if !ok {
		return false
	}
	return clusterConfigPodStatusDiffers(oldPod, newPod)
}

func isClusterConfigPod(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod == nil {
		return false
	}
	return pod.Labels["curity.io/component"] == "cluster-config"
}

// clusterConfigPodStatusDiffers reports whether two snapshots of a
// cluster-config Pod differ on the fields the translator inspects: Phase,
// the PodScheduled condition, and the genclust container's
// State/LastTerminationState/RestartCount.
func clusterConfigPodStatusDiffers(oldPod, newPod *corev1.Pod) bool {
	if oldPod.Status.Phase != newPod.Status.Phase {
		return true
	}
	if !equalPodScheduledCondition(oldPod.Status.Conditions, newPod.Status.Conditions) {
		return true
	}
	oldByName := indexContainerStatusesByName(oldPod.Status.ContainerStatuses)
	for i := range newPod.Status.ContainerStatuses {
		newStat := &newPod.Status.ContainerStatuses[i]
		if newStat.Name != "genclust" {
			continue
		}
		oldStat, found := oldByName[newStat.Name]
		if !found {
			return true
		}
		if !equalInitContainerStatus(oldStat, newStat) {
			return true
		}
	}
	return false
}

func equalPodScheduledCondition(oldConds, newConds []corev1.PodCondition) bool {
	oldCond := findPodCondition(oldConds, corev1.PodScheduled)
	newCond := findPodCondition(newConds, corev1.PodScheduled)
	if oldCond == nil && newCond == nil {
		return true
	}
	if oldCond == nil || newCond == nil {
		return false
	}
	return oldCond.Status == newCond.Status &&
		oldCond.Reason == newCond.Reason &&
		oldCond.Message == newCond.Message
}

func findPodCondition(conds []corev1.PodCondition, condType corev1.PodConditionType) *corev1.PodCondition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

func indexContainerStatusesByName(statuses []corev1.ContainerStatus) map[string]*corev1.ContainerStatus {
	out := make(map[string]*corev1.ContainerStatus, len(statuses))
	for i := range statuses {
		out[statuses[i].Name] = &statuses[i]
	}
	return out
}

// jobFailedCreateEventPredicate gates the cluster reconciler's Event watch
// to Warning FailedCreate events on Jobs. The cache is already filtered
// server-side via cache.Options.ByObject (cmd/manager/main.go), but the
// predicate provides defense-in-depth and documents intent.
//
// Create, Update pass when the Event matches the filter; Delete and Generic
// drop (a Delete on a stale FailedCreate Event carries no diagnostic value).
type jobFailedCreateEventPredicate struct {
	predicate.Funcs
}

func (p jobFailedCreateEventPredicate) Create(e event.CreateEvent) bool {
	return matchesJobFailedCreate(e.Object)
}

func (p jobFailedCreateEventPredicate) Update(e event.UpdateEvent) bool {
	return matchesJobFailedCreate(e.ObjectNew)
}

func (p jobFailedCreateEventPredicate) Delete(_ event.DeleteEvent) bool {
	return false
}

func (p jobFailedCreateEventPredicate) Generic(_ event.GenericEvent) bool {
	return false
}

func matchesJobFailedCreate(obj client.Object) bool {
	ev, ok := obj.(*corev1.Event)
	if !ok || ev == nil {
		return false
	}
	return ev.Type == corev1.EventTypeWarning &&
		ev.Reason == "FailedCreate" &&
		ev.InvolvedObject.Kind == "Job"
}

// equalContainerState compares two ContainerState values on the fields the
// translator inspects: Waiting{Reason,Message}, Running.StartedAt presence,
// Terminated{ExitCode,Reason,Message}.
func equalContainerState(a, b corev1.ContainerState) bool {
	switch {
	case a.Waiting != nil && b.Waiting != nil:
		return a.Waiting.Reason == b.Waiting.Reason && a.Waiting.Message == b.Waiting.Message
	case a.Running != nil && b.Running != nil:
		return true // running-vs-running counts as "same" — translator ignores running entries
	case a.Terminated != nil && b.Terminated != nil:
		return a.Terminated.ExitCode == b.Terminated.ExitCode &&
			a.Terminated.Reason == b.Terminated.Reason &&
			a.Terminated.Message == b.Terminated.Message
	case a.Waiting == nil && a.Running == nil && a.Terminated == nil &&
		b.Waiting == nil && b.Running == nil && b.Terminated == nil:
		return true
	default:
		// State transitioned between Waiting/Running/Terminated.
		return false
	}
}
