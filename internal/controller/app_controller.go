package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	helmv1 "github.com/k3s-io/helm-controller/pkg/apis/helm.cattle.io/v1"
	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	"github.com/shapeblock/operator/pkg/utils"
)

// AppReconciler reconciles a App object
type AppReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	WebsocketClient *utils.WebsocketClient
	GitClient       *utils.GitClient // Client to interact with ShapeBlock API for SSH keys
}

func (r *AppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	app := &appsv1alpha1.App{}
	if err := r.Get(ctx, req.NamespacedName, app); err != nil {
		if errors.IsNotFound(err) {
			// App was deleted, clean up resources
			return r.handleDeletion(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Initialize app status if not set
	if app.Status.Phase == "" {
		if err := r.updateAppStatus(ctx, app, "Initializing", ""); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Initializing new app",
			"app", app.Name,
			"namespace", app.Namespace,
			"gitUrl", app.Spec.Git.URL)
		// Requeue to continue with the reconciliation
		return ctrl.Result{Requeue: true}, nil
	}

	// Validate based on build type
	if app.Spec.Build.Type == "image" {
		// For image type, image and registry.secretName are required
		if app.Spec.Build.Image == "" {
			return r.failApp(ctx, app, "build.image is required for image type")
		}
		if app.Spec.Registry.SecretName == "" {
			return r.failApp(ctx, app, "registry.secretName is required for image type")
		}
	} else {
		// For other types, git.url and registry.url are required
		if app.Spec.Git.URL == "" {
			return r.failApp(ctx, app, "git.url is required for non-image type")
		}
		if app.Spec.Registry.URL == "" {
			return r.failApp(ctx, app, "registry.url is required for non-image type")
		}
	}

	// Handle private repository setup if needed
	if app.Spec.Git.IsPrivate {
		log.Info("Setting up private repository",
			"app", app.Name,
			"namespace", app.Namespace)
		if err := r.setupPrivateRepo(ctx, app); err != nil {
			return r.failApp(ctx, app, fmt.Sprintf("Failed to setup private repository: %v", err))
		}
	}

	// Check if registry secret exists
	if app.Spec.Registry.SecretName != "" {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      app.Spec.Registry.SecretName,
			Namespace: app.Namespace,
		}, secret); err != nil {
			if errors.IsNotFound(err) {
				return r.failApp(ctx, app, fmt.Sprintf("Registry secret %s not found in namespace %s", app.Spec.Registry.SecretName, app.Namespace))
			}
			return r.failApp(ctx, app, fmt.Sprintf("Failed to get registry secret: %v", err))
		}
		// Verify secret is of correct type
		if secret.Type != corev1.SecretTypeDockerConfigJson {
			return r.failApp(ctx, app, fmt.Sprintf("Registry secret %s must be of type %s", app.Spec.Registry.SecretName, corev1.SecretTypeDockerConfigJson))
		}
	}

	// Update status to Ready if not already
	if app.Status.Phase != "Ready" {
		if err := r.updateAppStatus(ctx, app, "Ready", ""); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("App is now ready",
			"app", app.Name,
			"namespace", app.Namespace,
			"buildType", app.Spec.Build.Type)
	}

	return ctrl.Result{}, nil
}

// updateAppStatus updates the status of the App resource
func (r *AppReconciler) updateAppStatus(ctx context.Context, app *appsv1alpha1.App, phase, message string) error {
	// Get the latest version of the App
	latest := &appsv1alpha1.App{}
	if err := r.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, latest); err != nil {
		return err
	}

	// If status is already set to desired values, return
	if latest.Status.Phase == phase && latest.Status.Message == message {
		return nil
	}

	// Create a copy of the latest object
	patch := latest.DeepCopy()

	// Update status fields
	patch.Status.Phase = phase
	if message != "" {
		patch.Status.Message = message
	}
	patch.Status.LastUpdated = &metav1.Time{Time: time.Now()}

	// Use strategic merge patch to update status
	if err := r.Status().Patch(ctx, patch, client.MergeFrom(latest)); err != nil {
		if errors.IsConflict(err) {
			// If there's a conflict, requeue the reconciliation
			return fmt.Errorf("conflict updating App status, will retry: %v", err)
		}
		return err
	}

	statusUpdate := utils.NewStatusUpdate("App", latest.Name, latest.Namespace)
	statusUpdate.Status = patch.Status.Phase
	statusUpdate.Message = patch.Status.Message
	r.WebsocketClient.SendStatus(statusUpdate)
	return nil
}

