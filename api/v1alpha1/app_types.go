package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AppSpec defines the desired state of App
type AppSpec struct {
	// DisplayName is the human-readable name of the application
	DisplayName string `json:"displayName"`

	// Description of the application
	Description string `json:"description,omitempty"`

	// Git repository configuration
	Git GitSpec `json:"git"`

	// Registry configuration for storing built images
	Registry RegistrySpec `json:"registry"`

	// Build configuration template
	Build BuildSpec `json:"build"`
}

type GitSpec struct {
	// URL of the git repository
	URL string `json:"url"`

	// Branch to use by default
	Branch string `json:"branch,omitempty"`

	// Secret name containing git credentials
	SecretName string `json:"secretName,omitempty"`

	// IsPrivate indicates if the git repository is private
	IsPrivate bool `json:"isPrivate,omitempty"`
}

type RegistrySpec struct {
	// URL of the container registry
	URL string `json:"url"`

	// Secret name containing registry credentials
	SecretName string `json:"secretName,omitempty"`
}

type BuildSpec struct {
	// Type of build: dockerfile, buildpack, or prebuilt
	Type string `json:"type"`

	// Builder image for buildpack builds
	BuilderImage string `json:"builderImage,omitempty"`

	// Prebuilt image
	Image string `json:"image,omitempty"`
}

// AppStatus defines the observed state of App
type AppStatus struct {
	// Current phase of the application
	Phase string `json:"phase,omitempty"`

	// Human-readable message
	Message string `json:"message,omitempty"`

	// Latest successful build
	LatestBuild string `json:"latestBuild,omitempty"`

	// Last time the status was updated
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Display Name",type="string",JSONPath=".spec.displayName"
//+kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// App is the Schema for the apps API
type App struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppSpec   `json:"spec,omitempty"`
	Status AppStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// AppList contains a list of App
type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []App `json:"items"`
}

func init() {
	SchemeBuilder.Register(&App{}, &AppList{})
}
