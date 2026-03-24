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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// InstallEpinioSpec defines the desired state of InstallEpinio
type InstallEpinioSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// +optional
	// +kubebuilder:validation:Pattern=`^v?[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$`
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?)(\.([a-z0-9]([-a-z0-9]*[a-z0-9])?))*$`
	// +kubebuilder:validation:MaxLength=253
	Domain string `json:"domain,omitempty"`
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	TargetNamespace string `json:"targetNamespace,omitempty"`
	// +optional
	ImagePullSecret string `json:"imagePullSecret,omitempty"`
	// +optional
	ImageRegistry string `json:"imageRegistry,omitempty"`
	// +optional
	ImageRegistryUsername string `json:"imageRegistryUsername,omitempty"`
	// +optional
	ImageRegistryPassword string `json:"imageRegistryPassword,omitempty"`

	// NginxReleaseName is the existing Helm release name for ingress-nginx.
	// If set, the operator will skip installing ingress-nginx and use this
	// release name to verify it is present (e.g. "rke2-ingress-nginx").
	// If empty, ingress-nginx will be installed fresh.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	NginxReleaseName string `json:"nginxReleaseName,omitempty"`

	// NginxReleaseNamespace is the namespace of the existing ingress-nginx release.
	// Defaults to "ingress-nginx" if NginxReleaseName is set but this is empty.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	NginxReleaseNamespace string `json:"nginxReleaseNamespace,omitempty"`

	// CertManagerReleaseName is the existing Helm release name for cert-manager.
	// If set, the operator will skip installing cert-manager and use this
	// release name to verify it is present (e.g. "rancher-cert-manager").
	// If empty, cert-manager will be installed fresh.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	CertManagerReleaseName string `json:"certManagerReleaseName,omitempty"`

	// CertManagerReleaseNamespace is the namespace of the existing cert-manager release.
	// Defaults to "cert-manager" if CertManagerReleaseName is set but this is empty.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	CertManagerReleaseNamespace string `json:"certManagerReleaseNamespace,omitempty"`
}

// InstallEpinioStatus defines the observed state of InstallEpinio.
type InstallEpinioStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// +optional
	LastUpdateTime metav1.Time `json:"lastUpdateTime,omitempty"`

	// conditions represent the current state of the InstallEpinio resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// InstallEpinio is the Schema for the installepinios API
type InstallEpinio struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of InstallEpinio
	// +required
	Spec InstallEpinioSpec `json:"spec"`

	// status defines the observed state of InstallEpinio
	// +optional
	Status InstallEpinioStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// InstallEpinioList contains a list of InstallEpinio
type InstallEpinioList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []InstallEpinio `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InstallEpinio{}, &InstallEpinioList{})
}
