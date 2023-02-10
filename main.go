package main

import (
	"fmt"
	"os"

	ct "github.com/stolostron/cluster-templates-operator/api/v1alpha1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	argo "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"

	"rawagner/rosa/controllers"

	mce "open-cluster-management.io/api/cluster/v1"
)

type AdminCreds struct {
	Password string `json:"password"`
	Username string `json:"username"`
}

type ClusterAPI struct {
	URL string `json:"url"`
}
type ClusterStatus struct {
	State string `json:"state"`
}

type ClusterDescribe struct {
	Status ClusterStatus `json:"status"`
	API    ClusterAPI    `json:"api"`
	Name   string        `json:"string"`
}

//ROSA_TOKEN
//AWS_ACCESS_KEY_ID
//AWS_SECRET_ACCESS_KEY
//AWS_REGION
//CLUSTER_NAME
//CLUSTER_NAMESPACE

var (
	setupLog = ctrl.Log.WithName("setup")
)

func main() {
	config := ctrl.GetConfigOrDie()
	err := ct.AddToScheme(scheme.Scheme)
	if err != nil {
		panic(err)
	}
	err = argo.AddToScheme(scheme.Scheme)
	if err != nil {
		panic(err)
	}
	err = mce.AddToScheme(scheme.Scheme)
	if err != nil {
		panic(err)
	}

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.RosaReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		fmt.Println(err)
		setupLog.Error(err, "unable to create controller", "controller", "ClusterTemplateQuota")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
