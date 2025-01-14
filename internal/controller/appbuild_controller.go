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
	"encoding/json"
	"fmt"
	"io"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"bytes"

	helmv1 "github.com/shapeblock/operator/api/v1"
	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	"github.com/shapeblock/operator/utils"
	"gopkg.in/yaml.v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
)

// AppBuildReconciler reconciles a AppBuild object
type AppBuildReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	WebsocketClient *utils.WebsocketClient
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
	log := log.FromContext(ctx)

	build := &appsv1alpha1.AppBuild{}
	if err := r.Get(ctx, req.NamespacedName, build); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if build is already completed or failed
	if build.Status.Phase == "Completed" || build.Status.Phase == "Failed" {
		return ctrl.Result{}, nil
	}

	// If job doesn't exist, create it
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      fmt.Sprintf("build-%s-%s", build.Spec.AppName, build.Spec.ImageTag),
		Namespace: build.Namespace,
	}, job)

	if err != nil && errors.IsNotFound(err) {
		// Create new job based on build type
		switch build.Spec.BuildType {
		case "buildpack":
			if err := r.handleBuildpackBuild(ctx, build); err != nil {
				return r.failBuild(ctx, build, err.Error())
			}
			// ... other build types ...
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Monitor job status
	if err := r.updateBuildStatus(ctx, build, job); err != nil {
		log.Error(err, "failed to update build status")
		return ctrl.Result{}, err
	}

	// If build is completed, create Helm release
	if build.Status.Phase == "Completed" {
		if err := r.createOrUpdateHelmRelease(ctx, build); err != nil {
			log.Error(err, "failed to create/update helm release")
			build.Status.Phase = "Failed"
			build.Status.Message = fmt.Sprintf("Failed to deploy: %v", err)
			return ctrl.Result{}, r.Status().Update(ctx, build)
		}
	}

	// Requeue while job is running
	if build.Status.Phase == "Building" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *AppBuildReconciler) updateBuildStatus(ctx context.Context, build *appsv1alpha1.AppBuild, job *batchv1.Job) error {
	// Update build phase based on job status
	if job.Status.Failed > 0 {
		build.Status.Phase = "Failed"
		build.Status.Message = "Build job failed"
		build.Status.CompletionTime = &metav1.Time{Time: time.Now()}

		// Get the pod for logs
		pod, err := r.getBuildPod(ctx, build)
		if err == nil && pod != nil {
			logs, err := r.getPodLogs(ctx, pod)
			if err == nil {
				build.Status.Logs = logs
			}
		}
	} else if job.Status.Succeeded > 0 {
		build.Status.Phase = "Completed"
		build.Status.Message = "Build completed successfully"
		build.Status.CompletionTime = &metav1.Time{Time: time.Now()}

		// Get the pod for logs
		pod, err := r.getBuildPod(ctx, build)
		if err == nil && pod != nil {
			logs, err := r.getPodLogs(ctx, pod)
			if err == nil {
				build.Status.Logs = logs
			}
		}
	} else {
		// Job is still running
		build.Status.Phase = "Building"
		build.Status.Message = "Build in progress"

		// Get the pod for status updates
		pod, err := r.getBuildPod(ctx, build)
		if err == nil && pod != nil {
			build.Status.PodName = pod.Name
			build.Status.PodNamespace = pod.Namespace
		}
	}

	// Update status in cluster
	if err := r.Status().Update(ctx, build); err != nil {
		return fmt.Errorf("failed to update build status: %v", err)
	}

	// Send status update to server
	r.sendBuildStatus(build)

	return nil
}

func (r *AppBuildReconciler) getBuildPod(ctx context.Context, build *appsv1alpha1.AppBuild) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(build.Namespace),
		client.MatchingLabels{
			"build.shapeblock.io/id": build.Name,
		}); err != nil {
		return nil, err
	}

	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no pod found for build %s", build.Name)
	}

	return &podList.Items[0], nil
}

