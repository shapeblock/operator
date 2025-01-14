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

// AppBuildSpec defines the desired state of AppBuild
type AppBuildSpec struct {
	// AppName is the name of the application
	AppName string `json:"appName"`

	// ImageTag is the tag for the built image
	ImageTag string `json:"imageTag"`

	// BuildType specifies how to build the image: dockerfile, buildpack, or prebuilt
	BuildType string `json:"buildType"`

	// Git repository configuration
	Git GitSpec `json:"git,omitempty"`

	// Registry configuration
	RegistryURL string `json:"registryURL"`

	// Previous image for buildpack builds
	PreviousImage string `json:"previousImage,omitempty"`

	// Builder image for buildpack builds
	BuilderImage string `json:"builderImage,omitempty"`

	// Build environment variables
	BuildVars []BuildVar `json:"buildVars,omitempty"`

	// Indicates if the git repository is private
	IsPrivate bool `json:"isPrivate,omitempty"`

	// Raw helm values to override default nxs-universal-chart values
	// +optional
	HelmValues *runtime.RawExtension `json:"helmValues,omitempty"`
}

type GitSpec struct {
	// URL of the git repository
	URL string `json:"url"`

	// Git reference (branch, tag, or commit)
	Ref string `json:"ref"`

	// SSH key secret name for private repositories
	SSHKeySecret string `json:"sshKeySecret,omitempty"`
}

type BuildVar struct {
	// Name of the build variable
	Key string `json:"key"`

	// Value of the build variable
	Value string `json:"value"`
}

// AppBuildStatus defines the observed state of AppBuild
type AppBuildStatus struct {
	// Current phase of the build: Pending, Building, Completed, Failed
	Phase string `json:"phase,omitempty"`

	// Human-readable message indicating details about current phase
	Message string `json:"message,omitempty"`

	// Name of the pod running the build
	PodName string `json:"podName,omitempty"`

	// Namespace of the pod running the build
	PodNamespace string `json:"podNamespace,omitempty"`

	// Time when the build started
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// Time when the build completed
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Complete build logs (populated after build completion)
	Logs string `json:"logs,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AppBuild is the Schema for the appbuilds API
type AppBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppBuildSpec   `json:"spec,omitempty"`
	Status AppBuildStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// AppBuildList contains a list of AppBuild
type AppBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppBuild `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AppBuild{}, &AppBuildList{})
}
