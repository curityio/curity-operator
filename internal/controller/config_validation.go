package controller

import (
	"fmt"
	"sort"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	configValidationJobSuffix = "-config-validation"
	validationJobBackoffLimit = int32(0)
	validationTimeoutSeconds  = 60
	validationContainerName   = "config-validate"

	// Annotation storing the config hash that was validated.
	annotationConfigHash = "curity.io/config-hash"

	// Annotation on the cluster storing the last validated config hash.
	annotationValidatedConfigHash = "curity.io/validated-config-hash"
)

// validationCommand starts the Curity Identity Server as a standalone runtime
// node (no cluster join), polls the health check port using bash /dev/tcp,
// and exits 0 on success or 1 on timeout. Uses bash because curl/wget are
// not available in the Curity container image.
var validationCommand = fmt.Sprintf(
	`RESULT_FILE=/tmp/validation_result
/opt/idsvr/bin/idsvr -s validation --no-admin &
PID=$!
# Poll health check — write result and kill idsvr
(for i in $(seq 1 %d); do
  if (echo > /dev/tcp/localhost/%d) 2>/dev/null; then
    echo 0 > $RESULT_FILE; kill $PID 2>/dev/null; exit
  fi
  sleep 1
done; echo 1 > $RESULT_FILE; kill $PID 2>/dev/null) &
# Wait for idsvr — returns immediately if it crashes
wait $PID 2>/dev/null
# If no result file, idsvr crashed before poll could write one
if [ ! -f $RESULT_FILE ]; then exit 1; fi
exit $(cat $RESULT_FILE)`, validationTimeoutSeconds, portHealthCheck)

// buildConfigValidationJob creates a Job that validates discovered configs
// by starting idsvr and checking if it passes the health check. The Job is
// owned by the IdentityServerCluster for garbage collection.
func buildConfigValidationJob(
	cluster *v1alpha1.IdentityServerCluster,
	configs []DiscoveredConfigResource,
	configHash string,
	scheme *runtime.Scheme,
) (*batchv1.Job, error) {
	backoff := validationJobBackoffLimit
	volumes, mounts := buildValidationVolumes(configs)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name + configValidationJobSuffix,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "curity-operator",
				"curity.io/cluster":            cluster.Name,
				"curity.io/component":          "config-validation",
			},
			Annotations: map[string]string{
				annotationConfigHash: configHash,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"curity.io/cluster":   cluster.Name,
						"curity.io/component": "config-validation",
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
							Name:    validationContainerName,
							Image:   buildImage(cluster),
							Command: []string{"/usr/bin/bash", "-c", validationCommand},
							VolumeMounts: append(mounts, corev1.VolumeMount{
								Name:      "tmp",
								MountPath: "/tmp",
							}),
						},
					},
					Volumes: append(volumes, corev1.Volume{
						Name: "tmp",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					}),
				},
			},
		},
	}

	// Inject admin credentials env vars for idsvr to start.
	if cluster.Spec.AdminCredentials != nil {
		secretName := cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name
		job.Spec.Template.Spec.Containers[0].Env = append(
			job.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{
				Name: "ADMIN_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  "ADMIN_PASSWORD",
						Optional:             ptr.To(true),
					},
				},
			},
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

	// Inherit scheduling constraints from cluster.
	if cluster.Spec.ImagePullSecret != "" {
		job.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: cluster.Spec.ImagePullSecret},
		}
	}
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

	// Set OwnerReference to the cluster for garbage collection.
	if scheme != nil {
		if err := controllerutil.SetOwnerReference(cluster, job, scheme); err != nil {
			return nil, fmt.Errorf("setting owner reference on validation Job: %w", err)
		}
	}

	return job, nil
}

// buildValidationVolumes creates volumes and mounts for the validation Job.
// It mounts all discovered config resources at their production paths.
func buildValidationVolumes(configs []DiscoveredConfigResource) ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := make([]corev1.Volume, 0, len(configs))
	mounts := make([]corev1.VolumeMount, 0, len(configs))

	// No cluster-config volume — the validation pod runs standalone
	// (--no-admin) and does not join the real cluster. Only user-provided
	// managed configs are mounted for validation.

	// Discovered config volumes.
	for _, cfg := range configs {
		volName := configVolumeName(cfg.IsSecret, cfg.Name)
		mountBase := mountPathForConfigType(cfg.ConfigType)

		if cfg.IsSecret {
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: cfg.Name,
					},
				},
			})
		} else {
			volumes = append(volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: cfg.Name},
					},
				},
			})
		}

		// Mount each data key as a SubPath mount (sorted for determinism).
		keys := make([]string, 0, len(cfg.Data))
		for k := range cfg.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: mountBase + key,
				SubPath:   key,
				ReadOnly:  true,
			})
		}
	}

	return volumes, mounts
}
