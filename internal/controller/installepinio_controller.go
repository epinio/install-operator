/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	epiniov1alpha1 "apps.example.com/install-epinio/api/v1alpha1"
)

// Install script runs in the Job container (helm only — no kubectl in alpine/helm image).
// Uses "helm status" to skip ingress-nginx and cert-manager if already installed.
const installScript = `#!/bin/sh
set -e
echo "Epinio installer: domain=${DOMAIN}, epinio namespace=${EPINIO_NAMESPACE}"

helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx 2>/dev/null || true
helm repo add jetstack https://charts.jetstack.io 2>/dev/null || true
helm repo add epinio https://epinio.github.io/helm-charts 2>/dev/null || true
helm repo update

if helm status ingress-nginx -n ingress-nginx >/dev/null 2>&1; then
  echo "ingress-nginx helm release already exists — skipping"
else
  echo "Installing ingress-nginx..."
  helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx \
    --namespace ingress-nginx --create-namespace \
    --set controller.ingressClassResource.default=true \
    --set controller.service.type=LoadBalancer \
  || echo "Warning: ingress-nginx install failed (may already exist under a different release name, e.g. rke2-ingress-nginx) — continuing"
fi

if helm status cert-manager -n cert-manager >/dev/null 2>&1; then
  echo "cert-manager helm release already exists — skipping"
else
  echo "Installing cert-manager..."
  helm upgrade --install cert-manager jetstack/cert-manager \
    --namespace cert-manager --create-namespace \
    --set crds.enabled=true \
    --set extraArgs={--enable-certificate-owner-ref=true} \
  || echo "Warning: cert-manager install failed (may already exist under a different release name) — continuing"
fi

EPINIO_VERSION_ARGS=""
if [ -n "${EPINIO_VERSION}" ]; then
  EPINIO_VERSION_ARGS="--version ${EPINIO_VERSION}"
fi
echo "Installing Epinio (namespace=${EPINIO_NAMESPACE}, domain=${DOMAIN})..."
helm upgrade --install epinio epinio/epinio \
  --namespace "${EPINIO_NAMESPACE}" --create-namespace \
  ${EPINIO_VERSION_ARGS} \
  --set global.domain="${DOMAIN}"
echo "Epinio install completed successfully."
`

const (
	configMapNamePrefix            = "epinio-install-script-"
	jobNamePrefix                  = "epinio-installer-"
	installCleanupFinalizer        = "epinio.apps.example.com/install-cleanup"
	conditionAvailable             = "Available"
	conditionProgressing           = "Progressing"
	conditionDegraded              = "Degraded"
	defaultControllerNamespace     = "system"
	defaultInstallerServiceAccount = "controller-manager"
	defaultInstallerHelmImage      = "alpine/helm:3.18"
)

// InstallEpinioReconciler reconciles a InstallEpinio object
type InstallEpinioReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	ControllerNamespace     string
	InstallerServiceAccount string
	InstallerHelmImage      string
}

// +kubebuilder:rbac:groups=epinio.apps.example.com,resources=installepinios,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=epinio.apps.example.com,resources=installepinios/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=epinio.apps.example.com,resources=installepinios/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;namespaces,verbs=get;list;watch;create;update;patch;delete

