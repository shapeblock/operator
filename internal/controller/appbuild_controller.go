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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	helmv1 "github.com/k3s-io/helm-controller/pkg/apis/helm.cattle.io/v1"
	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	"github.com/shapeblock/operator/pkg/utils"
	"gopkg.in/yaml.v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

var DEBUG = os.Getenv("DEBUG") == "true"

const buildFinalizer = "apps.shapeblock.io/build-cleanup"

// AppBuildReconciler reconciles a AppBuild object
type AppBuildReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	WebsocketClient *utils.WebsocketClient
	CoreV1Client    corev1client.CoreV1Interface
}

// +kubebuilder:rbac:groups=apps.shapeblock.io,resources=appbuilds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.shapeblock.io,resources=appbuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.shapeblock.io,resources=appbuilds/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=helm.cattle.io,resources=helmcharts,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the AppBuild object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *AppBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Get AppBuild
	build := &appsv1alpha1.AppBuild{}
	if err := r.Get(ctx, req.NamespacedName, build); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !build.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, build)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(build, buildFinalizer) {
		controllerutil.AddFinalizer(build, buildFinalizer)
		if err := r.Update(ctx, build); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Get referenced App
	app := &appsv1alpha1.App{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: build.Namespace,
		Name:      build.Spec.AppName,
	}, app); err != nil {
		log.Error(err, "Unable to fetch App",
			"appName", build.Spec.AppName,
			"buildName", build.Name)
		return r.failBuild(ctx, build, fmt.Sprintf("unable to fetch App %s: %v", build.Spec.AppName, err))
	}

	// Initialize build status if not set
	if build.Status.Phase == "" {
		// Create a copy of the build object
		buildCopy := build.DeepCopy()
		buildCopy.Status.Phase = "Pending"
		buildCopy.Status.StartTime = &metav1.Time{Time: time.Now()}

		// Use Patch instead of Update
		if err := r.Status().Patch(ctx, buildCopy, client.MergeFrom(build)); err != nil {
			if errors.IsConflict(err) {
				log.Info("Conflict updating build status, will retry",
					"buildName", build.Name)
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, err
		}

		log.Info("Initializing new build",
			"buildName", build.Name,
			"appName", app.Name,
			"buildType", app.Spec.Build.Type)
		r.sendBuildStatus(buildCopy)

		// Requeue to handle the rest of the reconciliation
		return ctrl.Result{Requeue: true}, nil
	}

	// Check for existing build pod/job
	switch app.Spec.Build.Type {
	case "dockerfile":
		log.Info("Processing Dockerfile build",
			"buildName", build.Name,
			"appName", app.Name)
		result, err := r.monitorDockerfileBuild(ctx, app, build)
		if err != nil {
			return result, err
		}
		if build.Status.Phase == "Completed" {
			log.Info("Build completed, creating Helm release",
				"buildName", build.Name,
				"appName", app.Name)

			if DEBUG {
				helmValues, _ := json.MarshalIndent(build.Spec.HelmValues, "", "  ")
				log.Info("Helm values", "values", string(helmValues))
			}

			if err := r.createOrUpdateHelmRelease(ctx, app, build); err != nil {
				log.Error(err, "Failed to create Helm release")
				return r.failBuild(ctx, build, fmt.Sprintf("failed to create helm release: %v", err))
			}
			return r.monitorHelmRelease(ctx, app, build)
		}
		return result, nil

	case "buildpack":
		if build.Status.Phase == "Pending" {
			log.Info("Starting buildpack build",
				"buildName", build.Name,
				"appName", app.Name,
				"builderImage", app.Spec.Build.BuilderImage)
		}
		result, err := r.monitorBuildpackBuild(ctx, app, build)
		if err != nil {
			return result, err
		}
		if build.Status.Phase == "Completed" {
			if err := r.createOrUpdateHelmRelease(ctx, app, build); err != nil {
				log.Error(err, "Failed to create Helm release")
				// Requeue to retry Helm release creation
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
			return r.monitorHelmRelease(ctx, app, build)
		}
		return result, nil

	case "image":
		log.Info("Processing pre-built image deployment",
			"buildName", build.Name,
			"appName", app.Name,
			"image", app.Spec.Build.Image)

		if build.Status.Phase == "" || build.Status.Phase == "Pending" {
			build.Status.Phase = "Deploying"
			build.Status.Message = "Deploying pre-built image"
			build.Status.StartTime = &metav1.Time{Time: time.Now()}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}
			r.sendBuildStatus(build)

			if DEBUG {
				helmValues, _ := json.MarshalIndent(build.Spec.HelmValues, "", "  ")
				log.Info("Helm values", "values", string(helmValues))
			}

			if err := r.createOrUpdateHelmRelease(ctx, app, build); err != nil {
				log.Error(err, "Failed to create Helm release")
				return r.failBuild(ctx, build, fmt.Sprintf("failed to create helm release: %v", err))
			}
		}
		return r.monitorHelmRelease(ctx, app, build)

	default:
		log.Error(nil, "Unsupported build type",
			"buildType", app.Spec.Build.Type,
			"buildName", build.Name)
		return r.failBuild(ctx, build, fmt.Sprintf("unsupported build type: %s", app.Spec.Build.Type))
	}
}

