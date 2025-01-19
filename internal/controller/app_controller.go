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
		// Requeue to continue with the rest of the reconciliation
		return ctrl.Result{Requeue: true}, nil
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

	// Create or update registry secret
	if app.Spec.Registry.SecretName != "" {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      app.Spec.Registry.SecretName,
				Namespace: app.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":       app.Name,
					"app.kubernetes.io/managed-by": "shapeblock-operator",
				},
			},
			Type: corev1.SecretTypeDockerConfigJson,
			// ... secret data setup ...
		}

		if err := r.Create(ctx, secret); err != nil {
			if !errors.IsAlreadyExists(err) {
				log.Error(err, "Failed to create registry secret")
				return r.failApp(ctx, app, fmt.Sprintf("Failed to create registry secret: %v", err))
			}
			log.Info("Registry secret already exists",
				"secretName", app.Spec.Registry.SecretName)
		} else {
			log.Info("Successfully created registry secret",
				"secretName", app.Spec.Registry.SecretName,
				"app", app.Name)
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
	statusUpdate := utils.NewStatusUpdate("App", latest.Name, latest.Namespace)
	statusUpdate.Status = latest.Status.Phase
	statusUpdate.Message = latest.Status.Message
	r.WebsocketClient.SendStatus(statusUpdate)
	return nil
}

func (r *AppReconciler) failApp(ctx context.Context, app *appsv1alpha1.App, message string) (ctrl.Result, error) {
	if err := r.updateAppStatus(ctx, app, "Failed", message); err != nil {
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

	// Update App with secret reference
	app.Spec.Git.SecretName = secret.Name
	if err := r.Update(ctx, app); err != nil {
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

	// 2. Delete the HelmChart CR
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

	// 3. Delete the cache PVC
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
