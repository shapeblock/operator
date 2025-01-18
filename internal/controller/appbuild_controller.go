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

	// Check if build is already completed
	if build.Status.Phase == "Completed" || build.Status.Phase == "Failed" {
		return ctrl.Result{}, nil
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

		if build.Status.Phase == "" {
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
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      build.Name,
		Namespace: build.Namespace,
	}, pod)

	if err != nil {
		if errors.IsNotFound(err) {
			// Create new build pod
			if err := r.handleDockerfileBuild(ctx, app, build); err != nil {
				return r.failBuild(ctx, build, fmt.Sprintf("failed to create build pod: %v", err))
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Update status based on pod phase
	return r.updateBuildStatus(ctx, build, pod)
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
		if build.Status.Phase != "Completed" {
			// Stream final logs synchronously to ensure we capture everything
			if err := r.streamFinalLogs(ctx, build, pod); err != nil {
				log.Error(err, "Failed to capture final logs")
			}

			build.Status.Phase = "Completed"
			build.Status.Message = "Build completed successfully"
			build.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Build completed successfully",
				"buildName", build.Name,
				"appName", app.Name)

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
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *AppBuildReconciler) updateBuildStatus(ctx context.Context, build *appsv1alpha1.AppBuild, pod *corev1.Pod) (ctrl.Result, error) {
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
		if build.Status.Phase != "Completed" {
			build.Status.Phase = "Completed"
			build.Status.Message = "Build completed successfully"
			build.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
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
	// Create Kaniko pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-build-%s", app.Name, build.Name),
			Namespace: build.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":     "kaniko",
				"app.kubernetes.io/instance": build.Name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "kaniko",
				Image: "gcr.io/kaniko-project/executor:latest",
				Args: []string{
					"--dockerfile=Dockerfile",
					fmt.Sprintf("--context=%s", app.Spec.Git.URL),
					fmt.Sprintf("--destination=%s/%s:%s", app.Spec.Registry.URL, app.Name, build.Spec.ImageTag),
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "docker-config",
					MountPath: "/kaniko/.docker",
				}},
			}},
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{{
				Name: "docker-config",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: app.Spec.Registry.SecretName,
						Items: []corev1.KeyToPath{{
							Key:  ".dockerconfigjson",
							Path: "config.json",
						}},
					},
				},
			}},
		},
	}

	return r.Create(ctx, pod)
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
					InitContainers: []corev1.Container{
						{
							Name:    "setup-platform",
							Image:   "busybox:1.36.1",
							Command: []string{"/bin/sh", "-c"},
							Args:    []string{r.generatePlatformScript(build.Spec.BuildVars)},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "platform",
									MountPath: "/platform",
								},
							},
						},
						{
							Name:    "git-clone",
							Image:   "alpine/git:latest",
							Command: []string{"git", "clone", "--branch", build.Spec.GitRef, app.Spec.Git.URL, "/workspace/source"},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "source",
									MountPath: "/workspace/source",
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "buildpack",
							Image:   app.Spec.Build.BuilderImage,
							Command: []string{"/cnb/lifecycle/creator"},
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
					Volumes: r.getBuildVolumes(app),
				},
			},
		},
	}

	// Add SSH key volume for private repositories if needed
	if app.Spec.Git.SecretName != "" {
		job.Spec.Template.Spec.Volumes = append(
			job.Spec.Template.Spec.Volumes,
			corev1.Volume{
				Name: "ssh-key",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  app.Spec.Git.SecretName,
						DefaultMode: pointer.Int32(0400),
					},
				},
			},
		)

		// Update git clone container for SSH
		job.Spec.Template.Spec.InitContainers[1].Command = []string{"/bin/sh", "-c"}
		job.Spec.Template.Spec.InitContainers[1].Args = []string{
			`mkdir -p /tmp/.ssh
			 cp /root/.ssh/ssh-privatekey /tmp/.ssh/id_rsa
			 chmod 400 /tmp/.ssh/id_rsa
			 GIT_SSH_COMMAND="ssh -i /tmp/.ssh/id_rsa -o StrictHostKeyChecking=no" git clone --branch ` +
				build.Spec.GitRef + " " + app.Spec.Git.URL + " /workspace/source",
		}
		job.Spec.Template.Spec.InitContainers[1].VolumeMounts = append(
			job.Spec.Template.Spec.InitContainers[1].VolumeMounts,
			corev1.VolumeMount{
				Name:      "ssh-key",
				MountPath: "/root/.ssh",
			},
		)
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
					SecretName: app.Spec.Registry.SecretName,
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
					corev1.ResourceStorage: resource.MustParse("5Gi"),
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
	// First try to get existing HelmChart
	existing := &helmv1.HelmChart{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      app.Name,
		Namespace: build.Namespace,
	}, existing)

	// Default values for nxs-universal-chart
	defaultValues := map[string]interface{}{}

	// Set image based on build type
	if app.Spec.Build.Type == "image" {
		repository, tag := utils.ParseImageString(app.Spec.Build.Image)
		defaultValues["defaultImage"] = repository
		defaultValues["defaultImageTag"] = tag
	} else {
		defaultValues["defaultImage"] = fmt.Sprintf("%s/%s", app.Spec.Registry.URL, app.Name)
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: build.Namespace,
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "helm.cattle.io/v1",
			Kind:       "HelmChart",
		},
		Spec: helmv1.HelmChartSpec{
			Chart:           "nxs-universal-chart",
			Version:         "2.8.1",
			Repo:            "https://registry.nixys.io/chartrepo/public",
			TargetNamespace: build.Namespace,
			ValuesContent:   string(valuesYAML),
		},
	}

	if err != nil {
		if errors.IsNotFound(err) {
			return r.Create(ctx, helmChart)
		}
		return err
	}

	// Update existing chart
	existing.Spec = helmChart.Spec
	return r.Update(ctx, existing)
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

	log.Info("Monitoring Helm release",
		"appName", app.Name,
		"jobName", helmChart.Status.JobName)

	// Check helm chart status
	if helmChart.Status.JobName != "" {
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      helmChart.Status.JobName,
			Namespace: build.Namespace,
		}, job)

		if err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		if job.Status.Succeeded > 0 {
			log.Info("Helm release completed successfully",
				"appName", app.Name,
				"buildName", build.Name)
			build.Status.Message = "Helm release completed successfully"
			if err := r.Status().Update(ctx, build); err != nil {
				return ctrl.Result{}, err
			}
			r.sendBuildStatus(build)
			return ctrl.Result{}, nil
		}

		if job.Status.Failed > 0 {
			log.Error(nil, "Helm release failed",
				"appName", app.Name,
				"buildName", build.Name)
			return r.failBuild(ctx, build, "Helm release failed")
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