// Reconcile moves cluster state toward the desired state (Epinio installed).
func (r *InstallEpinioReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var inst epiniov1alpha1.InstallEpinio
	if err := r.Get(ctx, req.NamespacedName, &inst); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("InstallEpinio not found, skipping")
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get InstallEpinio")
		return ctrl.Result{}, err
	}

	ctrlNs := r.controllerNamespace()
	log = log.WithValues(
		"installEpinio", req.NamespacedName,
		"controllerNamespace", ctrlNs,
		"targetNamespace", strings.TrimSpace(inst.Spec.TargetNamespace),
	)
	ctx = logf.IntoContext(ctx, log)

	if !inst.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &inst, ctrlNs)
	}

	if controllerutil.AddFinalizer(&inst, installCleanupFinalizer) {
		log.Info("Adding cleanup finalizer")
		if err := r.Update(ctx, &inst); err != nil {
			log.Error(err, "failed to add cleanup finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	domain := strings.TrimSpace(inst.Spec.Domain)
	if domain == "" {
		domain = "epinio.local"
	}

	epinioNs := strings.TrimSpace(inst.Spec.TargetNamespace)
	if epinioNs == "" {
		epinioNs = "epinio"
	}
	version := strings.TrimSpace(inst.Spec.Version)

	// Ensure epinio namespace exists (optional; helm --create-namespace will create it too)
	ns := &corev1.Namespace{}
	ns.Name = epinioNs
	if err := r.Get(ctx, client.ObjectKeyFromObject(ns), ns); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to fetch target namespace", "namespace", epinioNs)
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			log.Error(err, "failed to create namespace", "namespace", epinioNs)
			return ctrl.Result{}, err
		}
		log.Info("Ensured target namespace exists", "namespace", epinioNs)
	}

	cmName := configMapName(inst.Namespace, inst.Name)
	jobName := installJobName(inst.Namespace, inst.Name)

	// Create or update ConfigMap in controller namespace so the install Job can mount the script.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: ctrlNs,
			Labels: map[string]string{
				"install-epinio.io/cr-namespace": inst.Namespace,
				"install-epinio.io/cr-name":      inst.Name,
			},
		},
		Data: map[string]string{"install.sh": installScript},
	}
	if err := r.setOwnedByInstall(&inst, cm); err != nil {
		return ctrl.Result{}, err
	}

	existingCM := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), existingCM); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, cm); err != nil {
				log.Error(err, "failed to create ConfigMap", "configMap", cmName, "namespace", ctrlNs)
				return ctrl.Result{}, err
			}
			log.Info("Created install script ConfigMap", "configMap", cmName, "namespace", ctrlNs)
		} else {
			log.Error(err, "failed to get ConfigMap", "configMap", cmName, "namespace", ctrlNs)
			return ctrl.Result{}, err
		}
	} else {
		existingCM.Data = cm.Data
		if err := r.setOwnedByInstall(&inst, existingCM); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Update(ctx, existingCM); err != nil {
			log.Error(err, "failed to update ConfigMap", "configMap", cmName, "namespace", ctrlNs)
			return ctrl.Result{}, err
		}
		log.Info("Updated install script ConfigMap", "configMap", cmName, "namespace", ctrlNs)
	}

	// Check for existing Job (in controller namespace)
	job := &batchv1.Job{}
	jobKey := client.ObjectKey{Namespace: ctrlNs, Name: jobName}
	if err := r.Get(ctx, jobKey, job); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "failed to get install Job", "job", jobName, "namespace", ctrlNs)
			return ctrl.Result{}, err
		}

		// Create Job in controller namespace so it can use the controller's service account.
		job = r.buildInstallJob(jobName, ctrlNs, cmName, domain, epinioNs, version)
		job.Labels = map[string]string{
			"install-epinio.io/cr-namespace": inst.Namespace,
			"install-epinio.io/cr-name":      inst.Name,
		}
		if err := r.setOwnedByInstall(&inst, job); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil {
			log.Error(err, "failed to create install Job", "job", jobName, "namespace", ctrlNs)
			meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
				Type:    conditionDegraded,
				Status:  metav1.ConditionTrue,
				Reason:  "JobCreationFailed",
				Message: err.Error(),
			})
			if statusErr := r.updateStatus(ctx, &inst); statusErr != nil {
				return ctrl.Result{}, errors.Join(err, statusErr)
			}
			return ctrl.Result{}, err
		}
		log.Info(
			"Created install Job",
			"job", jobName,
			"namespace", ctrlNs,
			"serviceAccount", r.installerServiceAccount(),
			"image", r.installerHelmImage(),
			"domain", domain,
			"targetNamespace", epinioNs,
			"version", version,
		)
		setCondition(&inst, metav1.Condition{
			Type:    conditionProgressing,
			Status:  metav1.ConditionTrue,
			Reason:  "Installing",
			Message: "Install Job created",
		})
		if err := r.updateStatus(ctx, &inst); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Update status from Job
	succeeded := job.Status.Succeeded
	failed := job.Status.Failed
	if failed > 0 {
		log.Info("Install Job reported failure", "job", jobName, "namespace", ctrlNs, "failed", failed)
		setCondition(&inst, metav1.Condition{
			Type:    conditionDegraded,
			Status:  metav1.ConditionTrue,
			Reason:  "InstallFailed",
			Message: fmt.Sprintf("Install Job failed (%d failed)", failed),
		})
	} else if succeeded > 0 {
		log.Info("Install Job completed successfully", "job", jobName, "namespace", ctrlNs, "succeeded", succeeded)
		setCondition(&inst, metav1.Condition{
			Type:    conditionAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  "Installed",
			Message: "Epinio installed successfully",
		})
	} else {
		log.Info("Install Job still running", "job", jobName, "namespace", ctrlNs)
		setCondition(&inst, metav1.Condition{
			Type:    conditionProgressing,
			Status:  metav1.ConditionTrue,
			Reason:  "Installing",
			Message: "Install Job is still running",
		})
		if err := r.updateStatus(ctx, &inst); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err := r.updateStatus(ctx, &inst); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *InstallEpinioReconciler) buildInstallJob(name, namespace, configMapName, domain, epinioNs, version string) *batchv1.Job {
	backoffLimit := int32(0) // no retries - keep failed pod around for log inspection
	ttl := int32(300)       // delete job 5 min after completion
	mode := int32(0555)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: r.installerServiceAccount(),
					Containers: []corev1.Container{
						{
							Name:    "installer",
							Image:   r.installerHelmImage(),
							Command: []string{"/bin/sh", "/scripts/install.sh"},
							Env: []corev1.EnvVar{
								{Name: "DOMAIN", Value: domain},
								{Name: "EPINIO_NAMESPACE", Value: epinioNs},
								{Name: "EPINIO_VERSION", Value: version},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "scripts", MountPath: "/scripts", ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "scripts",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
									DefaultMode:          &mode,
								},
							},
						},
					},
				},
			},
		},
	}
}