func (r *AppBuildReconciler) getPodLogs(ctx context.Context, pod *corev1.Pod) (string, error) {
	req := r.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: "buildpack", // or the appropriate container name
		Follow:    false,
		Previous:  false,
	})

	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("error in opening stream: %v", err)
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return "", fmt.Errorf("error in copy information from podLogs to buf: %v", err)
	}

	return buf.String(), nil
}

func (r *AppBuildReconciler) failBuild(ctx context.Context, build *appsv1alpha1.AppBuild, message string) (ctrl.Result, error) {
	build.Status.Phase = "Failed"
	build.Status.Message = message
	build.Status.CompletionTime = &metav1.Time{Time: time.Now()}

	if err := r.Status().Update(ctx, build); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *AppBuildReconciler) handleDockerfileBuild(ctx context.Context, build *appsv1alpha1.AppBuild) error {
	// Create Kaniko pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-build", build.Name),
			Namespace: build.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":     "kaniko",
				"app.kubernetes.io/instance": build.Name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "kaniko",
					Image: "gcr.io/kaniko-project/executor:latest",
					Args: []string{
						"--dockerfile=Dockerfile",
						fmt.Sprintf("--context=%s", build.Spec.Source.Git.URL),
						fmt.Sprintf("--destination=%s", build.Spec.TargetImage),
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "docker-config",
							MountPath: "/kaniko/.docker",
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{
				{
					Name: "docker-config",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: "registry-credentials",
							Items: []corev1.KeyToPath{
								{
									Key:  ".dockerconfigjson",
									Path: "config.json",
								},
							},
						},
					},
				},
			},
		},
	}

	if err := r.Create(ctx, pod); err != nil {
		return fmt.Errorf("failed to create build pod: %v", err)
	}

	return nil
}

func (r *AppBuildReconciler) handleBuildpackBuild(ctx context.Context, build *appsv1alpha1.AppBuild) error {
	log := log.FromContext(ctx)

	if err := r.ensureBuildCache(ctx, build); err != nil {
		return err
	}

	// Create build job
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("build-%s-%s", build.Spec.AppName, build.Spec.ImageTag),
			Namespace: build.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      build.Spec.AppName,
				"app.kubernetes.io/component": "build",
				"build.shapeblock.io/id":      build.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: pointer.Int32(0),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":      build.Spec.AppName,
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
						r.createGitCloneContainer(build),
					},
					Containers: []corev1.Container{
						{
							Name:    "buildpack",
							Image:   build.Spec.BuilderImage,
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
								fmt.Sprintf("-previous-image=%s", build.Spec.PreviousImage),
								fmt.Sprintf("-cache-image=%s/%s-cache:latest", build.Spec.RegistryURL, build.Spec.AppName),
								fmt.Sprintf("%s/%s:%s", build.Spec.RegistryURL, build.Spec.AppName, build.Spec.ImageTag),
							},
							Env: []corev1.EnvVar{
								{
									Name:  "CNB_PLATFORM_API",
									Value: "0.12",
								},
							},
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
							VolumeMounts: r.getBuildpackVolumeMounts(),
						},
					},
					Volumes: r.getBuildVolumes(build),
				},
			},
		},
	}

	if err := r.Create(ctx, job); err != nil {
		return fmt.Errorf("failed to create build job: %v", err)
	}

	// Update build status
	build.Status.Phase = "Building"
	if err := r.Status().Update(ctx, build); err != nil {
		log.Error(err, "failed to update build status")
	}

	return nil
}

func (r *AppBuildReconciler) generatePlatformScript(buildVars []appsv1alpha1.BuildVar) string {
	script := "mkdir -p /platform/env\n"
	for _, buildVar := range buildVars {
		script += fmt.Sprintf("printf \"%%s\" \"%s\" > /platform/env/%s\n", buildVar.Value, buildVar.Key)
	}
	return script
}

