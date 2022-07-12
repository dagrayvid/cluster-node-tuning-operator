package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"runtime"

	apiconfigv1 "github.com/openshift/api/config/v1"
	performancev1 "github.com/openshift/cluster-node-tuning-operator/pkg/apis/performanceprofile/v1"
	performancev1alpha1 "github.com/openshift/cluster-node-tuning-operator/pkg/apis/performanceprofile/v1alpha1"
	performancev2 "github.com/openshift/cluster-node-tuning-operator/pkg/apis/performanceprofile/v2"
	paocontroller "github.com/openshift/cluster-node-tuning-operator/pkg/performanceprofile/controller"
	mcov1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	olmoperators "github.com/operator-framework/api/pkg/operators/install"
	olmv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tunedv1 "github.com/openshift/cluster-node-tuning-operator/pkg/apis/tuned/v1"
	"github.com/openshift/cluster-node-tuning-operator/pkg/config"
	"github.com/openshift/cluster-node-tuning-operator/pkg/metrics"
	"github.com/openshift/cluster-node-tuning-operator/pkg/operator"
	"github.com/openshift/cluster-node-tuning-operator/pkg/performanceprofile/cmd/render"
	"github.com/openshift/cluster-node-tuning-operator/pkg/signals"
	"github.com/openshift/cluster-node-tuning-operator/pkg/tuned"
	"github.com/openshift/cluster-node-tuning-operator/pkg/util"
	"github.com/openshift/cluster-node-tuning-operator/version"
)

const (
	operandFilename  = "openshift-tuned"
	operatorFilename = "cluster-node-tuning-operator"
	webhookPort      = 4343
	webhookCertDir   = "/apiserver.local.config/certificates"
	webhookCertName  = "apiserver.crt"
	webhookKeyName   = "apiserver.key"
)

var (
	scheme = apiruntime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(tunedv1.AddToScheme(scheme))
	utilruntime.Must(mcov1.AddToScheme(scheme))
	utilruntime.Must(apiconfigv1.Install(scheme))
	utilruntime.Must(performancev1alpha1.AddToScheme(scheme))
	utilruntime.Must(performancev1.AddToScheme(scheme))
	utilruntime.Must(performancev2.AddToScheme(scheme))
}

func printVersion() {
	klog.Infof("Go Version: %s", runtime.Version())
	klog.Infof("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH)
	klog.Infof("%s Version: %s", tunedv1.TunedClusterOperatorResourceName, version.Version)
}

var rootCmd = &cobra.Command{
	Use:   operatorFilename,
	Short: "NTO manages the containerized TuneD instances",
	Run: func(cmd *cobra.Command, args []string) {
		operatorRun()
	},
}

var enableLeaderElection bool
var showVersionAndExit bool

func prepareCommands() {
	rootCmd.PersistentFlags().BoolVar(&enableLeaderElection, "enable-leader-election", true,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	rootCmd.PersistentFlags().BoolVar(&showVersionAndExit, "version", false,
		"Show program version and exit.")

	// Include the klog command line arguments
	klog.InitFlags(nil)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	if !config.InHyperShift() {
		rootCmd.AddCommand(render.NewRenderCommand())
	}
}

func operatorRun() {
	printVersion()

	if showVersionAndExit {
		return
	}

	// We have two namespaces that we need to watch:
	// 1. NTO namespace - for NTO resources
	// 2. None namespace - for cluster wide resources
	ntoNamespace := config.WatchNamespace()
	namespaces := []string{
		ntoNamespace,
		metav1.NamespaceNone,
	}

	restConfig := ctrl.GetConfigOrDie()
	le := util.GetLeaderElectionConfig(restConfig, enableLeaderElection)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		NewCache:                cache.MultiNamespacedCacheBuilder(namespaces),
		Scheme:                  scheme,
		LeaderElection:          true,
		LeaderElectionID:        config.OperatorLockName,
		LeaderElectionNamespace: ntoNamespace,
		LeaseDuration:           &le.LeaseDuration.Duration,
		RetryPeriod:             &le.RetryPeriod.Duration,
		RenewDeadline:           &le.RenewDeadline.Duration,
		Namespace:               ntoNamespace,
	})

	if err != nil {
		klog.Exit(err)
	}

	if !config.InHyperShift() {
		if err := removePerformanceOLMOperator(restConfig); err != nil {
			klog.Fatalf("unable to remove Performance addons OLM operator: %v", err)
		}
	}

	controller, err := operator.NewController()
	if err != nil {
		klog.Fatalf("failed to create new controller: %v", err)
	}

	if err := mgr.Add(controller); err != nil {
		klog.Fatalf("failed to add new controller to the manager: %v", err)
	}

	if err := mgr.Add(metrics.Server{}); err != nil {
		klog.Fatalf("unable to add metrics server as runnable under the manager: %v", err)
	}
	metrics.RegisterVersion(version.Version)

	if !config.InHyperShift() {
		if err = (&paocontroller.PerformanceProfileReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: mgr.GetEventRecorderFor("performance-profile-controller"),
		}).SetupWithManager(mgr); err != nil {
			klog.Exitf("unable to create PerformanceProfile controller: %v", err)
		}

		// Configure webhook server.
		webHookServer := mgr.GetWebhookServer()
		webHookServer.Port = webhookPort
		webHookServer.CertDir = webhookCertDir
		webHookServer.CertName = webhookCertName
		webHookServer.KeyName = webhookKeyName

		if err = (&performancev1.PerformanceProfile{}).SetupWebhookWithManager(mgr); err != nil {
			klog.Exitf("unable to create PerformanceProfile v1 webhook: %v", err)
		}

		if err = (&performancev2.PerformanceProfile{}).SetupWebhookWithManager(mgr); err != nil {
			klog.Exitf("unable to create PerformanceProfile v2 webhook: %v", err)
		}
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		klog.Exitf("manager exited with non-zero code: %v", err)
	}
}

