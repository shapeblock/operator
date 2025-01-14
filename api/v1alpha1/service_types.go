/*
Copyright 2025.

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
	"k8s.io/apimachinery/pkg/runtime"
)

// ServiceSpec defines the desired state of Service
type ServiceSpec struct {
	// AppName is the name of the application
	AppName string `json:"appName"`

	// Chart configuration
	Chart ChartSpec `json:"chart"`

	// Image configuration
	Image ImageSpec `json:"image"`

	// Raw helm values to be passed to the chart
	HelmValues runtime.RawExtension `json:"helmValues,omitempty"`
}

// ChartSpec defines the Helm chart configuration
type ChartSpec struct {
	// Name of the chart
	Name string `json:"name"`

	// Version of the chart
	Version string `json:"version,omitempty"`

	// Predefined repository name (e.g., "bitnami", "stable")
	Repo string `json:"repo,omitempty"`

	// Custom repository URL
	RepoURL string `json:"repoURL,omitempty"`
}

type ImageSpec struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	PullPolicy string `json:"pullPolicy,omitempty"`
}

// ServiceStatus defines the observed state of Service
type ServiceStatus struct {
	// Current phase of the service: Pending, Deploying, Deployed, Failed
	Phase string `json:"phase,omitempty"`

	// Human-readable message indicating details about current phase
	Message string `json:"message,omitempty"`

	// Name of the Helm release
	HelmRelease string `json:"helmRelease,omitempty"`

	// URL where the service is accessible
	URL string `json:"url,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="App",type="string",JSONPath=".spec.appName"
//+kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.url"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Service is the Schema for the services API
type Service struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceSpec   `json:"spec,omitempty"`
	Status ServiceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ServiceList contains a list of Service
type ServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Service `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Service{}, &ServiceList{})
}