func (r *AppBuildReconciler) monitorDockerfileBuild(ctx context.Context, app *appsv1alpha1.App, build *appsv1alpha1.AppBuild) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Get the build job
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      build.Name,
		Namespace: build.Namespace,
	}, job)

	if err != nil {
		if errors.IsNotFound(err) {
			// Only create new job if we're in Pending phase
			if build.Status.Phase == "Pending" {
				if err := r.handleDockerfileBuild(ctx, app, build); err != nil {
					return r.failBuild(ctx, build, fmt.Sprintf("failed to create build job: %v", err))
				}
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Get the pod for the job
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(build.Namespace),
		client.MatchingLabels{
			"job-name": build.Name,
		}); err != nil {
		return ctrl.Result{}, err
	}

	if len(pods.Items) == 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	pod := &pods.Items[0]

	// Update status and stream logs when pod is running
	if pod.Status.Phase == corev1.PodRunning {
		if build.Status.Phase != "Building" {
			build.Status.Phase = "Building"
			build.Status.Message = "Build in progress"
			build.Status.PodName = pod.Name
			build.Status.BuildStartTime = &metav1.Time{Time: time.Now()}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}
			// Start streaming logs
			go r.streamLogsToWebsocket(ctx, build, pod)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Check if job completed
	if job.Status.Succeeded > 0 {
		// Only process completion once
		if build.Status.Phase != "Completed" && build.Status.Phase != "Deploying" {
			// Stream final logs synchronously to ensure we capture everything
			if err := r.streamFinalLogs(ctx, build, pod); err != nil {
				log.Error(err, "Failed to capture final logs")
			}

			// Read Git commit SHA from the file
			req := r.CoreV1Client.Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				Container: "git-clone",
				Follow:    false,
			})
			stream, err := req.Stream(ctx)
			if err == nil {
				defer stream.Close()
				if content, err := io.ReadAll(stream); err == nil {
					// Extract the commit SHA from the git rev-parse output
					lines := strings.Split(string(content), "\n")
					for _, line := range lines {
						if len(line) == 40 { // Git commit SHA is 40 characters
							build.Status.GitCommit = line
							break
						}
					}
				}
			}

			build.Status.Phase = "Deploying"
			build.Status.Message = "Build completed, deploying application"
			build.Status.ImageTag = build.Spec.ImageTag // Store the image tag in status
			build.Status.BuildEndTime = &metav1.Time{Time: time.Now()}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Build completed successfully, starting deployment",
				"buildName", build.Name,
				"appName", app.Name)

			// Delete the completed job
			background := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, job, &client.DeleteOptions{
				PropagationPolicy: &background,
			}); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete completed job")
			}

			if err := r.createOrUpdateHelmRelease(ctx, app, build); err != nil {
				log.Error(err, "Failed to create Helm release")
				return r.failBuild(ctx, build, fmt.Sprintf("failed to create helm release: %v", err))
			}
			return ctrl.Result{}, nil
		}
		// If we're already in Deploying/Completed phase, no need to requeue
		return ctrl.Result{}, nil
	}

	// Check if job failed
	if job.Status.Failed > 0 {
		if build.Status.Phase != "Failed" {
			// Stream final logs synchronously to capture failure details
			if err := r.streamFinalLogs(ctx, build, pod); err != nil {
				log.Error(err, "Failed to capture final logs")
			}

			build.Status.Phase = "Failed"
			build.Status.Message = "Build failed"
			build.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			build.Status.BuildEndTime = &metav1.Time{Time: time.Now()}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}

			// Delete the failed job
			background := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, job, &client.DeleteOptions{
				PropagationPolicy: &background,
			}); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete failed job")
			}

			return ctrl.Result{}, nil
		}
		// If we're already in Failed phase, no need to requeue
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *AppBuildReconciler) monitorBuildpackBuild(ctx context.Context, app *appsv1alpha1.App, build *appsv1alpha1.AppBuild) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Get the build job
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      build.Name,
		Namespace: build.Namespace,
	}, job)

	if err != nil {
		if errors.IsNotFound(err) {
			// Create new build job
			if err := r.handleBuildpackBuild(ctx, app, build); err != nil {
				return r.failBuild(ctx, build, fmt.Sprintf("failed to create build job: %v", err))
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Get the pod for the job
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(build.Namespace),
		client.MatchingLabels{
			"job-name": build.Name,
		}); err != nil {
		return ctrl.Result{}, err
	}

	if len(pods.Items) == 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	pod := &pods.Items[0]

	// Update status and stream logs when pod is running
	if pod.Status.Phase == corev1.PodRunning {
		if build.Status.Phase != "Building" {
			build.Status.Phase = "Building"
			build.Status.Message = "Build in progress"
			build.Status.PodName = pod.Name
			build.Status.BuildStartTime = &metav1.Time{Time: time.Now()}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}
			// Start streaming logs
			go r.streamLogsToWebsocket(ctx, build, pod)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Check if job completed
	if job.Status.Succeeded > 0 {
		// Get final logs if we haven't already
		if build.Status.Phase != "Completed" && build.Status.Phase != "Deploying" {
			// Stream final logs synchronously to ensure we capture everything
			if err := r.streamFinalLogs(ctx, build, pod); err != nil {
				log.Error(err, "Failed to capture final logs")
			}

			// Read Git commit SHA from the file
			req := r.CoreV1Client.Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				Container: "git-clone",
				Follow:    false,
			})
			stream, err := req.Stream(ctx)
			if err == nil {
				defer stream.Close()
				if content, err := io.ReadAll(stream); err == nil {
					// Extract the commit SHA from the git rev-parse output
					lines := strings.Split(string(content), "\n")
					for _, line := range lines {
						if len(line) == 40 { // Git commit SHA is 40 characters
							build.Status.GitCommit = line
							break
						}
					}
				}
			}

			build.Status.Phase = "Deploying"
			build.Status.Message = "Build completed, deploying application"
			build.Status.ImageTag = build.Spec.ImageTag // Store the image tag in status
			build.Status.BuildEndTime = &metav1.Time{Time: time.Now()}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Build completed successfully, starting deployment",
				"buildName", build.Name,
				"appName", app.Name)

			if err := r.createOrUpdateHelmRelease(ctx, app, build); err != nil {
				log.Error(err, "Failed to create Helm release")
				return r.failBuild(ctx, build, fmt.Sprintf("failed to create helm release: %v", err))
			}
			return ctrl.Result{}, nil
		}
	}

	// Check if job failed
	if job.Status.Failed > 0 {
		if build.Status.Phase != "Failed" {
			// Stream final logs synchronously to capture failure details
			if err := r.streamFinalLogs(ctx, build, pod); err != nil {
				log.Error(err, "Failed to capture final logs")
			}

			build.Status.Phase = "Failed"
			build.Status.Message = "Build failed"
			build.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			build.Status.BuildEndTime = &metav1.Time{Time: time.Now()}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *AppBuildReconciler) updateBuildStatus(ctx context.Context, build *appsv1alpha1.AppBuild, pod *corev1.Pod) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// When pod starts running, initialize log streaming
	if pod.Status.Phase == corev1.PodRunning && build.Status.Phase != "Building" {
		build.Status.Phase = "Building"
		build.Status.Message = "Build in progress"
		build.Status.PodName = pod.Name

		if err := r.Status().Update(ctx, build); err != nil {
			return ctrl.Result{}, err
		}

		// Start streaming logs to websocket channel identified by build ID
		go r.streamLogsToWebsocket(ctx, build, pod)
	}

	// Update phase based on pod status
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		if build.Status.Phase != "Completed" && build.Status.Phase != "Deploying" {
			build.Status.Phase = "Deploying"
			build.Status.Message = "Build completed, deploying application"
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Build completed successfully, starting deployment",
				"buildName", build.Name)

			app := &appsv1alpha1.App{}
			if err := r.Get(ctx, types.NamespacedName{
				Namespace: build.Namespace,
				Name:      build.Spec.AppName,
			}, app); err != nil {
				return ctrl.Result{}, err
			}

			if err := r.createOrUpdateHelmRelease(ctx, app, build); err != nil {
				log.Error(err, "Failed to create Helm release")
				return r.failBuild(ctx, build, fmt.Sprintf("failed to create helm release: %v", err))
			}
			return ctrl.Result{}, nil
		}

	case corev1.PodFailed:
		if build.Status.Phase != "Failed" {
			build.Status.Phase = "Failed"
			build.Status.Message = "Build failed"
			build.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *AppBuildReconciler) streamLogsToWebsocket(ctx context.Context, build *appsv1alpha1.AppBuild, pod *corev1.Pod) {
	// Skip if either client is not configured
	if r.WebsocketClient == nil || r.CoreV1Client == nil {
		return
	}

	req := r.CoreV1Client.Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Follow: true,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		build.Status.Message = fmt.Sprintf("Error getting log stream: %v", err)
		r.sendBuildStatus(build)
		return
	}
	defer stream.Close()

	reader := bufio.NewReader(stream)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				build.Status.Message = fmt.Sprintf("Error reading logs: %v", err)
				r.sendBuildStatus(build)
			}
			return
		}

		// Send log line to websocket channel identified by build ID
		r.WebsocketClient.SendBuildLog(build.Name, string(line))
	}
}

