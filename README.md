I have built a Heroku like app which helps users abstract out the complexities of k8s and manages apps, services and teams/groups on top of k8s. It also has a bit of rancher like features where I can spin up and manage k3s based clusters on normal VMs(like Digitalocean or Hetzner using terraform, custom user provided machines using SSH) or manage existing EKS/AKS/GKE/DOKS clusters on the platform. It is meant to run on commodity hardware(typically 4GB RAM with room for user applications). This is a lot like https://epinio.io/. I call it shapeblock. I've built the first version on the following stack:
1. FastAPI with Uvicorn to handle websockets
2. postgres to handle the DB
3. Redis for caching
4. Tekton for handling k8s setups(running tf, installing k3s on client machines, scaling cluster nodes etc.)

I plan to offer it as a product which users can purchase and install using a one-time license. This comes in 2 offerings:

1. Single tenant: users spin up a VM and install k3s, nginx ingress, cert manager and the above shapeblock components. Users can add more nodes to this k3s cluster and manage apps in this cluster.
2. Multitenant: users can do everything in single tenant, plus create and manage multiple k8s clusters using the shapeblock installation in the host machine, plus additional stuff like teams, log retention and all the enterprise stuff.
I want to offer it as a SaaS also. The main goals are

1. minimal usage of resources
2. to give a great dev experience
3. flexibility and customizability

I want to build a custom operator which will be deployed in each k8s cluster managed by Shapeblock and communicates with the shapeblock server. This operator will be responsible for handling the lifecycle of the projects, apps, services and teams/groups.

