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

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	helmv1 "github.com/k3s-io/helm-controller/pkg/apis/helm.cattle.io/v1"
	v1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	appsv1alpha1 "github.com/shapeblock/operator/api/v1alpha1"
	"github.com/shapeblock/operator/internal/controller"
	"github.com/shapeblock/operator/pkg/utils"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(appsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(v1.AddToScheme(scheme))
	utilruntime.Must(helmv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	var metricsOpts metricsserver.Options
	if metricsAddr != "0" {
		metricsOpts = metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			TLSOpts:       tlsOpts,
		}
		if secureMetrics {
			metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
		}
	}

	managerOpts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOpts,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
	}

	if enableLeaderElection {
		managerOpts.LeaderElection = true
		managerOpts.LeaderElectionID = "f7ef2605.shapeblock.io"
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), managerOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Get license details from environment
	licenseKey := os.Getenv("SHAPEBLOCK_LICENSE_KEY")
	licenseEmail := os.Getenv("SHAPEBLOCK_LICENSE_EMAIL")
	if licenseKey == "" || licenseEmail == "" {
		setupLog.Error(fmt.Errorf("SHAPEBLOCK_LICENSE_KEY and SHAPEBLOCK_LICENSE_EMAIL must be set"), "")
		os.Exit(1)
	}

	// Initialize websocket client
	wsClient, err := utils.NewWebsocketClient(
		os.Getenv("SHAPEBLOCK_WS_URL"),
		os.Getenv("SHAPEBLOCK_API_KEY"),
	)
	if err != nil {
		setupLog.Error(err, "unable to create websocket client")
		os.Exit(1)
	}
	defer wsClient.Close()

	if err = (&controller.ProjectReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Project")
		os.Exit(1)
	}
	if err = (&controller.AppBuildReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		WebsocketClient: wsClient,
		CoreV1Client:    kubernetes.NewForConfigOrDie(ctrl.GetConfigOrDie()).CoreV1(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AppBuild")
		os.Exit(1)
	}
	if err = (&controller.ServiceReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		WebsocketClient: wsClient,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Service")
		os.Exit(1)
	}
	if err = (&controller.AppReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		WebsocketClient: wsClient,
		GitClient:       utils.NewGitClient(os.Getenv("SHAPEBLOCK_API_URL"), os.Getenv("SHAPEBLOCK_API_KEY")),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "App")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
