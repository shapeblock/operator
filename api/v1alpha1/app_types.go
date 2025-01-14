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

	// Build configuration
	Build BuildSpec `json:"build"`

	// Service configuration
	Service ServiceSpec `json:"service"`
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

	// Build environment variables
	Env []BuildVar `json:"env,omitempty"`
}

// AppStatus defines the observed state of App
type AppStatus struct {
	// Current phase of the application: Pending, Building, Deploying, Running, Failed
	Phase string `json:"phase,omitempty"`

	// Human-readable message indicating details about current phase
	Message string `json:"message,omitempty"`

	// Latest successful build
	LatestBuild string `json:"latestBuild,omitempty"`

	// Current service deployment
	CurrentService string `json:"currentService,omitempty"`

	// URL where the application is accessible
	URL string `json:"url,omitempty"`

	// Last time the status was updated
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Display Name",type="string",JSONPath=".spec.displayName"
//+kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.phase"
//+kubebuilder:printcolumn:name="URL",type="string",JSONPath=".status.url"
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