func (r *AppBuildReconciler) handleDockerfileBuild(ctx context.Context, app *appsv1alpha1.App, build *appsv1alpha1.AppBuild) error {
	log := ctrl.LoggerFrom(ctx)

	// Check if we already have a successful build for this commit
	if build.Spec.GitRef != "" {
		// List all successful builds for this app
		buildList := &appsv1alpha1.AppBuildList{}
		if err := r.List(ctx, buildList,
			client.InNamespace(build.Namespace),
			client.MatchingLabels{
				"app.kubernetes.io/name": app.Name,
			}); err != nil {
			return fmt.Errorf("failed to list builds: %v", err)
		}

		// Check if we have a successful build with the same commit
		for _, existingBuild := range buildList.Items {
			if existingBuild.Status.GitCommit == build.Spec.GitRef &&
				existingBuild.Status.Phase == "Completed" {
				log.Info("Found existing successful build for commit",
					"buildName", existingBuild.Name,
					"commit", build.Spec.GitRef)

				// Update current build status to reuse the existing image
				build.Status.Phase = "Deploying"
				build.Status.Message = fmt.Sprintf("Reusing image from build %s", existingBuild.Name)
				build.Status.ImageTag = existingBuild.Status.ImageTag
				build.Status.GitCommit = existingBuild.Status.GitCommit
				if err := r.Status().Update(ctx, build); err != nil {
					return fmt.Errorf("failed to update build status: %v", err)
				}

				// Create Helm release with the existing image
				if err := r.createOrUpdateHelmRelease(ctx, app, build); err != nil {
					return fmt.Errorf("failed to create helm release: %v", err)
				}

				return nil
			}
		}
	}

	log.Info("Creating Kaniko build job",
		"buildName", build.Name,
		"appName", app.Name,
		"gitURL", app.Spec.Git.URL,
		"registryURL", app.Spec.Registry.URL)

	// Ensure build cache exists
	if err := r.ensureKanikoBuildCache(ctx, app); err != nil {
		return fmt.Errorf("failed to create cache: %v", err)
	}

	// Use default registry secret if none specified
	registrySecret := "registry-creds"
	if app.Spec.Registry.SecretName != "" {
		registrySecret = app.Spec.Registry.SecretName
	}

	// Create base volumes list
	volumes := []corev1.Volume{
		{
			Name: "registry-creds",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: registrySecret,
					Items: []corev1.KeyToPath{
						{
							Key:  ".dockerconfigjson",
							Path: "config.json",
						},
					},
				},
			},
		},
		{
			Name: "source",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: fmt.Sprintf("kaniko-cache-%s", app.Name),
				},
			},
		},
		{
			Name: "git-commit",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	// Create base git-clone container
	gitCloneContainer := corev1.Container{
		Name:    "git-clone",
		Image:   "bitnami/git:latest",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			`set -e
			mkdir -p /root/.ssh
			cp /ssh-key/ssh-privatekey /root/.ssh/id_rsa
			chmod 400 /root/.ssh/id_rsa
			ssh-keyscan -H github.com > /root/.ssh/known_hosts
			chmod 400 /root/.ssh/known_hosts

			# Configure SSH to use the key
			cat > /root/.ssh/config << EOF
Host github.com
    StrictHostKeyChecking no
    IdentityFile /root/.ssh/id_rsa
EOF
			chmod 400 /root/.ssh/config

			# Clone repository
			git clone ` + app.Spec.Git.URL + ` /workspace/source
			cd /workspace/source
			git checkout ` + app.Spec.Git.Branch + `
			git rev-parse HEAD > /workspace/git-commit/sha`,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("64Mi"),
				corev1.ResourceCPU:    resource.MustParse("100m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("128Mi"),
				corev1.ResourceCPU:    resource.MustParse("200m"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "source",
				MountPath: "/workspace/source",
			},
			{
				Name:      "git-commit",
				MountPath: "/workspace/git-commit",
			},
		},
	}

	// Add SSH key volume and update git clone container for private repositories
	if app.Spec.Git.SecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "ssh-key",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  app.Spec.Git.SecretName,
					DefaultMode: pointer.Int32(0400),
				},
			},
		})

		// Use bitnami/git which has better SSH support
		gitCloneContainer.Image = "bitnami/git:latest"
		gitCloneContainer.Args = []string{
			`set -e
			mkdir -p /root/.ssh
			cp /ssh-key/ssh-privatekey /root/.ssh/id_rsa
			chmod 400 /root/.ssh/id_rsa
			ssh-keyscan -H github.com > /root/.ssh/known_hosts
			chmod 400 /root/.ssh/known_hosts

			# Configure SSH to use the key
			cat > /root/.ssh/config << EOF
Host github.com
    StrictHostKeyChecking no
    IdentityFile /root/.ssh/id_rsa
EOF
			chmod 400 /root/.ssh/config

			# Clone repository
			git clone ` + app.Spec.Git.URL + ` /workspace/source
			cd /workspace/source
			git checkout ` + app.Spec.Git.Branch + `
			git rev-parse HEAD > /workspace/git-commit/sha`,
		}
		gitCloneContainer.VolumeMounts = append(gitCloneContainer.VolumeMounts, corev1.VolumeMount{
			Name:      "ssh-key",
			MountPath: "/ssh-key",
		})
	}

	// Create Kaniko job
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      build.Name,
			Namespace: build.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kaniko",
				"app.kubernetes.io/instance":   build.Name,
				"app.kubernetes.io/managed-by": "shapeblock-operator",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(build, appsv1alpha1.GroupVersion.WithKind("AppBuild")),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: pointer.Int32(0),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":       "kaniko",
						"app.kubernetes.io/instance":   build.Name,
						"app.kubernetes.io/managed-by": "shapeblock-operator",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					InitContainers: []corev1.Container{
						gitCloneContainer,
					},
					Containers: []corev1.Container{
						{
							Name:  "kaniko",
							Image: "gcr.io/kaniko-project/executor:latest",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("512Mi"),
									corev1.ResourceCPU:    resource.MustParse("250m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("1Gi"),
									corev1.ResourceCPU:    resource.MustParse("500m"),
								},
							},
							Args: []string{
								"--dockerfile=Dockerfile",
								"--context=dir:///workspace/source",
								"--cache=true",
								"--cache-dir=/cache",
								"--cache-repo=" + fmt.Sprintf("%s/%s/%s-cache", app.Spec.Registry.URL, app.Namespace, app.Name),
								fmt.Sprintf("--destination=%s/%s/%s:%s", app.Spec.Registry.URL, app.Namespace, app.Name, build.Spec.ImageTag),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "registry-creds",
									MountPath: "/kaniko/.docker",
								},
								{
									Name:      "source",
									MountPath: "/workspace/source",
								},
								{
									Name:      "cache",
									MountPath: "/cache",
								},
								{
									Name:      "git-commit",
									MountPath: "/workspace/git-commit",
								},
							},
						},
					},
					Volumes: volumes,
					Affinity: func() *corev1.Affinity {
						if build.Spec.BuildNodeAffinity == nil {
							return nil
						}
						return &corev1.Affinity{
							NodeAffinity: build.Spec.BuildNodeAffinity,
						}
					}(),
				},
			},
		},
	}

	// Try to get existing job
	existingJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      build.Name,
		Namespace: build.Namespace,
	}, existingJob); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing job: %v", err)
		}
		// Job doesn't exist, create it
		if err := r.Create(ctx, job); err != nil {
			log.Error(err, "Failed to create Kaniko job",
				"buildName", build.Name,
				"appName", app.Name)
			return fmt.Errorf("failed to create Kaniko job: %v", err)
		}
	} else if isJobFinished(existingJob) {
		// Job exists and is finished, check if it failed
		for _, condition := range existingJob.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				// Only recreate if the job failed
				if err := r.Delete(ctx, existingJob); err != nil {
					return fmt.Errorf("failed to delete failed job: %v", err)
				}
				if err := r.Create(ctx, job); err != nil {
					log.Error(err, "Failed to create new Kaniko job",
						"buildName", build.Name,
						"appName", app.Name)
					return fmt.Errorf("failed to create new Kaniko job: %v", err)
				}
				break
			}
		}
	}

	log.Info("Successfully handled Kaniko job",
		"jobName", job.Name,
		"buildName", build.Name,
		"appName", app.Name)

	return nil
}

