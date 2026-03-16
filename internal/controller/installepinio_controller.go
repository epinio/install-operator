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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	epiniov1alpha1 "apps.example.com/install-epinio/api/v1alpha1"
)

// Install script runs in the Job container (helm + sh). Uses env DOMAIN and EPINIO_NAMESPACE.
const installScript = `#!/bin/sh
set -e
echo "Epinio installer: domain=${DOMAIN}, epinio namespace=${EPINIO_NAMESPACE}"
helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx 2>/dev/null || true
helm repo add jetstack https://charts.jetstack.io 2>/dev/null || true
helm repo add epinio https://epinio.github.io/helm-charts 2>/dev/null || true
helm repo update
helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx \
  --namespace ingress-nginx --create-namespace \
  --set controller.ingressClassResource.default=true \
  --set controller.service.type=LoadBalancer
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --set crds.enabled=true \
  --set extraArgs={--enable-certificate-owner-ref=true}
EPINIO_VERSION_ARGS=""
if [ -n "${EPINIO_VERSION}" ]; then
  EPINIO_VERSION_ARGS="--version ${EPINIO_VERSION}"
fi
helm upgrade --install epinio epinio/epinio \
  --namespace "${EPINIO_NAMESPACE}" --create-namespace \
  ${EPINIO_VERSION_ARGS} \
  --set global.domain="${DOMAIN}"
echo "Epinio install completed successfully."
`

const (
	configMapNamePrefix = "epinio-install-script-"
	jobNamePrefix       = "epinio-installer-"
)

// InstallEpinioReconciler reconciles a InstallEpinio object
type InstallEpinioReconciler struct {
	client.Client
	Scheme              *runtime.Scheme
	ControllerNamespace string
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

	domain := strings.TrimSpace(inst.Spec.Domain)
	if domain == "" {
		domain = "epinio.local"
	}
	inst.Spec.Domain = domain
	epinioNs := strings.TrimSpace(inst.Spec.TargetNamespace)
	if epinioNs == "" {
		epinioNs = "epinio"
	}
	inst.Spec.TargetNamespace = epinioNs
	version := strings.TrimSpace(inst.Spec.Version)

	// Ensure epinio namespace exists (optional; helm --create-namespace will create it too)
	ns := &corev1.Namespace{}
	ns.Name = epinioNs
	if err := r.Get(ctx, client.ObjectKeyFromObject(ns), ns); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			log.Error(err, "failed to create namespace", "namespace", epinioNs)
			return ctrl.Result{}, err
		}
	}

	ctrlNs := r.ControllerNamespace
	if ctrlNs == "" {
		ctrlNs = "system"
	}
	cmName := configMapNamePrefix + inst.Namespace + "-" + inst.Name
	jobName := jobNamePrefix + inst.Namespace + "-" + inst.Name

	// Create or update ConfigMap in controller namespace (so install Job can use controller-manager SA).
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
	existingCM := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), existingCM); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, cm); err != nil {
				log.Error(err, "failed to create ConfigMap")
				return ctrl.Result{}, err
			}
		} else {
			return ctrl.Result{}, err
		}
	} else {
		existingCM.Data = cm.Data
		if err := r.Update(ctx, existingCM); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check for existing Job (in controller namespace)
	job := &batchv1.Job{}
	jobKey := client.ObjectKey{Namespace: ctrlNs, Name: jobName}
	if err := r.Get(ctx, jobKey, job); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// Create Job in controller namespace so it can use controller-manager SA.
		job = r.buildInstallJob(jobName, ctrlNs, cmName, domain, epinioNs, version)
		job.Labels = map[string]string{
			"install-epinio.io/cr-namespace": inst.Namespace,
			"install-epinio.io/cr-name":      inst.Name,
		}
		if err := r.Create(ctx, job); err != nil {
			log.Error(err, "failed to create install Job")
			meta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
				Type:    "Degraded",
				Status:  metav1.ConditionTrue,
				Reason:  "JobCreationFailed",
				Message: err.Error(),
			})
			_ = r.Status().Update(ctx, &inst)
			return ctrl.Result{}, err
		}
		log.Info("created install Job", "job", jobName, "namespace", ctrlNs)
		setCondition(&inst, metav1.Condition{
			Type:    "Progressing",
			Status:  metav1.ConditionTrue,
			Reason:  "Installing",
			Message: "Install Job created",
		})
		inst.Status.RunningStatus = "Installing"
		if err := r.updateStatus(ctx, &inst); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Update status from Job
	succeeded := job.Status.Succeeded
	failed := job.Status.Failed
	if succeeded > 0 {
		setCondition(&inst, metav1.Condition{
			Type:    "Available",
			Status:  metav1.ConditionTrue,
			Reason:  "Installed",
			Message: "Epinio installed successfully",
		})
		inst.Status.RunningStatus = "Installed"
	} else if failed > 0 {
		setCondition(&inst, metav1.Condition{
			Type:    "Degraded",
			Status:  metav1.ConditionTrue,
			Reason:  "InstallFailed",
			Message: fmt.Sprintf("Install Job failed (%d failed)", failed),
		})
		inst.Status.RunningStatus = "Failed"
	} else {
		setCondition(&inst, metav1.Condition{
			Type:    "Progressing",
			Status:  metav1.ConditionTrue,
			Reason:  "Installing",
			Message: "Install Job is still running",
		})
		inst.Status.RunningStatus = "Installing"
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
	one := int32(1)
	mode := int32(0555)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: &one,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					ServiceAccountName: "controller-manager",
					Containers: []corev1.Container{
						{
							Name:    "installer",
							Image:   "alpine/helm:3.18",
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

func (r *InstallEpinioReconciler) updateStatus(ctx context.Context, inst *epiniov1alpha1.InstallEpinio) error {
	inst.Status.LastUpdateTime = metav1.Now()
	inst.Status.LastTransitionTime = metav1.Now()
	return r.Status().Update(ctx, inst)
}

func setCondition(inst *epiniov1alpha1.InstallEpinio, condition metav1.Condition) {
	if condition.Type == "Progressing" {
		meta.RemoveStatusCondition(&inst.Status.Conditions, "Available")
		meta.RemoveStatusCondition(&inst.Status.Conditions, "Degraded")
	}
	if condition.Type == "Available" {
		meta.RemoveStatusCondition(&inst.Status.Conditions, "Progressing")
		meta.RemoveStatusCondition(&inst.Status.Conditions, "Degraded")
	}
	if condition.Type == "Degraded" {
		meta.RemoveStatusCondition(&inst.Status.Conditions, "Progressing")
	}
	meta.SetStatusCondition(&inst.Status.Conditions, condition)
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstallEpinioReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&epiniov1alpha1.InstallEpinio{}).
		Named("installepinio").
		Complete(r)
}
