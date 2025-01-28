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

	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	"github.com/shapeblock/operator/pkg/utils"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/pointer"
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

	// 1. Delete all AppBuilds associated with this app
	buildList := &appsv1alpha1.AppBuildList{}
	if err := r.List(ctx, buildList, client.InNamespace(namespacedName.Namespace)); err != nil {
		log.Error(err, "Failed to list AppBuilds")
		return ctrl.Result{}, err
	}

	for _, build := range buildList.Items {
		if build.Spec.AppName == namespacedName.Name {
			// Delete each AppBuild
			if err := r.Delete(ctx, &build); err != nil {
				if !errors.IsNotFound(err) {
					log.Error(err, "Failed to delete AppBuild", "build", build.Name)
					return ctrl.Result{}, err
				}
			}
			log.Info("Deleted AppBuild", "build", build.Name)
		}
	}

	// 2. Delete all jobs with the label app.kubernetes.io/name: <app-name>
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

	background := metav1.DeletePropagationBackground
	for _, job := range jobList.Items {
		if err := r.Delete(ctx, &job, &client.DeleteOptions{
			PropagationPolicy: &background,
		}); err != nil {
			if !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete job", "job", job.Name)
				return ctrl.Result{}, err
			}
		}
		log.Info("Deleted job", "job", job.Name)
	}

	// 3. Delete the SSH secret if it exists
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
			if !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete SSH secret")
				return ctrl.Result{}, err
			}
		}
		log.Info("Deleted SSH secret", "name", sshSecret.Name)
	}

	// 4. Create a Helm uninstall job
	uninstallJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-helm-uninstall", namespacedName.Name),
			Namespace: namespacedName.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "shapeblock-operator",
				"app.kubernetes.io/name":       namespacedName.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: pointer.Int32(0),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "shapeblock-operator",
						"app.kubernetes.io/name":       namespacedName.Name,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "helm-deployer",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "helm",
							Image:   "alpine/helm:3.17",
							Command: []string{"/bin/sh", "-c"},
							Args: []string{
								fmt.Sprintf(`
set -e
# Check if release exists
if helm status %s -n %s >/dev/null 2>&1; then
  echo "Uninstalling release"
  helm uninstall %s \
    --namespace %s \
    --wait \
    --timeout 5m
fi
`, namespacedName.Name, namespacedName.Namespace, namespacedName.Name, namespacedName.Namespace),
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("128Mi"),
									corev1.ResourceCPU:    resource.MustParse("100m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("256Mi"),
									corev1.ResourceCPU:    resource.MustParse("200m"),
								},
							},
						},
					},
				},
			},
		},
	}

	// Create the uninstall job
	if err := r.Create(ctx, uninstallJob); err != nil {
		if !errors.IsAlreadyExists(err) {
			log.Error(err, "Failed to create uninstall job")
			return ctrl.Result{}, err
		}
	}

	// Wait for job completion
	for i := 0; i < 60; i++ { // Wait up to 5 minutes
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      uninstallJob.Name,
			Namespace: uninstallJob.Namespace,
		}, job)

		if err != nil {
			if !errors.IsNotFound(err) {
				log.Error(err, "Failed to get uninstall job")
				return ctrl.Result{}, err
			}
		} else {
			if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
				// Job completed (success or failure), delete it
				if err := r.Delete(ctx, job, &client.DeleteOptions{
					PropagationPolicy: &background,
				}); err != nil && !errors.IsNotFound(err) {
					log.Error(err, "Failed to delete uninstall job")
				}
				break
			}
		}
		time.Sleep(5 * time.Second)
	}

	// 5. Delete all PVCs associated with this app
	// First, delete the build cache PVC
	buildCachePVC := &corev1.PersistentVolumeClaim{}
	buildCachePVCName := fmt.Sprintf("cache-%s", namespacedName.Name)
	if err := r.Get(ctx, types.NamespacedName{
		Name:      buildCachePVCName,
		Namespace: namespacedName.Namespace,
	}, buildCachePVC); err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "Failed to get build cache PVC")
			return ctrl.Result{}, err
		}
	} else {
		// Force delete the PVC by removing finalizers
		if len(buildCachePVC.Finalizers) > 0 {
			buildCachePVC.Finalizers = nil
			if err := r.Update(ctx, buildCachePVC); err != nil {
				log.Error(err, "Failed to remove finalizers from build cache PVC")
				return ctrl.Result{}, err
			}
		}
		if err := r.Delete(ctx, buildCachePVC); err != nil {
			if !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete build cache PVC")
				return ctrl.Result{}, err
			}
		}
		log.Info("Deleted build cache PVC", "name", buildCachePVC.Name)
	}

	// Then, delete the Kaniko cache PVC if it exists
	kanikoCachePVC := &corev1.PersistentVolumeClaim{}
	kanikoCachePVCName := fmt.Sprintf("kaniko-cache-%s", namespacedName.Name)
	if err := r.Get(ctx, types.NamespacedName{
		Name:      kanikoCachePVCName,
		Namespace: namespacedName.Namespace,
	}, kanikoCachePVC); err != nil {
		if !errors.IsNotFound(err) {
			log.Error(err, "Failed to get Kaniko cache PVC")
			return ctrl.Result{}, err
		}
	} else {
		// Force delete the PVC by removing finalizers
		if len(kanikoCachePVC.Finalizers) > 0 {
			kanikoCachePVC.Finalizers = nil
			if err := r.Update(ctx, kanikoCachePVC); err != nil {
				log.Error(err, "Failed to remove finalizers from Kaniko cache PVC")
				return ctrl.Result{}, err
			}
		}
		if err := r.Delete(ctx, kanikoCachePVC); err != nil {
			if !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete Kaniko cache PVC")
				return ctrl.Result{}, err
			}
		}
		log.Info("Deleted Kaniko cache PVC", "name", kanikoCachePVC.Name)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.App{}).
		Complete(r)
}
