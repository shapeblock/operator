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

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	"github.com/shapeblock/operator/pkg/utils"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//+kubebuilder:rbac:groups=apps.shapeblock.io,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps.shapeblock.io,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

const serviceFinalizer = "apps.shapeblock.io/service-cleanup"

type ServiceReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	WebsocketClient *utils.WebsocketClient
	CoreV1Client    kubernetes.Interface
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
			"service", service.Name)
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

	// Create or update Helm release
	if err := r.createOrUpdateHelmRelease(ctx, service); err != nil {
		log.Error(err, "Failed to create/update Helm release",
			"service", service.Name,
			"chart", service.Spec.Chart.Name)
		service.Status.Phase = "Failed"
		service.Status.Message = fmt.Sprintf("Failed to create/update Helm release: %v", err)
		if err := r.Status().Update(ctx, service); err != nil {
			return ctrl.Result{}, err
		}
		r.sendServiceStatus(service)
		return ctrl.Result{}, err
	}

	return r.monitorHelmRelease(ctx, service)
}

func (r *ServiceReconciler) createOrUpdateHelmRelease(ctx context.Context, service *appsv1alpha1.Service) error {
	log := log.FromContext(ctx)

	// Only proceed if service is in Pending state
	if service.Status.Phase != "Pending" {
		log.Info("Service is not in Pending state, skipping helm job creation",
			"service", service.Name,
			"phase", service.Status.Phase)
		return nil
	}

	// Create ConfigMap to store Helm values
	valuesCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-helm-values", service.Name),
			Namespace: service.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "shapeblock-operator",
				"service.shapeblock.io/id":     service.Name,
			},
		},
		Data: map[string]string{
			"values.yaml": string(service.Spec.HelmValues.Raw),
		},
	}

	// Create or update ConfigMap
	if err := r.Create(ctx, valuesCM); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create values configmap: %v", err)
		}
		// ConfigMap exists, update it
		existing := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(valuesCM), existing); err != nil {
			return fmt.Errorf("failed to get existing configmap: %v", err)
		}
		existing.Data = valuesCM.Data
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update values configmap: %v", err)
		}
	}

	// Create Helm install/upgrade job
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-helm", service.Name),
			Namespace: service.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "shapeblock-operator",
				"service.shapeblock.io/id":     service.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: pointer.Int32(0),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "shapeblock-operator",
						"service.shapeblock.io/id":     service.Name,
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
# Add helm repo
helm repo add %s %s
helm repo update

# Check if release exists
if helm status %s -n %s >/dev/null 2>&1; then
  echo "Upgrading existing release"
  helm upgrade %s %s/%s \
    --namespace %s \
    --version %s \
    --values /values/values.yaml \
    --wait \
    --timeout 10m
else
  echo "Installing new release"
  helm install %s %s/%s \
    --namespace %s \
    --version %s \
    --values /values/values.yaml \
    --wait \
    --timeout 10m