Projects are just loaded namespaces. Apps are deployed as helm charts and are either:
1. built from source using dockerfile(kaniko) or buildpacks using a k8s job. This runs in the same namespace as the app.
2. these images are pushed to a registry, usually a private registry in the cluster or a private registry provided by the shapeblock server.
3. Once the image is pushed, the operator will deploy the app using helm. We mostly use https://github.com/nixys/nxs-universal-chart.
4. Services are also deployed as helm charts.
5. helm chart deployment is done using a helm operator(https://github.com/k3s-io/helm-controller).

The idea of using operator is to offload the task of managing the apps, services and teams/groups to the operator so that the shapeblock server can focus on the business logic and other incoming requests. The server just keeps record of the projects, apps, services and builds. The actual tasks happen in the respective clusters orchestrated by the operator.

### Pros of Kubernetes Operator:

#### Client-Side Resource Management:
Builds run entirely in client's cluster
No need to monitor from your control plane
Better security as client data stays in their cluster

#### Native K8s Integration:
Uses K8s native concepts (CRDs, controllers)
Better resource management
Automatic reconciliation

#### Scalability:
Each client cluster handles its own workload
Your control plane only needs to store metadata
Reduced network traffic between control and client clusters


   Your Control Plane (FastAPI)        Client Cluster
   +------------------+               +------------------+
   |                  |               |                  |
   | - User Auth      |  kubectl      | - Operator      |
   | - Project Meta   +-------------->| - Builds        |
   | - Billing        |               | - Deployments   |
   | - API            |               | - Apps          |
   |                  |               |                  |
   +------------------+               +------------------+

#### Resource Usage:
Control plane becomes very lightweight
No need for Celery/Redis
Minimal monitoring overhead

```go
// controllers/appbuild_controller.go
func (r *AppBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    build := &shapeblockkv1.AppBuild{}
    if err := r.Get(ctx, req.NamespacedName, build); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // Create build pod
    buildPod := &corev1.Pod{
        // Pod spec for buildpack
    }
    if err := r.Create(ctx, buildPod); err != nil {
        return ctrl.Result{}, err
    }

    // Update status
    build.Status.Phase = "Building"
    if err := r.Status().Update(ctx, build); err != nil {
        return ctrl.Result{}, err
    }

    return ctrl.Result{}, nil
}
```

```python
@router.post("/apps/{app_id}/builds")
async def create_build(
    app_id: UUID,
    build_config: BuildConfig,
    current_user: User = Depends(get_current_user)
):
    # Create build CR in client's cluster
    build_manifest = {
        "apiVersion": "shapeblock.io/v1",
        "kind": "AppBuild",
        "metadata": {
            "name": f"build-{uuid.uuid4()}",
            "namespace": app.project.name
        },
        "spec": build_config.dict()
    }

    k8s_client = get_cluster_client(app.project.cluster)
    await k8s_client.create_namespaced_custom_object(
        group="shapeblock.io",
        version="v1",
        namespace=app.project.name,
        plural="appbuilds",
        body=build_manifest
    )
```

Move to an operator-based approach because:
- Better separation of concerns
- More scalable SaaS architecture
- Reduced control plane complexity
- Better security model (client data stays in their cluster)
- Native K8s integration

Keep in FastAPI:
- User management
- Project metadata
- Billing/usage tracking
- API endpoints for creating/managing resources
- Status updates via K8s watch/informers

Move to Operator:
- Build process management
- Deployment orchestration
- Resource cleanup
- Health checks
- Auto-scaling

This approach would:
- Reduce your control plane footprint
- Improve scalability
- Better security model
- More native K8s experience
- Easier to maintain and debug

## More context

This project uses Operator SDK to generate the operator. The operator is deployed in each k8s cluster managed by Shapeblock. The operator is responsible for handling the lifecycle of the projects, apps, app builds and services.

The operator relays the status of these resources to the shapeblock server via websockets. The shapeblock server then updates the UI with the status of the resources.

## Project CR

```yaml
apiVersion: apps.shapeblock.io/v1alpha1
kind: Project
metadata:
  name: my-project
spec:
  name: "my-project"
  displayName: "My Project"
  description: "A sample project"
```

Handles namespace creation, copies registry secret from "shapeblock" namespace to the newly created namespace.

## App CR


```yaml
apiVersion: apps.shapeblock.io/v1alpha1
kind: App
metadata:
  name: sample-app
  namespace: my-project
spec:
  displayName: "Sample Application"
  description: "A sample application to demonstrate the App CR"

  git:
    url: "https://github.com/example/sample-app.git"
    branch: "main"
    secretName: "git-credentials"  # Optional
    isPrivate: true

  registry:
    url: "registry.shapeblock.io"
    secretName: "registry-credentials"

status:
  phase: "Pending"
  message: "Initializing application"
  latestBuild: ""
  lastUpdated: "2024-03-20T10:00:00Z"
# Dockerfile build
apiVersion: apps.shapeblock.io/v1alpha1
kind: App
metadata:
  name: dockerfile-app
  namespace: my-project
spec:
  displayName: "Dockerfile App"
  description: "App built using Dockerfile"
  git:
    url: "https://github.com/example/app.git"
    branch: "main"
  registry:
    url: "registry.shapeblock.io"
    secretName: "registry-credentials"
  build:
    type: "dockerfile"
---
# Buildpack build
apiVersion: apps.shapeblock.io/v1alpha1
kind: App
metadata:
  name: buildpack-app
  namespace: my-project
spec:
  displayName: "Buildpack App"
  description: "App built using Cloud Native Buildpacks"
  git:
    url: "https://github.com/example/app.git"
    branch: "main"
  registry:
    url: "registry.shapeblock.io"
    secretName: "registry-credentials"
  build:
    type: "buildpack"
    builderImage: "paketobuildpacks/builder:base"
---
# Pre-built image
apiVersion: apps.shapeblock.io/v1alpha1
kind: App
metadata:
  name: prebuilt-app
  namespace: my-project
spec:
  displayName: "Pre-built App"
  description: "App using existing container image"
  git:
    url: "https://github.com/example/app.git"
    branch: "main"
  registry:
    url: "registry.shapeblock.io"
    secretName: "registry-credentials"
  build:
    type: "image"
    image: "registry.shapeblock.io/my-project/sample-app:v1.0.0"
```

Creates app ssh secret, nothing more. Also, stores the meta information like git url, registry info etc. Decides the build strategy(dockerfile or buildpacks).

## AppBuild CR

```yaml
apiVersion: apps.shapeblock.io/v1alpha1
kind: AppBuild
metadata:
  name: sample-app-build-1
  namespace: my-project
spec:
  appName: "sample-app"    # References the App CR name
  gitRef: "main"          # Git reference to build (commit, branch, tag)
  imageTag: "v1.0.0"      # Tag for the built image

  buildVars:              # Optional build environment variables
    - key: "NODE_ENV"
      value: "production"
    - key: "BUILD_FLAG"
      value: "true"

  helmValues:             # Optional Helm values for deployment
    replicaCount: 2
    env:
      NODE_ENV: "production"
    ingress:
      enabled: true
      hosts:
        - "myapp.example.com"

status:
  phase: "Pending"        # Pending, Building, Completed, Failed
  message: "Build initialized"
  podName: ""            # Will be set when build pod is created
  startTime: "2024-03-20T10:00:00Z"
  completionTime: ""     # Will be set when build completes
```

Handles the build process.

# Helm chart deploy

```sh
# Package the chart
helm package helm/shapeblock-operator

# Login to GHCR
echo $GITHUB_TOKEN | helm registry login ghcr.io -u USERNAME --password-stdin

# Push the chart
helm push shapeblock-operator-0.1.3.tgz oci://ghcr.io/shapeblock/charts
```

# TODOs
- [x] identify patchable fields in app and appbuild CR
- [x] service CR
- [x] test dockerfile build
- [x] kaniko cache
- [ ] different dockerfile
- [x] test buildpack build
- [x] test pre-built image build
- [x] Add resource limits to appbuild job
- [ ] image reuse
- [x] failure scenarios
- [ ] identify and optimize websocket flow
- [x] test private repo creation(and deletion)
- [x] previous image as status field for App CR
- [ ] create a github action for the controller
- [x] record build timestamps in appbuild CR
- [x] add labels to appbuild job