func (r *InstallEpinioReconciler) reconcileDelete(ctx context.Context, inst *epiniov1alpha1.InstallEpinio, ctrlNs string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(inst, installCleanupFinalizer) {
		return ctrl.Result{}, nil
	}

	cmName := configMapName(inst.Namespace, inst.Name)
	jobName := installJobName(inst.Namespace, inst.Name)

	if err := r.deleteIfExists(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ctrlNs}}, "Job"); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteIfExists(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ctrlNs}}, "ConfigMap"); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(inst, installCleanupFinalizer)
	log.Info("Removing cleanup finalizer")
	if err := r.Update(ctx, inst); err != nil {
		log.Error(err, "failed to remove cleanup finalizer")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *InstallEpinioReconciler) updateStatus(ctx context.Context, inst *epiniov1alpha1.InstallEpinio) error {
	log := logf.FromContext(ctx)
	inst.Status.LastUpdateTime = metav1.Now()
	if err := r.Status().Update(ctx, inst); err != nil {
		log.Error(err, "failed to update status")
		return err
	}

	log.Info("Updated install status", "conditions", summarizeConditions(inst.Status.Conditions))
	return nil
}

func setCondition(inst *epiniov1alpha1.InstallEpinio, condition metav1.Condition) {
	condition.ObservedGeneration = inst.Generation

	if condition.Type == conditionProgressing {
		meta.RemoveStatusCondition(&inst.Status.Conditions, conditionAvailable)
		meta.RemoveStatusCondition(&inst.Status.Conditions, conditionDegraded)
	}
	if condition.Type == conditionAvailable {
		meta.RemoveStatusCondition(&inst.Status.Conditions, conditionProgressing)
		meta.RemoveStatusCondition(&inst.Status.Conditions, conditionDegraded)
	}
	if condition.Type == conditionDegraded {
		meta.RemoveStatusCondition(&inst.Status.Conditions, conditionProgressing)
	}
	meta.SetStatusCondition(&inst.Status.Conditions, condition)
}

func (r *InstallEpinioReconciler) setOwnedByInstall(inst *epiniov1alpha1.InstallEpinio, obj metav1.Object) error {
	if inst.Namespace != obj.GetNamespace() {
		return nil
	}

	return controllerutil.SetControllerReference(inst, obj, r.Scheme)
}

func (r *InstallEpinioReconciler) deleteIfExists(ctx context.Context, obj client.Object, kind string) error {
	log := logf.FromContext(ctx)
	if err := r.Delete(ctx, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		log.Error(err, "failed to delete owned resource during cleanup", "kind", kind, "name", obj.GetName(), "namespace", obj.GetNamespace())
		return err
	}

	log.Info("Deleted owned resource during cleanup", "kind", kind, "name", obj.GetName(), "namespace", obj.GetNamespace())
	return nil
}

func (r *InstallEpinioReconciler) controllerNamespace() string {
	if r.ControllerNamespace == "" {
		return defaultControllerNamespace
	}

	return r.ControllerNamespace
}

func (r *InstallEpinioReconciler) installerServiceAccount() string {
	if r.InstallerServiceAccount == "" {
		return defaultInstallerServiceAccount
	}

	return r.InstallerServiceAccount
}

func (r *InstallEpinioReconciler) installerHelmImage() string {
	if r.InstallerHelmImage == "" {
		return defaultInstallerHelmImage
	}

	return r.InstallerHelmImage
}

func configMapName(namespace, name string) string {
	return configMapNamePrefix + namespace + "-" + name
}

func installJobName(namespace, name string) string {
	return jobNamePrefix + namespace + "-" + name
}

func summarizeConditions(conditions []metav1.Condition) []string {
	summary := make([]string, 0, len(conditions))
	for _, condition := range conditions {
		summary = append(summary, fmt.Sprintf("%s=%s/%s", condition.Type, condition.Status, condition.Reason))
	}

	return summary
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstallEpinioReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&epiniov1alpha1.InstallEpinio{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Named("installepinio").
		Complete(r)
}
