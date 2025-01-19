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
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1 "github.com/k3s-io/helm-controller/pkg/apis/helm.cattle.io/v1"
	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	"github.com/shapeblock/operator/pkg/utils"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//+kubebuilder:rbac:groups=apps.shapeblock.io,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps.shapeblock.io,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=helm.cattle.io,resources=helmcharts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete

const serviceFinalizer = "apps.shapeblock.io/service-cleanup"

type ServiceReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	WebsocketClient *utils.WebsocketClient
}

// CreateOrPatch creates or patches a Kubernetes object using server-side apply
func (r *ServiceReconciler) CreateOrPatch(ctx context.Context, obj client.Object, mutate func() error) error {
	// Get the current state of the object
	key := client.ObjectKeyFromObject(obj)
	current := obj.DeepCopyObject().(client.Object)

	err := r.Get(ctx, key, current)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		// Object doesn't exist, create it
		return r.Create(ctx, obj)
	}

	// Object exists, apply the mutation
	if err := mutate(); err != nil {
		return err
	}

	// Patch the object
	patch := client.MergeFrom(current)
	return r.Patch(ctx, obj, patch)
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	service := &appsv1alpha1.Service{}
	if err := r.Get(ctx, req.NamespacedName, service); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !service.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, service)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(service, serviceFinalizer) {
		controllerutil.AddFinalizer(service, serviceFinalizer)
		if err := r.Update(ctx, service); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Initialize status if needed
	if service.Status.Phase == "" {
		log.Info("Initializing service status",
			"service", service.Name,
			"app", service.Spec.AppName)
		service.Status.Phase = "Pending"
		service.Status.Message = "Starting helm chart installation"
		if err := r.Status().Update(ctx, service); err != nil {
			return ctrl.Result{}, err
		}
		r.sendServiceStatus(service)
	}

	// Validate required fields
	if service.Name == "" {
		return ctrl.Result{}, fmt.Errorf("service name is empty")
	}
	if service.Namespace == "" {
		return ctrl.Result{}, fmt.Errorf("service namespace is empty")
	}
	if service.Spec.Chart.Name == "" {
		return ctrl.Result{}, fmt.Errorf("chart name is empty")
	}

	// Create or update HelmChart
	helmChart := &helmv1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service.Name,
			Namespace: service.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "shapeblock-operator",
				"app.kubernetes.io/name":       service.Spec.AppName,
			},
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "helm.cattle.io/v1",
			Kind:       "HelmChart",
		},
		Spec: helmv1.HelmChartSpec{
			Chart:           service.Spec.Chart.Name,
			Version:         service.Spec.Chart.Version,
			Repo:            service.Spec.Chart.RepoURL,
			TargetNamespace: service.Namespace,
		},
	}

	// Add helm values if provided
	if len(service.Spec.HelmValues.Raw) > 0 {
		log.Info("Adding helm values to chart",
			"service", service.Name,
			"chart", service.Spec.Chart.Name)
		values := make(map[string]interface{})
		if err := yaml.Unmarshal(service.Spec.HelmValues.Raw, &values); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to unmarshal helm values: %v", err)
		}
		valuesYAML, err := yaml.Marshal(values)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to marshal helm values: %v", err)
		}
		helmChart.Spec.ValuesContent = string(valuesYAML)
	}

	// Create or update the HelmChart using server-side apply
	if err := r.CreateOrPatch(ctx, helmChart, func() error {
		// No additional mutations needed as the object is already configured
		return nil
	}); err != nil {
		log.Error(err, "Failed to create/update HelmChart",
			"service", service.Name,
			"chart", service.Spec.Chart.Name)
		return ctrl.Result{}, err
	}

	// Check helm install job status
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList,
		client.InNamespace(service.Namespace),
		client.MatchingLabels{"helmcharts.helm.cattle.io/chart": service.Name},
	); err != nil {
		log.Error(err, "Failed to list helm jobs",
			"service", service.Name,
			"namespace", service.Namespace)
		return ctrl.Result{}, err
	}

	if len(jobList.Items) > 0 {
		job := jobList.Items[len(jobList.Items)-1]
		for _, condition := range job.Status.Conditions {
			if condition.Type == "Complete" && condition.Status == "True" {
				log.Info("Helm chart installation completed",
					"service", service.Name,
					"chart", service.Spec.Chart.Name)
				service.Status.Phase = "Deployed"
				service.Status.Message = "Helm chart installed successfully"
				service.Status.HelmRelease = service.Name
				r.sendServiceStatus(service)
				return ctrl.Result{}, r.Status().Update(ctx, service)
			}
			if condition.Type == "Failed" && condition.Status == "True" {
				log.Error(nil, "Helm chart installation failed",
					"service", service.Name,
					"chart", service.Spec.Chart.Name,
					"message", condition.Message)
				service.Status.Phase = "Failed"
				service.Status.Message = fmt.Sprintf("Helm installation failed: %s", condition.Message)
				r.sendServiceStatus(service)
				return ctrl.Result{}, r.Status().Update(ctx, service)
			}
		}
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *ServiceReconciler) handleDeletion(ctx context.Context, service *appsv1alpha1.Service) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(service, serviceFinalizer) {
		// Delete the HelmChart CR
		helmChart := &helmv1.HelmChart{
			ObjectMeta: metav1.ObjectMeta{
				Name:      service.Name,
				Namespace: service.Namespace,
			},
		}

		if err := r.Delete(ctx, helmChart); err != nil {
			if !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete HelmChart",
					"service", service.Name,
					"namespace", service.Namespace)
				return ctrl.Result{}, err
			}
		}

		log.Info("Deleted HelmChart",
			"service", service.Name,
			"namespace", service.Namespace)

		// List and delete PVCs with the app label
		pvcList := &corev1.PersistentVolumeClaimList{}
		if err := r.List(ctx, pvcList,
			client.InNamespace(service.Namespace),
			client.MatchingLabels{
				"app.kubernetes.io/instance": service.Name,
			}); err != nil {
			log.Error(err, "Failed to list PVCs",
				"service", service.Name,
				"namespace", service.Namespace)
			return ctrl.Result{}, err
		}

		for _, pvc := range pvcList.Items {
			if err := r.Delete(ctx, &pvc); err != nil {
				if !errors.IsNotFound(err) {
					log.Error(err, "Failed to delete PVC",
						"pvc", pvc.Name,
						"namespace", pvc.Namespace)
					return ctrl.Result{}, err
				}
			}
			log.Info("Deleted PVC",
				"pvc", pvc.Name,
				"namespace", pvc.Namespace)
		}

		// Send final status update
		service.Status.Phase = "Deleted"
		service.Status.Message = "Service, HelmChart and associated PVCs deleted"
		r.sendServiceStatus(service)

		// Remove finalizer
		controllerutil.RemoveFinalizer(service, serviceFinalizer)
		if err := r.Update(ctx, service); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Service{}).
		Complete(r)
}

func (r *ServiceReconciler) sendServiceStatus(service *appsv1alpha1.Service) {
	if r.WebsocketClient == nil {
		return
	}

	update := utils.NewStatusUpdate("Service", service.Name, service.Namespace)
	update.Status = service.Status.Phase
	update.Message = service.Status.Message
	update.Labels = map[string]string{
		"app.kubernetes.io/name":   service.Spec.AppName,
		"service.shapeblock.io/id": service.Name,
	}

	r.WebsocketClient.SendStatus(update)
}
