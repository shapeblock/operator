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

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1 "github.com/shapeblock/operator/api/v1"
	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	"github.com/shapeblock/operator/utils"
	"gopkg.in/yaml.v3"
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	WebsocketClient *utils.WebsocketClient
}

// +kubebuilder:rbac:groups=apps.shapeblock.io,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.shapeblock.io,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.shapeblock.io,resources=services/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Service object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	service := &appsv1alpha1.Service{}
	if err := r.Get(ctx, req.NamespacedName, service); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Create HelmChart CR for k3s helm-controller
	helmChart := &helmv1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service.Name,
			Namespace: service.Namespace,
		},
		Spec: helmv1.HelmChartSpec{
			Chart:   "nxs-universal-chart", // Using nixys/nxs-universal-chart
			Version: "1.0.0",
			Repo:    "https://registry.nixys.ru/chartrepo/public",
			Set: []helmv1.Value{
				{
					Name:  "image.repository",
					Value: service.Spec.Image.Repository,
				},
				{
					Name:  "image.tag",
					Value: service.Spec.Image.Tag,
				},
				// Add other helm values from service spec
			},
			// Add values from service.Spec.HelmValues
			ValuesContent: string(service.Spec.HelmValues),
		},
	}

	// Apply the HelmChart
	if err := r.Apply(ctx, helmChart, service); err != nil {
		service.Status.Phase = "Failed"
		service.Status.Message = fmt.Sprintf("Failed to apply helm chart: %v", err)
		r.Status().Update(ctx, service)
		return ctrl.Result{}, err
	}

	// Update status
	service.Status.Phase = "Deployed"
	service.Status.Message = "Service deployed successfully"
	if err := r.Status().Update(ctx, service); err != nil {
		return ctrl.Result{}, err
	}

	// Notify ShapeBlock server
	if r.WebsocketClient != nil {
		r.WebsocketClient.SendStatus(utils.StatusUpdate{
			ResourceType: "Service",
			Name:         service.Name,
			Namespace:    service.Namespace,
			Status:       string(service.Status.Phase),
			Message:      service.Status.Message,
		})
	}

	return ctrl.Result{}, nil
}

func (r *ServiceReconciler) Apply(ctx context.Context, helmChart *helmv1.HelmChart, service *appsv1alpha1.Service) error {
	// Check if HelmChart already exists
	existing := &helmv1.HelmChart{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      helmChart.Name,
		Namespace: helmChart.Namespace,
	}, existing)

	if err != nil && errors.IsNotFound(err) {
		// Create new HelmChart
		if err := r.Create(ctx, helmChart); err != nil {
			return fmt.Errorf("failed to create helm chart: %v", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get helm chart: %v", err)
	} else {
		// Update existing HelmChart
		existing.Spec = helmChart.Spec
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update helm chart: %v", err)
		}
	}

	return nil
}

func (r *ServiceReconciler) createHelmChart(ctx context.Context, service *appsv1alpha1.Service) error {
	// Prepare values
	values := make(map[string]interface{})

	// If helmValues is provided, use it as base
	if service.Spec.HelmValues != nil {
		if err := json.Unmarshal(service.Spec.HelmValues.Raw, &values); err != nil {
			return fmt.Errorf("failed to parse helm values: %v", err)
		}
	}

	// Convert values to YAML
	valuesYAML, err := yaml.Marshal(values)
	if err != nil {
		return fmt.Errorf("failed to marshal helm values: %v", err)
	}

	// Create HelmChart CR
	helmChart := &helmv1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service.Name,
			Namespace: service.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "shapeblock-operator",
				"app.kubernetes.io/name":       service.Spec.AppName,
			},
		},
		Spec: helmv1.HelmChartSpec{
			Chart:         service.Spec.Chart.Name,
			Version:       service.Spec.Chart.Version,
			ValuesContent: string(valuesYAML),
		},
	}

	// Set repo based on provided configuration
	if service.Spec.Chart.RepoURL != "" {
		helmChart.Spec.Repo = service.Spec.Chart.RepoURL
	} else if service.Spec.Chart.Repo != "" {
		// Use predefined repos
		switch service.Spec.Chart.Repo {
		case "bitnami":
			helmChart.Spec.Repo = "https://charts.bitnami.com/bitnami"
		case "stable":
			helmChart.Spec.Repo = "https://charts.helm.sh/stable"
		default:
			return fmt.Errorf("unknown chart repository: %s", service.Spec.Chart.Repo)
		}
	}

	return r.Create(ctx, helmChart)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Service{}).
		Complete(r)
}