func tunedOperandRun() {
	var boolVersion bool
	flag.BoolVar(&boolVersion, "version", false, "show program version and exit")

	// flag.Parse is called from within tuned.Run -> parseCmdOpts
	// but the version flag variable is inherited from here..

	stopCh := signals.SetupSignalHandler()
	tuned.Run(stopCh, &boolVersion, version.Version)
}

// Uninstall PAO OLM operator since PAO is shipped
// as a core operator from 4.11.
// This is relevant for any upgrade path of an OpenShift cluster
// below 4.11 containing PAO to this current version.
func removePerformanceOLMOperator(cfg *rest.Config) error {
	k8sclient, err := client.New(cfg, client.Options{})
	if err != nil {
		return err
	}

	// Register OLM types to the client
	olmoperators.Install(k8sclient.Scheme())

	var performanceOperatorCSVs []olmv1alpha1.ClusterServiceVersion
	csvs := &olmv1alpha1.ClusterServiceVersionList{}
	if err := k8sclient.List(context.TODO(), csvs); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}
	for i := range csvs.Items {
		csv := &csvs.Items[i]
		deploymentSpecs := csv.Spec.InstallStrategy.StrategySpec.DeploymentSpecs
		if deploymentSpecs != nil {
			for _, deployment := range deploymentSpecs {
				if deployment.Name == "performance-operator" {
					performanceOperatorCSVs = append(performanceOperatorCSVs, *csv)
					break
				}
			}
		}
	}

	subscriptions := &olmv1alpha1.SubscriptionList{}
	if err := k8sclient.List(context.TODO(), subscriptions); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}
	for i := range subscriptions.Items {
		subscription := &subscriptions.Items[i]
		subscriptionExists := true
		for _, csv := range performanceOperatorCSVs {
			if subscription.Namespace == csv.Namespace && subscription.Status.InstalledCSV == csv.Name {
				if subscriptionExists {
					klog.Infof("Removing performance-addon-operator subscription %s", subscription.Name)
					if err := k8sclient.Delete(context.TODO(), subscription); err != nil {
						return err
					}
					subscriptionExists = false
				}
				klog.Infof("Removing performance-addon-operator related CSV %s/%s", csv.Name, csv.Namespace)
				if err := k8sclient.Delete(context.TODO(), &csv); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func main() {
	runAs := filepath.Base(os.Args[0])

	switch runAs {
	case operatorFilename:
		prepareCommands()
		_ = rootCmd.Execute()
	case operandFilename:
		tunedOperandRun()
	default:
		klog.Fatalf("application should be run as \"%s\" or \"%s\"", operatorFilename, operandFilename)
	}
}