func (r *AppReconciler) failApp(ctx context.Context, app *appsv1alpha1.App, message string) (ctrl.Result, error) {
	if err := r.updateAppStatus(ctx, app, "Failed", message); err != nil {
		if errors.IsConflict(err) {
			// If there's a conflict, requeue after a short delay
			return ctrl.Result{Requeue: true, RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, fmt.Errorf(message)
}

func (r *AppReconciler) setupPrivateRepo(ctx context.Context, app *appsv1alpha1.App) error {
	log := log.FromContext(ctx)

	// Get SSH key from ShapeBlock API
	sshKey, err := r.GitClient.GetSSHKey(app.Name)
	if err != nil {
		return fmt.Errorf("failed to get SSH key: %v", err)
	}
	log.Info("Retrieved SSH key from API", "app", app.Name)

	// Create SSH secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-git-auth", app.Name),
			Namespace: app.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       app.Name,
				"app.kubernetes.io/managed-by": "shapeblock-operator",
			},
		},
		Type: corev1.SecretTypeSSHAuth,
		Data: map[string][]byte{
			"ssh-privatekey": []byte(sshKey),
		},
	}

	if err := r.Create(ctx, secret); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create SSH secret: %v", err)
		}
		log.Info("SSH secret already exists",
			"secretName", secret.Name)
	} else {
		log.Info("Successfully created SSH secret",
			"secretName", secret.Name,
			"app", app.Name)
	}

	// Get latest version of App before updating
	latest := &appsv1alpha1.App{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: app.Namespace,
		Name:      app.Name,
	}, latest); err != nil {
		return fmt.Errorf("failed to get latest App version: %v", err)
	}

	// Create patch with the secret reference
	patch := latest.DeepCopy()
	patch.Spec.Git.SecretName = secret.Name

	// Apply patch using strategic merge
	if err := r.Patch(ctx, patch, client.MergeFrom(latest)); err != nil {
		return fmt.Errorf("failed to update App with secret reference: %v", err)
	}

	log.Info("Updated app with SSH secret reference",
		"app", app.Name,
		"secretName", secret.Name)

	return nil
}

func (r *AppReconciler) handleDeletion(ctx context.Context, namespacedName types.NamespacedName) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// 1. Delete all jobs with the label app.kubernetes.io/name: <app-name>
	jobList := &batchv1.JobList{}
	labelSelector := labels.SelectorFromSet(map[string]string{
		"app.kubernetes.io/name": namespacedName.Name,
	})
	if err := r.List(ctx, jobList, &client.ListOptions{
		Namespace:     namespacedName.Namespace,
		LabelSelector: labelSelector,
	}); err != nil {
		log.Error(err, "Failed to list jobs")
		return ctrl.Result{}, err
	}

	for _, job := range jobList.Items {
		if err := r.Delete(ctx, &job); err != nil {
			log.Error(err, "Failed to delete job", "job", job.Name)
			return ctrl.Result{}, err
		}
		log.Info("Deleted job", "job", job.Name)
	}

	// 2. Delete the SSH secret if it exists
	sshSecret := &corev1.Secret{}
	sshSecretName := fmt.Sprintf("%s-git-auth", namespacedName.Name)
	if err := r.Get(ctx, types.NamespacedName{
		Name:      sshSecretName,
		Namespace: namespacedName.Namespace,
	}, sshSecret); err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "Failed to get SSH secret")
			return ctrl.Result{}, err
		}
	} else {
		if err := r.Delete(ctx, sshSecret); err != nil {
			log.Error(err, "Failed to delete SSH secret")
			return ctrl.Result{}, err
		}
		log.Info("Deleted SSH secret", "name", sshSecret.Name)
	}

	// 3. Delete the HelmChart CR
	helmChart := &helmv1.HelmChart{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      namespacedName.Name,
		Namespace: namespacedName.Namespace,
	}, helmChart); err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "Failed to get HelmChart")
			return ctrl.Result{}, err
		}
	} else {
		if err := r.Delete(ctx, helmChart); err != nil {
			log.Error(err, "Failed to delete HelmChart")
			return ctrl.Result{}, err
		}
		log.Info("Deleted HelmChart", "name", helmChart.Name)
	}

	// 4. Delete the cache PVC
	pvc := &corev1.PersistentVolumeClaim{}
	pvcName := fmt.Sprintf("cache-%s", namespacedName.Name)
	if err := r.Get(ctx, types.NamespacedName{
		Name:      pvcName,
		Namespace: namespacedName.Namespace,
	}, pvc); err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "Failed to get cache PVC")
			return ctrl.Result{}, err
		}
	} else {
		if err := r.Delete(ctx, pvc); err != nil {
			log.Error(err, "Failed to delete cache PVC")
			return ctrl.Result{}, err
		}
		log.Info("Deleted cache PVC", "name", pvc.Name)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.App{}).
		Complete(r)
}