// Helper function to create PVC for Kaniko cache if it doesn't exist
func (r *AppBuildReconciler) ensureKanikoBuildCache(ctx context.Context, app *appsv1alpha1.App) error {
	cachePVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("kaniko-cache-%s", app.Name),
			Namespace: app.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      app.Name,
				"app.kubernetes.io/component": "kaniko-cache",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("5Gi"),
				},
			},
		},
	}

	err := r.Create(ctx, cachePVC)
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create kaniko cache PVC: %v", err)
	}

	return nil
}

// Helper function to check if a job is finished
func isJobFinished(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *AppBuildReconciler) failBuild(ctx context.Context, build *appsv1alpha1.AppBuild, message string) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Create a copy of the build object
	buildCopy := build.DeepCopy()
	buildCopy.Status.Phase = "Failed"
	buildCopy.Status.Message = message
	buildCopy.Status.CompletionTime = &metav1.Time{Time: time.Now()}

	// Use Patch instead of Update
	if err := r.Status().Patch(ctx, buildCopy, client.MergeFrom(build)); err != nil {
		if errors.IsConflict(err) {
			log.Info("Conflict updating build status, will retry",
				"buildName", build.Name)
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *AppBuildReconciler) handleBuildpackBuild(ctx context.Context, app *appsv1alpha1.App, build *appsv1alpha1.AppBuild) error {
	log := ctrl.LoggerFrom(ctx)

	// Ensure build cache exists
	if err := r.ensureBuildCache(ctx, app); err != nil {
		return fmt.Errorf("failed to create cache: %v", err)
	}

	// Create base volumes list
	volumes := []corev1.Volume{
		{
			Name: "source",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: fmt.Sprintf("cache-%s", app.Name),
				},
			},
		},
		{
			Name: "layers",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: resource.NewQuantity(2*1024*1024*1024, resource.BinarySI), // 2Gi
				},
			},
		},
		{
			Name: "platform",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "registry-creds",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: func() string {
						if app.Spec.Registry.SecretName != "" {
							return app.Spec.Registry.SecretName
						}
						return "registry-creds"
					}(),
					Items: []corev1.KeyToPath{
						{
							Key:  ".dockerconfigjson",
							Path: "config.json",
						},
					},
				},
			},
		},
		{
			Name: "git-commit",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	// Create base git-clone container
	gitCloneContainer := corev1.Container{
		Name:    "git-clone",
		Image:   "bitnami/git:latest",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			`set -e
			mkdir -p /root/.ssh
			cp /ssh-key/ssh-privatekey /root/.ssh/id_rsa
			chmod 400 /root/.ssh/id_rsa
			ssh-keyscan -H github.com > /root/.ssh/known_hosts
			chmod 400 /root/.ssh/known_hosts

			# Configure SSH to use the key
			cat > /root/.ssh/config << EOF
Host github.com
    StrictHostKeyChecking no
    IdentityFile /root/.ssh/id_rsa
EOF
			chmod 400 /root/.ssh/config

			# Clone repository
			git clone ` + app.Spec.Git.URL + ` /workspace/source
			cd /workspace/source
			git checkout ` + build.Spec.GitRef + `
			git rev-parse HEAD > /workspace/git-commit/sha`,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("64Mi"),
				corev1.ResourceCPU:    resource.MustParse("100m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("128Mi"),
				corev1.ResourceCPU:    resource.MustParse("200m"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "source",
				MountPath: "/workspace/source",
			},
			{
				Name:      "git-commit",
				MountPath: "/workspace/git-commit",
			},
		},
	}

	// Add SSH key volume and update git clone container for private repositories
	if app.Spec.Git.SecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "ssh-key",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  app.Spec.Git.SecretName,
					DefaultMode: pointer.Int32(0400),
				},
			},
		})

		// Use bitnami/git which has better SSH support
		gitCloneContainer.Image = "bitnami/git:latest"
		gitCloneContainer.Args = []string{
			`set -e
	mkdir -p /root/.ssh
	cp /ssh-key/ssh-privatekey /root/.ssh/id_rsa
	chmod 400 /root/.ssh/id_rsa
	ssh-keyscan -H github.com > /root/.ssh/known_hosts
	chmod 400 /root/.ssh/known_hosts

	# Configure SSH to use the key
	cat > /root/.ssh/config << EOF
Host github.com
    StrictHostKeyChecking no
    IdentityFile /root/.ssh/id_rsa
EOF
	chmod 400 /root/.ssh/config

	# Clone repository
	git clone ` + app.Spec.Git.URL + ` /workspace/source
	cd /workspace/source
	git checkout ` + build.Spec.GitRef + `
	git rev-parse HEAD > /workspace/git-commit/sha`,
		}
		gitCloneContainer.VolumeMounts = append(gitCloneContainer.VolumeMounts, corev1.VolumeMount{
			Name:      "ssh-key",
			MountPath: "/ssh-key",
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      build.Name,
			Namespace: build.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      app.Name,
				"app.kubernetes.io/component": "build",
				"build.shapeblock.io/id":      build.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: pointer.Int32(0),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":      app.Name,
						"app.kubernetes.io/component": "build",
						"build.shapeblock.io/id":      build.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Affinity: func() *corev1.Affinity {
						if build.Spec.BuildNodeAffinity == nil {
							return nil
						}
						return &corev1.Affinity{
							NodeAffinity: build.Spec.BuildNodeAffinity,
						}
					}(),
					InitContainers: []corev1.Container{
						{
							Name:    "setup-platform",
							Image:   "busybox:1.36.1",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{r.generatePlatformScript(build.Spec.BuildVars)},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("32Mi"),
									corev1.ResourceCPU:    resource.MustParse("50m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("64Mi"),
									corev1.ResourceCPU:    resource.MustParse("100m"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "platform",
									MountPath: "/platform",
								},
							},
						},
						gitCloneContainer,
					},
					Containers: []corev1.Container{
						{
							Name:    "buildpack",
							Image:   app.Spec.Build.BuilderImage,
							Command: []string{"/cnb/lifecycle/creator"},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("512Mi"),
									corev1.ResourceCPU:    resource.MustParse("250m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("1Gi"),
									corev1.ResourceCPU:    resource.MustParse("500m"),
								},
							},
							Args: []string{
								"-app=/workspace/source",
								"-cache-dir=/workspace/cache",
								"-uid=1001",
								"-gid=1000",
								"-layers=/layers",
								"-platform=/platform",
								"-report=/layers/report.toml",
								"-skip-restore=false",
								fmt.Sprintf("-cache-image=%s/%s-cache:latest", app.Spec.Registry.URL, app.Name),
								fmt.Sprintf("%s/%s/%s:%s", app.Spec.Registry.URL, app.Namespace, app.Name, build.Spec.ImageTag),
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CNB_PLATFORM_API",
									Value: "0.12",
								},
							},
							VolumeMounts: r.getBuildpackVolumeMounts(),
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	// Use Create instead of CreateOrPatch for job creation
	if err := r.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			log.Info("Build job already exists, skipping creation",
				"job", build.Name,
				"namespace", build.Namespace)
			return nil
		}
		log.Error(err, "Failed to create build job",
			"jobName", job.Name,
			"namespace", job.Namespace)
		return err
	}

	return nil
}

