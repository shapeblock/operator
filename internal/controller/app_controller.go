package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

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
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Initialize app status if not set
	if app.Status.Phase == "" {
		app.Status.Phase = "Initializing"
		app.Status.LastUpdated = &metav1.Time{Time: time.Now()}
		if err := r.Status().Update(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("Initializing new app",
			"app", app.Name,
			"namespace", app.Namespace,
			"gitUrl", app.Spec.Git.URL)
	}

	// Handle private repository setup
	if app.Spec.Git.IsPrivate {
		log.Info("Setting up private repository",
			"app", app.Name,
			"namespace", app.Namespace)

		if err := r.setupPrivateRepo(ctx, app); err != nil {
			log.Error(err, "Failed to setup private repository")
			return r.failApp(ctx, app, fmt.Sprintf("Failed to setup private repository: %v", err))
		}
		log.Info("Successfully setup private repository",
			"app", app.Name,
			"secretName", app.Spec.Git.SecretName)
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

	// Update status to Ready
	if app.Status.Phase != "Ready" {
		app.Status.Phase = "Ready"
		app.Status.Message = "Application is ready for builds"
		app.Status.LastUpdated = &metav1.Time{Time: time.Now()}
		if err := r.Status().Update(ctx, app); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("App is now ready",
			"app", app.Name,
			"namespace", app.Namespace,
			"buildType", app.Spec.Build.Type)
	}

	return ctrl.Result{}, nil
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

func (r *AppReconciler) failApp(ctx context.Context, app *appsv1alpha1.App, message string) (ctrl.Result, error) {
	app.Status.Phase = "Failed"
	app.Status.Message = message
	app.Status.LastUpdated = &metav1.Time{Time: time.Now()}
	if err := r.Status().Update(ctx, app); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.App{}).
		Complete(r)
}