func (r *AppBuildReconciler) createGitCloneContainer(build *appsv1alpha1.AppBuild) corev1.Container {
	container := corev1.Container{
		Name:  "git-clone",
		Image: "alpine/git:latest",
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
		},
	}

	if build.Spec.IsPrivate {
		container.Command = []string{"/bin/sh", "-c"}
		container.Args = []string{
			`mkdir -p /tmp/.ssh
			 cp /root/.ssh/ssh-privatekey /tmp/.ssh/id_rsa
			 chmod 400 /tmp/.ssh/id_rsa
			 GIT_SSH_COMMAND="ssh -i /tmp/.ssh/id_rsa -o StrictHostKeyChecking=no" git clone --branch ` +
				build.Spec.GitRef + " " + build.Spec.GitRepo + " /workspace/source",
		}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "ssh-key",
			MountPath: "/root/.ssh",
		})
	} else {
		container.Command = []string{"git", "clone", "--branch", build.Spec.GitRef, build.Spec.GitRepo, "/workspace/source"}
	}

	return container
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

func (r *AppBuildReconciler) getBuildVolumes(build *appsv1alpha1.AppBuild) []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "source",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: resource.NewQuantity(1*1024*1024*1024, resource.BinarySI), // 1Gi
				},
			},
		},
		{
			Name: "cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: fmt.Sprintf("cache-%s", build.Spec.AppName),
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
					SecretName: "registry-creds",
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
	if build.Spec.IsPrivate {
		volumes = append(volumes, corev1.Volume{
			Name: "ssh-key",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  fmt.Sprintf("%s-ssh", build.Spec.AppName),
					DefaultMode: pointer.Int32(0400),
				},
			},
		})
	}

	return volumes
}

// Helper function to create PVC for build cache if it doesn't exist
func (r *AppBuildReconciler) ensureBuildCache(ctx context.Context, build *appsv1alpha1.AppBuild) error {
	cachePVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cache-%s", build.Spec.AppName),
			Namespace: build.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      build.Spec.AppName,
				"app.kubernetes.io/component": "build-cache",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.ResourceRequirements{
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

// Add new function for Helm release management
func (r *AppBuildReconciler) createOrUpdateHelmRelease(ctx context.Context, build *appsv1alpha1.AppBuild) error {
	// Default values for nxs-universal-chart
	defaultValues := map[string]interface{}{
		"image": map[string]interface{}{
			"repository": fmt.Sprintf("%s/%s", build.Spec.RegistryURL, build.Spec.AppName),
			"tag":        build.Spec.ImageTag,
			"pullPolicy": "IfNotPresent",
		},
	}

	// If user provided helm values, merge them with defaults
	if build.Spec.HelmValues != nil {
		userValues := make(map[string]interface{})
		if err := json.Unmarshal(build.Spec.HelmValues.Raw, &userValues); err != nil {
			return fmt.Errorf("failed to parse helm values: %v", err)
		}
		// Merge user values with defaults (user values take precedence)
		defaultValues = mergeMaps(defaultValues, userValues)
	}

	// Convert the merged values to YAML
	valuesYAML, err := yaml.Marshal(defaultValues)
	if err != nil {
		return fmt.Errorf("failed to marshal helm values: %v", err)
	}

	helmChart := &helmv1.HelmChart{
		ObjectMeta: metav1.ObjectMeta{
			Name:      build.Spec.AppName,
			Namespace: build.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      build.Spec.AppName,
				"app.kubernetes.io/component": "application",
			},
		},
		Spec: helmv1.HelmChartSpec{
			Chart:         "nxs-universal-chart",
			Version:       "1.0.0",
			Repo:          "https://registry.nixys.ru/chartrepo/public",
			ValuesContent: string(valuesYAML),
		},
	}

	// Create or update the HelmChart resource
	err = r.Get(ctx, client.ObjectKey{Name: helmChart.Name, Namespace: helmChart.Namespace}, &helmv1.HelmChart{})
	if err != nil {
		if err := r.Create(ctx, helmChart); err != nil {
			return fmt.Errorf("failed to create helm chart: %v", err)
		}
	} else {
		if err := r.Update(ctx, helmChart); err != nil {
			return fmt.Errorf("failed to update helm chart: %v", err)
		}
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

// SetupWithManager sets up the controller with the Manager.
func (r *AppBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.AppBuild{}).
		Complete(r)
}
