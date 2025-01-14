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
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helmv1 "github.com/k3s-io/helm-controller/pkg/apis/helm.cattle.io/v1"
	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//+kubebuilder:rbac:groups=apps.shapeblock.io,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps.shapeblock.io,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=helm.cattle.io,resources=helmcharts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch

type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	service := &appsv1alpha1.Service{}
	if err := r.Get(ctx, req.NamespacedName, service); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Initialize status if needed
	if service.Status.Phase == "" {
		service.Status.Phase = "Pending"
		service.Status.Message = "Starting helm chart installation"
		if err := r.Status().Update(ctx, service); err != nil {
			return ctrl.Result{}, err
		}
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
			Chart:           service.Spec.Chart.Name,
			Version:         service.Spec.Chart.Version,
			Repo:            service.Spec.Chart.RepoURL,
			TargetNamespace: service.Namespace,
		},
	}

	// Add helm values if provided
	if len(service.Spec.HelmValues.Raw) > 0 {
		values, err := yaml.Marshal(service.Spec.HelmValues.Raw)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to marshal helm values: %v", err)
		}
		helmChart.Spec.ValuesContent = string(values)
	}

	// Create or update HelmChart
	if err := r.Create(ctx, helmChart); err != nil {
		if client.IgnoreAlreadyExists(err) != nil {
			return ctrl.Result{}, err
		}
		if err := r.Update(ctx, helmChart); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check helm install job status
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList,
		client.InNamespace(service.Namespace),
		client.MatchingLabels{"helmcharts.helm.cattle.io/chart": service.Name},
	); err != nil {
		return ctrl.Result{}, err
	}

	if len(jobList.Items) > 0 {
		job := jobList.Items[len(jobList.Items)-1]
		for _, condition := range job.Status.Conditions {
			if condition.Type == "Complete" && condition.Status == "True" {
				service.Status.Phase = "Deployed"
				service.Status.Message = "Helm chart installed successfully"
				service.Status.HelmRelease = service.Name
				return ctrl.Result{}, r.Status().Update(ctx, service)
			}
			if condition.Type == "Failed" && condition.Status == "True" {
				service.Status.Phase = "Failed"
				service.Status.Message = fmt.Sprintf("Helm installation failed: %s", condition.Message)
				return ctrl.Result{}, r.Status().Update(ctx, service)
			}
		}
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Service{}).
		Complete(r)
}
