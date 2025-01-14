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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/errors"

	"github.com/shapeblock/operator/utils"
)

// ProjectReconciler reconciles a Project object
type ProjectReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	WebsocketClient *utils.WebsocketClient
}

// +kubebuilder:rbac:groups=apps.shapeblock.io,resources=projects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.shapeblock.io,resources=projects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.shapeblock.io,resources=projects/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Project object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Project instance
	project := &appsv1alpha1.Project{}
	if err := r.Get(ctx, req.NamespacedName, project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Create namespace if it doesn't exist
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: project.Spec.Name,
			Labels: map[string]string{
				"shapeblock.io/project-id": project.Spec.ID,
				"shapeblock.io/managed-by": "shapeblock-operator",
			},
		},
	}

	if err := r.Create(ctx, ns); err != nil {
		if !errors.IsAlreadyExists(err) {
			log.Error(err, "Failed to create namespace")
			return ctrl.Result{}, err
		}
	}

	// Copy registry secret if specified
	if project.Spec.RegistrySecret != "" {
		if err := r.copyRegistrySecret(ctx, project); err != nil {
			log.Error(err, "Failed to copy registry secret")
			return ctrl.Result{}, err
		}
	}

	// Apply resource quotas if specified
	if project.Spec.ResourceQuota != nil {
		if err := r.applyResourceQuota(ctx, project); err != nil {
			log.Error(err, "Failed to apply resource quota")
			return ctrl.Result{}, err
		}
	}

	// Update status
	project.Status.Phase = "Active"
	if err := r.Status().Update(ctx, project); err != nil {
		log.Error(err, "Failed to update project status")
		return ctrl.Result{}, err
	}

	// Notify ShapeBlock server
	if r.WebsocketClient != nil {
		r.WebsocketClient.SendStatus(utils.StatusUpdate{
			ResourceType: "Project",
			Name:         project.Name,
			Namespace:    project.Namespace,
			Status:       "Active",
			Message:      "Project setup completed",
		})
	}

	return ctrl.Result{}, nil
}

func (r *ProjectReconciler) copyRegistrySecret(ctx context.Context, project *appsv1alpha1.Project) error {
	// Get source secret
	sourceSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      project.Spec.RegistrySecret,
		Namespace: "shapeblock-system", // predefined namespace for source secrets
	}, sourceSecret); err != nil {
		return err
	}

	// Create new secret in project namespace
	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "registry-credentials",
			Namespace: project.Spec.Name,
		},
		Type: sourceSecret.Type,
		Data: sourceSecret.Data,
	}

	return r.Create(ctx, newSecret)
}

func (r *ProjectReconciler) applyResourceQuota(ctx context.Context, project *appsv1alpha1.Project) error {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "project-quota",
			Namespace: project.Spec.Name,
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: project.Spec.ResourceQuota.ToResourceList(),
		},
	}

	return r.Create(ctx, quota)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Project{}).
		Complete(r)
}