func (r *AppBuildReconciler) getBuildpackVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{
			Name:      "source",
			MountPath: "/workspace/source",
		},
		{
			Name:      "cache",
			MountPath: "/workspace/cache",
		},
		{
			Name:      "layers",
			MountPath: "/layers",
		},
		{
			Name:      "platform",
			MountPath: "/platform",
		},
		{
			Name:      "registry-creds",
			MountPath: "/home/cnb/.docker",
			ReadOnly:  true,
		},
	}
}

func (r *AppBuildReconciler) getBuildVolumes(app *appsv1alpha1.App) []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "source",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: fmt.Sprintf("cache-%s", app.Name),
				},
			},
		},
		{
			Name: "layers",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: resource.NewQuantity(2*1024*1024*1024, resource.BinarySI), // 2Gi
				},
			},
		},
		{
			Name: "platform",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "registry-creds",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: func() string {
						if app.Spec.Registry.SecretName != "" {
							return app.Spec.Registry.SecretName
						}
						return "registry-creds"
					}(),
					Items: []corev1.KeyToPath{
						{
							Key:  ".dockerconfigjson",
							Path: "config.json",
						},
					},
				},
			},
		},
	}

	// Add SSH key volume for private repositories
	if app.Spec.Git.SecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "ssh-key",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  app.Spec.Git.SecretName,
					DefaultMode: pointer.Int32(0400),
				},
			},
		})
	}

	return volumes
}

