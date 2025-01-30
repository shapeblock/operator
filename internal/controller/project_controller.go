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
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	"github.com/shapeblock/operator/pkg/utils"
)

const projectFinalizer = "apps.shapeblock.io/finalizer"

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
//+kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete

func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Project instance
	project := &appsv1alpha1.Project{}
	if err := r.Get(ctx, req.NamespacedName, project); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !project.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, project)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(project, projectFinalizer) {
		controllerutil.AddFinalizer(project, projectFinalizer)
		if err := r.Update(ctx, project); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize status if not set
	if project.Status.Phase == "" {
		if err := r.updateProjectStatus(ctx, project, "Initializing", ""); err != nil {
			return ctrl.Result{}, err
		}
		// Requeue to continue with the rest of the reconciliation
		return ctrl.Result{Requeue: true}, nil
	}

	// Create namespace if it doesn't exist
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: project.Name,
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
		log.Info("Namespace already exists", "namespace", project.Name)
	} else {
		log.Info("Successfully created namespace",
			"namespace", project.Name,
			"projectId", project.Name)
	}

	// Create ServiceAccount for Helm jobs
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "helm-deployer",
			Namespace: project.Name,
			Labels: map[string]string{
				"shapeblock.io/project-id": project.Name,
				"shapeblock.io/managed-by": "shapeblock-operator",
			},
		},
	}
	if err := r.Create(ctx, sa); err != nil && !errors.IsAlreadyExists(err) {
		log.Error(err, "Failed to create ServiceAccount")
		return r.failProject(ctx, project, fmt.Sprintf("Failed to create ServiceAccount: %v", err))
	}

	// Create Role for Helm jobs
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "helm-deployer",
			Namespace: project.Name,
			Labels: map[string]string{
				"shapeblock.io/project-id": project.Name,
				"shapeblock.io/managed-by": "shapeblock-operator",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				// Core API resources that most charts need
				APIGroups: []string{""},
				Resources: []string{
					"services", "pods", "pods/exec", "pods/portforward", "pods/log",
					"configmaps", "secrets", "serviceaccounts",
					"persistentvolumeclaims", "persistentvolumes",
					"events", "endpoints", "nodes",
				},
				Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				// Apps API group for workload controllers
				APIGroups: []string{"apps"},
				Resources: []string{
					"deployments", "statefulsets", "replicasets",
					"daemonsets", "controllerrevisions",
				},
				Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				// Batch jobs and cronjobs
				APIGroups: []string{"batch"},
				Resources: []string{"jobs", "cronjobs"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				// Networking resources
				APIGroups: []string{"networking.k8s.io"},
				Resources: []string{
					"ingresses", "ingressclasses", "networkpolicies",
				},
				Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				// RBAC resources that charts might need to create
				APIGroups: []string{"rbac.authorization.k8s.io"},
				Resources: []string{"roles", "rolebindings"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				// Autoscaling for HPA
				APIGroups: []string{"autoscaling"},
				Resources: []string{"horizontalpodautoscalers"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				// Policy for PodDisruptionBudgets
				APIGroups: []string{"policy"},
				Resources: []string{"poddisruptionbudgets"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
		},
	}
	if err := r.Create(ctx, role); err != nil && !errors.IsAlreadyExists(err) {
		log.Error(err, "Failed to create Role")
		return r.failProject(ctx, project, fmt.Sprintf("Failed to create Role: %v", err))
	}

	// Create RoleBinding for Helm jobs
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "helm-deployer",
			Namespace: project.Name,
			Labels: map[string]string{
				"shapeblock.io/project-id": project.Name,
				"shapeblock.io/managed-by": "shapeblock-operator",
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      sa.Name,
				Namespace: sa.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     role.Name,
		},
	}
	if err := r.Create(ctx, rb); err != nil && !errors.IsAlreadyExists(err) {
		log.Error(err, "Failed to create RoleBinding")
		return r.failProject(ctx, project, fmt.Sprintf("Failed to create RoleBinding: %v", err))
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
		"namespace", project.Name)

	// Update status to Active if not already
	if project.Status.Phase != "Active" {
		if err := r.updateProjectStatus(ctx, project, "Active", "Project setup completed"); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *ProjectReconciler) handleDeletion(ctx context.Context, project *appsv1alpha1.Project) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(project, projectFinalizer) {
		// Check if namespace still exists
		ns := &corev1.Namespace{}
		err := r.Get(ctx, types.NamespacedName{Name: project.Name}, ns)
		if err == nil {
			// Namespace exists, delete it if not already being deleted
			if ns.DeletionTimestamp.IsZero() {
				log.Info("Deleting namespace", "namespace", project.Name)
				if err := r.Delete(ctx, ns); err != nil && !errors.IsNotFound(err) {
					log.Error(err, "Failed to delete namespace")
					return ctrl.Result{}, err
				}
			}
			// Namespace is still being deleted, requeue
			log.Info("Waiting for namespace deletion", "namespace", project.Name)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		} else if !errors.IsNotFound(err) {
			// Error other than NotFound
			log.Error(err, "Failed to get namespace")
			return ctrl.Result{}, err
		}

		// Namespace is gone, remove finalizer
		log.Info("Namespace deleted, removing finalizer", "namespace", project.Name)
		controllerutil.RemoveFinalizer(project, projectFinalizer)
		if err := r.Update(ctx, project); err != nil {
			log.Error(err, "Failed to remove finalizer")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// updateProjectStatus updates the status of the Project resource
func (r *ProjectReconciler) updateProjectStatus(ctx context.Context, project *appsv1alpha1.Project, phase, message string) error {
	// Get the latest version of the Project
	latest := &appsv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Name: project.Name, Namespace: project.Namespace}, latest); err != nil {
		return err
	}

	// Update status fields
	latest.Status.Phase = phase
	if message != "" {
		latest.Status.Message = message
	}
	latest.Status.LastUpdated = &metav1.Time{Time: time.Now()}

	// Update status and send websocket notification
	if err := r.Status().Update(ctx, latest); err != nil {
		return err
	}
	r.sendProjectStatus(latest)
	return nil
}

func (r *ProjectReconciler) copyRegistrySecret(ctx context.Context, project *appsv1alpha1.Project, sourceSecretName string) error {
	log := log.FromContext(ctx)

	// Check if secret already exists in target namespace
	existingSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "registry-creds",
		Namespace: project.Name,
	}, existingSecret)

	if err == nil {
		log.Info("Registry secret already exists in namespace, skipping copy",
			"namespace", project.Name,
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
			Namespace: project.Name,
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
	if err := r.updateProjectStatus(ctx, project, "Failed", message); err != nil {
		return ctrl.Result{}, err
	}
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
