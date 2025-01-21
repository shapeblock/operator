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
	// AppName references the App CR
	AppName string `json:"appName"`

	// Git reference to build (commit, branch, tag)
	// +kubebuilder:validation:Optional
	GitRef string `json:"gitRef,omitempty"`

	// ImageTag for this specific build
	// +kubebuilder:validation:Optional
	ImageTag string `json:"imageTag,omitempty"`

	// Additional build environment variables
	BuildVars []BuildVar `json:"buildVars,omitempty"`

	// HelmValues for deployment
	HelmValues *runtime.RawExtension `json:"helmValues,omitempty"`
}

type SourceSpec struct {
	// Git repository information
	Git *GitSource `json:"git,omitempty"`
}

type GitSource struct {
	// URL of the git repository
	URL string `json:"url"`

	// Reference to checkout (branch, tag, commit)
	Ref string `json:"ref"`

	// Whether the repository is private
	IsPrivate bool `json:"isPrivate,omitempty"`
}

type BuildVar struct {
	// Name of the build variable
	Key string `json:"key"`

	// Value of the build variable
	Value string `json:"value"`
}

// AppBuildStatus defines the observed state of AppBuild
type AppBuildStatus struct {
	// Current phase of the build
	// +kubebuilder:validation:Enum=Pending;Building;Deploying;Completed;Failed
	// +kubebuilder:default=Pending
	Phase string `json:"phase,omitempty"`

	// Human-readable message
	Message string `json:"message,omitempty"`

	// Build pod details for log streaming
	// Only set for dockerfile and buildpack builds
	PodName string `json:"podName,omitempty"`

	// Git commit SHA of the code being built
	GitCommit string `json:"gitCommit,omitempty"`

	// Image tag for the built container
	ImageTag string `json:"imageTag,omitempty"`

	// Timestamps
	StartTime      *metav1.Time `json:"startTime,omitempty"`
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="App",type="string",JSONPath=".spec.appName"
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