func (r *AppBuildReconciler) generatePlatformScript(buildVars []appsv1alpha1.BuildVar) string {
	script := "mkdir -p /platform/env\n"

	// Add build-specific vars (these can override app-level vars)
	for _, v := range buildVars {
		script += fmt.Sprintf("printf \"%%s\" \"%s\" > /platform/env/%s\n", v.Value, v.Key)
	}

	return script
}

// Helper function to create PVC for build cache if it doesn't exist
func (r *AppBuildReconciler) ensureBuildCache(ctx context.Context, app *appsv1alpha1.App) error {
	cachePVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cache-%s", app.Name),
			Namespace: app.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      app.Name,
				"app.kubernetes.io/component": "build-cache",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("3Gi"),
				},
			},
		},
	}

	err := r.Create(ctx, cachePVC)
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create cache PVC: %v", err)
	}

	return nil
}

// CreateOrPatch creates or patches a Kubernetes object using server-side apply
func (r *AppBuildReconciler) CreateOrPatch(ctx context.Context, obj client.Object, mutate func() error) error {
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

// Add new function for Helm release management
func (r *AppBuildReconciler) createOrUpdateHelmRelease(ctx context.Context, app *appsv1alpha1.App, build *appsv1alpha1.AppBuild) error {
	log := ctrl.LoggerFrom(ctx)

	// Add debug logging
	log.Info("Received app and build objects",
		"app", app != nil,
		"build", build != nil,
		"appName", app.Name,
		"buildName", build.Name,
		"buildNamespace", build.Namespace)

	// Existing validation
	if app == nil {
		return fmt.Errorf("app is nil")
	}
	if build == nil {
		return fmt.Errorf("build is nil")
	}
	if app.Name == "" {
		return fmt.Errorf("app name is empty")
	}
	if build.Namespace == "" {
		return fmt.Errorf("build namespace is empty")
	}

	log.Info("Creating/Updating HelmChart",
		"app", app.Name,
		"namespace", build.Namespace,
		"buildName", build.Name)

	// Default values for nxs-universal-chart
	defaultValues := map[string]interface{}{}

	// Set image based on build type
	if app.Spec.Build.Type == "image" {
		repository, tag := utils.ParseImageString(app.Spec.Build.Image)
		defaultValues["defaultImage"] = repository
		defaultValues["defaultImageTag"] = tag
	} else {
		defaultValues["defaultImage"] = fmt.Sprintf("%s/%s/%s", app.Spec.Registry.URL, app.Namespace, app.Name)
		defaultValues["defaultImageTag"] = build.Spec.ImageTag
	}

	// If user provided helm values, merge them with defaults
	if build.Spec.HelmValues != nil {
		userValues := make(map[string]interface{})
		if err := json.Unmarshal(build.Spec.HelmValues.Raw, &userValues); err != nil {
			return fmt.Errorf("failed to parse helm values: %v", err)
		}
		defaultValues = mergeMaps(defaultValues, userValues)
	}

	// Convert the merged values to YAML
	valuesYAML, err := yaml.Marshal(defaultValues)
	if err != nil {
		return fmt.Errorf("failed to marshal helm values: %v", err)
	}

	helmChart := &helmv1.HelmChart{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "helm.cattle.io/v1",
			Kind:       "HelmChart",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: build.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "shapeblock-operator",
				"app.kubernetes.io/name":       app.Name,
			},
		},
		Spec: helmv1.HelmChartSpec{
			Chart:           "nxs-universal-chart",
			Version:         "2.8.1",
			Repo:            "https://registry.nixys.io/chartrepo/public",
			TargetNamespace: build.Namespace,
			ValuesContent:   string(valuesYAML),
		},
	}

	// First try to get existing HelmChart
	existing := &helmv1.HelmChart{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      app.Name,
		Namespace: build.Namespace,
	}, existing)

	// Create new HelmChart if it doesn't exist
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get HelmChart: %v", err)
		}
		log.Info("Creating new HelmChart",
			"appName", app.Name,
			"buildName", build.Name,
			"namespace", build.Namespace)
		return r.Create(ctx, helmChart)
	}

	// Check if update is needed by comparing specs
	if existing.Spec.Chart != helmChart.Spec.Chart ||
		existing.Spec.Version != helmChart.Spec.Version ||
		existing.Spec.Repo != helmChart.Spec.Repo ||
		existing.Spec.TargetNamespace != helmChart.Spec.TargetNamespace ||
		existing.Spec.ValuesContent != helmChart.Spec.ValuesContent {
		log.Info("Updating HelmChart due to spec changes",
			"name", existing.GetName(),
			"namespace", existing.GetNamespace())
		existing.Spec = helmChart.Spec
		return r.Update(ctx, existing)
	}

	return nil
}