fi
`, service.Name, service.Spec.Chart.RepoURL,
									service.Name, service.Namespace,
									service.Name, service.Name, service.Spec.Chart.Name,
									service.Namespace, service.Spec.Chart.Version,
									service.Name, service.Name, service.Spec.Chart.Name,
									service.Namespace, service.Spec.Chart.Version),
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("256Mi"),
									corev1.ResourceCPU:    resource.MustParse("100m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("512Mi"),
									corev1.ResourceCPU:    resource.MustParse("200m"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "values",
									MountPath: "/values",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "values",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: valuesCM.Name,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Create job if it doesn't exist
	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create helm job: %v", err)
		}
		log.Info("Helm job already exists", "job", job.Name)
	}

	return nil
}

func (r *ServiceReconciler) monitorHelmRelease(ctx context.Context, service *appsv1alpha1.Service) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Get the helm job
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{
		Name:      fmt.Sprintf("%s-helm", service.Name),
		Namespace: service.Namespace,
	}, job)

	if err != nil {
		if errors.IsNotFound(err) {
			// If the job is not found and service is still in Pending state,
			// we should retry creating it
			if service.Status.Phase == "Pending" {
				log.Info("Helm job not found, recreating",
					"service", service.Name)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			// Otherwise, nothing to monitor
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Check if job failed or any pods are in failed state
	if job.Status.Failed > 0 || isPodFailed(ctx, r, job) {
		log.Info("Helm job failed",
			"service", service.Name)

		// Get the pod for logs
		pods := &corev1.PodList{}
		if err := r.List(ctx, pods,
			client.InNamespace(service.Namespace),
			client.MatchingLabels{
				"job-name": job.Name,
			}); err != nil {
			log.Error(err, "Failed to get helm job pods")
			return ctrl.Result{}, err
		}

		failureMsg := "Helm release failed"
		if len(pods.Items) > 0 {
			// Get the logs from the helm job pod
			req := r.CoreV1Client.CoreV1().Pods(service.Namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{})
			logs, err := req.DoRaw(ctx)
			if err != nil {
				log.Error(err, "Failed to get helm job logs")
				// Check if pod was OOMKilled
				if pods.Items[0].Status.ContainerStatuses != nil &&
					len(pods.Items[0].Status.ContainerStatuses) > 0 &&
					pods.Items[0].Status.ContainerStatuses[0].State.Terminated != nil &&
					pods.Items[0].Status.ContainerStatuses[0].State.Terminated.Reason == "OOMKilled" {
					failureMsg = "Helm release failed: Pod was terminated due to out of memory"
				}
			} else {
				failureMsg = fmt.Sprintf("Helm release failed:\n%s", string(logs))
			}
		}

		// Update service status if it's not already failed
		if service.Status.Phase != "Failed" {
			service.Status.Phase = "Failed"
			service.Status.Message = failureMsg
			if err := r.Status().Update(ctx, service); err != nil {
				return ctrl.Result{}, err
			}
			r.sendServiceStatus(service)
		}

		// Delete the job and ConfigMap
		background := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{
			PropagationPolicy: &background,
		}); err != nil && !errors.IsNotFound(err) {
			log.Error(err, "Failed to delete failed job")
		}

		// Delete the ConfigMap
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      fmt.Sprintf("%s-helm-values", service.Name),
			Namespace: service.Namespace,
		}, cm); err == nil {
			if err := r.Delete(ctx, cm); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete values ConfigMap")
			}
		}

		return ctrl.Result{}, nil
	}

	// Check if job completed successfully
	if job.Status.Succeeded > 0 {
		// Check if all pods are ready
		log.Info("Helm job completed successfully",
			"service", service.Name)
		podList := &corev1.PodList{}
		if err := r.List(ctx, podList,
			client.InNamespace(service.Namespace),
			client.MatchingLabels{
				"app.kubernetes.io/instance": service.Name,
			}); err != nil {
			log.Error(err, "Failed to list service pods")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Check pod readiness
		var notReadyPods []string
		for _, pod := range podList.Items {
			if !utils.IsPodReady(&pod) {
				notReadyPods = append(notReadyPods, pod.Name)
			}
		}

		if len(notReadyPods) > 0 {
			log.Info("Waiting for pods to be ready",
				"service", service.Name,
				"notReadyPods", notReadyPods)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// All pods are ready, update status
		service.Status.Phase = "Deployed"
		service.Status.Message = "Service deployed successfully"
		service.Status.HelmRelease = service.Name
		if err := r.Status().Update(ctx, service); err != nil {
			return ctrl.Result{}, err
		}
		r.sendServiceStatus(service)

		// Delete the job and ConfigMap
		background := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{
			PropagationPolicy: &background,
		}); err != nil && !errors.IsNotFound(err) {
			log.Error(err, "Failed to delete completed job")
		}

		// Delete the ConfigMap
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      fmt.Sprintf("%s-helm-values", service.Name),
			Namespace: service.Namespace,
		}, cm); err == nil {
			if err := r.Delete(ctx, cm); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete values ConfigMap")
			}
		}

		return ctrl.Result{}, nil
	}

	// Still waiting for job to complete
	log.Info("Waiting for helm job to complete",
		"service", service.Name)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// Helper function to check if any pod of the job is in failed state
func isPodFailed(ctx context.Context, r *ServiceReconciler, job *batchv1.Job) bool {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{
			"job-name": job.Name,
		}); err != nil {
		return false
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodFailed {
			return true
		}
		// Check for OOMKilled
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Terminated != nil &&
				containerStatus.State.Terminated.Reason == "OOMKilled" {
				return true
			}
		}
	}
	return false
}

func (r *ServiceReconciler) handleDeletion(ctx context.Context, service *appsv1alpha1.Service) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(service, serviceFinalizer) {
		// Create uninstall job
		uninstallJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-helm-uninstall", service.Name),
				Namespace: service.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "shapeblock-operator",
					"service.shapeblock.io/id":     service.Name,
				},
			},
			Spec: batchv1.JobSpec{
				BackoffLimit: pointer.Int32(0),
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app.kubernetes.io/managed-by": "shapeblock-operator",
							"service.shapeblock.io/id":     service.Name,
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
`, service.Name, service.Namespace, service.Name, service.Namespace),
								},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceMemory: resource.MustParse("256Mi"),
										corev1.ResourceCPU:    resource.MustParse("100m"),
									},
									Limits: corev1.ResourceList{
										corev1.ResourceMemory: resource.MustParse("512Mi"),
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
			err := r.Get(ctx, client.ObjectKey{
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
					background := metav1.DeletePropagationBackground
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

		// List and delete PVCs with the app label
		pvcList := &corev1.PersistentVolumeClaimList{}
		if err := r.List(ctx, pvcList,
			client.InNamespace(service.Namespace),
			client.MatchingLabels{
				"app.kubernetes.io/instance": service.Name,
			}); err != nil {
			log.Error(err, "Failed to list PVCs")
			return ctrl.Result{}, err
		}

		for _, pvc := range pvcList.Items {
			// Force delete PVC by removing finalizers
			if len(pvc.Finalizers) > 0 {
				pvc.Finalizers = nil
				if err := r.Update(ctx, &pvc); err != nil {
					log.Error(err, "Failed to remove finalizers from PVC")
					return ctrl.Result{}, err
				}
			}
			if err := r.Delete(ctx, &pvc); err != nil {
				if !errors.IsNotFound(err) {
					log.Error(err, "Failed to delete PVC")
					return ctrl.Result{}, err
				}
			}
			log.Info("Deleted PVC",
				"pvc", pvc.Name,
				"namespace", pvc.Namespace)
		}

		// Send final status update
		service.Status.Phase = "Deleted"
		service.Status.Message = "Service and associated resources deleted"
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
	// Initialize CoreV1Client
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("failed to create kubernetes clientset: %v", err)
	}
	r.CoreV1Client = clientset

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
		"service.shapeblock.io/id": service.Name,
	}

	r.WebsocketClient.SendStatus(update)
}
