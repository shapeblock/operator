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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	"github.com/shapeblock/operator/pkg/utils"
)

// ProjectReconciler reconciles a Project object
type ProjectReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	WebsocketClient *utils.WebsocketClient
}

//+kubebuilder:rbac:groups=apps.shapeblock.io,resources=projects,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps.shapeblock.io,resources=projects/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps.shapeblock.io,resources=projects/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Project instance
	project := &appsv1alpha1.Project{}
	if err := r.Get(ctx, req.NamespacedName, project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Initialize status if not set
	if project.Status.Phase == "" {
		project.Status.Phase = "Initializing"
		project.Status.LastUpdated = &metav1.Time{Time: time.Now()}
		if err := r.Status().Update(ctx, project); err != nil {
			return ctrl.Result{}, err
		}
		r.sendProjectStatus(project)
	}

	// Create namespace if it doesn't exist
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: project.Spec.Name,
			Labels: map[string]string{
				"shapeblock.io/project-id": project.Name,
				"shapeblock.io/managed-by": "shapeblock-operator",
			},
		},
	}

	if err := r.Create(ctx, ns); err != nil {
		if !errors.IsAlreadyExists(err) {
			log.Error(err, "Failed to create namespace")
			return r.failProject(ctx, project, fmt.Sprintf("Failed to create namespace: %v", err))
		}
		log.Info("Namespace already exists", "namespace", project.Spec.Name)
	} else {
		log.Info("Successfully created namespace",
			"namespace", project.Spec.Name,
			"projectId", project.Name)
	}

	// Copy registry secret - either specified or default
	registrySecretName := project.Spec.RegistrySecret
	if registrySecretName == "" {
		registrySecretName = "registry-creds" // Default secret name
		log.Info("No registry secret specified, using default",
			"secretName", registrySecretName)
	}

	if err := r.copyRegistrySecret(ctx, project, registrySecretName); err != nil {
		log.Error(err, "Failed to copy registry secret")
		return r.failProject(ctx, project, fmt.Sprintf("Failed to copy registry secret: %v", err))
	}
	log.Info("Successfully copied registry secret",
		"from", registrySecretName,
		"to", "registry-creds",
		"namespace", project.Spec.Name)

	// Update status to Active
	if project.Status.Phase != "Active" {
		project.Status.Phase = "Active"
		project.Status.Message = "Project setup completed"
		project.Status.LastUpdated = &metav1.Time{Time: time.Now()}
		if err := r.Status().Update(ctx, project); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Project is now active",
			"project", project.Name,
			"namespace", project.Spec.Name)
		r.sendProjectStatus(project)
	}

	return ctrl.Result{}, nil
}

func (r *ProjectReconciler) copyRegistrySecret(ctx context.Context, project *appsv1alpha1.Project, sourceSecretName string) error {
	log := log.FromContext(ctx)

	// Check if secret already exists in target namespace
	existingSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "registry-creds",
		Namespace: project.Spec.Name,
	}, existingSecret)

	if err == nil {
		log.Info("Registry secret already exists in namespace, skipping copy",
			"namespace", project.Spec.Name,
			"secretName", "registry-creds")
		return nil
	}

	if !errors.IsNotFound(err) {
		return err
	}

	// Get source secret
	sourceSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      sourceSecretName,
		Namespace: "shapeblock",
	}, sourceSecret); err != nil {
		return err
	}

	// Create new secret
	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "registry-creds",
			Namespace: project.Spec.Name,
			Labels: map[string]string{
				"shapeblock.io/project-id": project.Name,
				"shapeblock.io/managed-by": "shapeblock-operator",
			},
		},
		Type: sourceSecret.Type,
		Data: sourceSecret.Data,
	}

	return r.Create(ctx, newSecret)
}

func (r *ProjectReconciler) failProject(ctx context.Context, project *appsv1alpha1.Project, message string) (ctrl.Result, error) {
	project.Status.Phase = "Failed"
	project.Status.Message = message
	project.Status.LastUpdated = &metav1.Time{Time: time.Now()}
	if err := r.Status().Update(ctx, project); err != nil {
		return ctrl.Result{}, err
	}
	r.sendProjectStatus(project)
	return ctrl.Result{}, fmt.Errorf(message)
}

func (r *ProjectReconciler) sendProjectStatus(project *appsv1alpha1.Project) {
	if r.WebsocketClient == nil {
		return
	}

	update := utils.NewStatusUpdate("Project", project.Name, project.Namespace)
	update.Status = project.Status.Phase
	update.Message = project.Status.Message
	update.Labels = map[string]string{
		"shapeblock.io/project-id": project.Name,
	}

	r.WebsocketClient.SendStatus(update)
}

func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Project{}).
		Complete(r)
}