// Helper function to merge maps with deep merge support
func mergeMaps(base, override map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	for k, v := range base {
		result[k] = v
	}

	for k, v := range override {
		if baseVal, ok := result[k]; ok {
			if baseMap, ok := baseVal.(map[string]interface{}); ok {
				if overrideMap, ok := v.(map[string]interface{}); ok {
					result[k] = mergeMaps(baseMap, overrideMap)
					continue
				}
			}
		}
		result[k] = v
	}

	return result
}

func (r *AppBuildReconciler) sendBuildStatus(build *appsv1alpha1.AppBuild) {
	if r.WebsocketClient == nil {
		return
	}

	update := utils.NewStatusUpdate("AppBuild", build.Name, build.Namespace)
	update.Status = build.Status.Phase
	update.Message = build.Status.Message
	update.PodName = build.Status.PodName
	update.AppName = build.Spec.AppName

	// Backend can use these labels to identify the build stream
	update.Labels = map[string]string{
		"build.shapeblock.io/id": build.Name,
	}

	r.WebsocketClient.SendStatus(update)
}

func (r *AppBuildReconciler) monitorHelmRelease(ctx context.Context, app *appsv1alpha1.App, build *appsv1alpha1.AppBuild) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// If build is already completed, don't requeue
	if build.Status.Phase == "Completed" {
		return ctrl.Result{}, nil
	}

	helmChart := &helmv1.HelmChart{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      app.Name,
		Namespace: build.Namespace,
	}, helmChart)

	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("Waiting for Helm chart to be created",
				"appName", app.Name)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// If JobName is empty, wait for it to be populated
	if helmChart.Status.JobName == "" {
		log.Info("Waiting for helm job to be created",
			"appName", app.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Get the helm install/upgrade job
	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      helmChart.Status.JobName,
		Namespace: build.Namespace,
	}, job)

	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Check job conditions
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			// Check if all pods are ready
			podList := &corev1.PodList{}
			if err := r.List(ctx, podList,
				client.InNamespace(build.Namespace),
				client.MatchingLabels{"app.kubernetes.io/name": app.Name}); err != nil {
				return ctrl.Result{}, err
			}

			allPodsReady := true
			for _, pod := range podList.Items {
				if pod.Status.Phase != corev1.PodRunning {
					allPodsReady = false
					break
				}
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status != corev1.ConditionTrue {
						allPodsReady = false
						break
					}
				}
			}

			if !allPodsReady {
				log.Info("Waiting for pods to be ready",
					"appName", app.Name,
					"buildName", build.Name)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}

			// Only update status if not already completed
			if build.Status.Phase != "Completed" {
				log.Info("Helm release completed successfully",
					"appName", app.Name,
					"buildName", build.Name)
				build.Status.Phase = "Completed"
				build.Status.Message = "Application deployed successfully"
				build.Status.CompletionTime = &metav1.Time{Time: time.Now()}
				if err := r.Status().Update(ctx, build); err != nil {
					return ctrl.Result{}, err
				}
				r.sendBuildStatus(build)
			}
			return ctrl.Result{}, nil
		}

		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			log.Error(nil, "Helm release failed",
				"appName", app.Name,
				"buildName", build.Name,
				"message", condition.Message)
			return r.failBuild(ctx, build, fmt.Sprintf("Helm release failed: %s", condition.Message))
		}
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *AppBuildReconciler) handleDeletion(ctx context.Context, build *appsv1alpha1.AppBuild) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	if controllerutil.ContainsFinalizer(build, buildFinalizer) {
		// Delete the build job
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      build.Name,
			Namespace: build.Namespace,
		}, job)

		if err == nil {
			// Job exists, delete it
			log.Info("Deleting build job",
				"job", job.Name,
				"namespace", job.Namespace)
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				if !errors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
			}
		} else if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(build, buildFinalizer)
		if err := r.Update(ctx, build); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *AppBuildReconciler) streamFinalLogs(ctx context.Context, build *appsv1alpha1.AppBuild, pod *corev1.Pod) error {
	// Skip if either client is not configured
	if r.WebsocketClient == nil || r.CoreV1Client == nil {
		return nil
	}

	req := r.CoreV1Client.Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		r.WebsocketClient.SendBuildLog(build.Name, scanner.Text()+"\n")
	}
	return scanner.Err()
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize WebsocketClient if env vars are set
	serverURL := os.Getenv("SHAPEBLOCK_WS_URL")
	apiKey := os.Getenv("SHAPEBLOCK_API_KEY")

	if serverURL != "" && apiKey != "" {
		client, err := utils.NewWebsocketClient(serverURL, apiKey)
		if err != nil {
			return fmt.Errorf("failed to create websocket client: %v", err)
		}
		r.WebsocketClient = client
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.AppBuild{}).
		Complete(r)
}
