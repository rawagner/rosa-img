package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ct "github.com/stolostron/cluster-templates-operator/api/v1alpha1"
	"github.com/stolostron/cluster-templates-operator/clusterprovider"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	argo "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/kubernetes-client/go-base/config/api"
	rbacv1 "k8s.io/api/rbac/v1"

	"rawagner/rosa/controllers"
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

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme.Scheme,
		/*
			Port:                   9443,
			HealthProbeBindAddress: probeAddr,
			LeaderElection:         enableLeaderElection,
			LeaderElectionID:       "135184d5.openshift.io",
			// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
			// when the Manager ends. This requires the binary to immediately end when the
			// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
			// speeds up voluntary leader transitions as the new leader don't have to wait
			// LeaseDuration time first.
			//
			// In the default scaffold provided, the program ends immediately after
			// the manager stops, so would be fine to enable this option. However,
			// if you are doing or is intended to do any operation such as perform cleanups
			// after the manager stops then its usage might be unsafe.
			// LeaderElectionReleaseOnCancel: true,
		*/
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

	listener := make(chan os.Signal, 1)
	signal.Notify(listener, os.Interrupt, syscall.SIGTERM)
	fmt.Println("received a shutdown signal:", <-listener)
	go func() {
		<-listener
		fmt.Println("received a shutdown signal")
		cmdDelete := exec.Command(
			"rosa", "delete", "cluster",
			"--region", os.Getenv("AWS_REGION"),
			"-c", os.Getenv("CLUSTER_NAME"),
		)
		err = runCmd(cmdDelete)
		if err != nil {
			fmt.Println(err)
		}
	}()
}

func Main1() {
	k8sClient := getClient()
	fmt.Println("Logging into ROSA")
	cmd := exec.Command("rosa", "login", "--token", os.Getenv("ROSA_TOKEN"))

	err := runCmd(cmd)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Creating ROSA cluster")
	cmd = exec.Command(
		"rosa", "create", "cluster",
		"--region", os.Getenv("AWS_REGION"),
		"--cluster-name", os.Getenv("CLUSTER_NAME"),
	)
	err = runCmd(cmd)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Get ROSA cluster status")
	clusterDescribe := &ClusterDescribe{}
	for {
		cmd := exec.Command(
			"rosa", "describe", "cluster",
			"-c", os.Getenv("CLUSTER_NAME"),
			"-o", "yaml",
		)
		out, err := cmd.Output()
		if err != nil {
			log.Fatal(err)
		}

		err = yaml.Unmarshal(out, clusterDescribe)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("Status: " + clusterDescribe.Status.State)
		if clusterDescribe.Status.State == "ready" {
			break
		}
		time.Sleep(5 * time.Minute)
	}

	fmt.Println("Create admin user")
	cmd = exec.Command(
		"rosa", "create", "admin",
		"-c", os.Getenv("CLUSTER_NAME"),
		"--region", os.Getenv("AWS_REGION"),
		"-o", "yaml",
	)

	out, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}

	adminCreds := &AdminCreds{}
	err = yaml.Unmarshal(out, adminCreds)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Cluster installed!")

	app := &argo.Application{}
	err = k8sClient.Get(
		context.TODO(),
		types.NamespacedName{
			Name:      os.Getenv("CLUSTER_NAME"),
			Namespace: os.Getenv("CLUSTER_NAMESPACE"),
		},
		app,
	)
	if err != nil {
		log.Fatal(err)
	}

	ctiName := app.Labels[ct.CTINameLabel]
	ctiNamespace := app.Labels[ct.CTINamespaceLabel]

	cti := &ct.ClusterTemplateInstance{}
	err = k8sClient.Get(
		context.TODO(),
		types.NamespacedName{
			Name:      ctiName,
			Namespace: ctiNamespace,
		},
		cti,
	)
	if err != nil {
		log.Fatal(err)
	}
	/*
		cti := &ct.ClusterTemplateInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "rosa-x1",
				Namespace: "devuserns",
			},
		}
		ctiName := "rosa-x1"
	*/

	newClusterClient := getClientForNewCluster(
		clusterDescribe.API.URL,
		adminCreds.Username,
		adminCreds.Password,
	)

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "admin-sa",
			Namespace: "kube-system",
		},
	}

	fmt.Println("Create SA")
	err = newClusterClient.Create(context.TODO(), sa)
	if err != nil {
		log.Fatal(err)
	}

	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: sa.Name + "-role",
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"*"},
				APIGroups: []string{"*"},
				Resources: []string{"*"},
			},
			{
				NonResourceURLs: []string{"*"},
				Verbs:           []string{"*"},
			},
		},
	}

	fmt.Println("Create CR")
	err = newClusterClient.Create(context.TODO(), clusterRole)
	if err != nil {
		log.Fatal(err)
	}

	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: sa.Name + "-role-binding",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRole.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      sa.Name,
				Namespace: sa.Namespace,
			},
		},
	}

	fmt.Println("Create CRB")
	err = newClusterClient.Create(context.TODO(), clusterRoleBinding)
	if err != nil {
		log.Fatal(err)
	}

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sa.Name + "-token",
			Namespace: sa.Namespace,
			Annotations: map[string]string{
				corev1.ServiceAccountNameKey: sa.Name,
			},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}

	fmt.Println("Create token Secret")
	err = newClusterClient.Create(context.TODO(), tokenSecret)
	if err != nil {
		log.Fatal(err)
	}

	//wait for secret to populate
	time.Sleep(1 * time.Minute)
	err = newClusterClient.Get(context.TODO(), client.ObjectKeyFromObject(tokenSecret), tokenSecret)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Create admin pass secret")

	kubeconfig := api.Config{
		Kind: "Config",
		Clusters: []api.NamedCluster{
			{
				Name: ctiName,
				Cluster: api.Cluster{
					Server:                   clusterDescribe.API.URL,
					CertificateAuthorityData: tokenSecret.Data["service-ca.crt"],
				},
			},
		},
		Contexts: []api.NamedContext{
			{
				Name: sa.Name,
				Context: api.Context{
					Cluster:  ctiName,
					AuthInfo: sa.Name,
				},
			},
		},
		CurrentContext: sa.Name,
		AuthInfos: []api.NamedAuthInfo{
			{
				Name: sa.Name,
				AuthInfo: api.AuthInfo{
					Token: string(tokenSecret.Data["token"]),
				},
			},
		},
	}

	kubeconfigBytes, err := json.Marshal(kubeconfig)
	if err != nil {
		log.Fatal(err)
	}

	err = clusterprovider.CreateClusterSecrets(
		context.TODO(),
		k8sClient,
		kubeconfigBytes,
		[]byte(adminCreds.Username),
		[]byte(adminCreds.Password),
		*cti,
	)

	if err != nil {
		log.Fatal(err)
	}

	cti.SetClusterInstallCondition(
		metav1.ConditionTrue,
		ct.ClusterInstalled,
		"Available",
	)

	err = k8sClient.Status().Update(context.TODO(), cti)
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}

func getClient() client.Client {
	cfg := ctrl.GetConfigOrDie()
	err := ct.AddToScheme(scheme.Scheme)
	if err != nil {
		panic(err)
	}
	err = argo.AddToScheme(scheme.Scheme)
	if err != nil {
		panic(err)
	}
	k8sClient, err := client.New(cfg, client.Options{})
	if err != nil {
		panic(err)
	}
	return k8sClient
}

func getClientForNewCluster(
	apiURL string,
	username string,
	password string,
) client.Client {
	fmt.Println(apiURL)
	fmt.Println(username)
	fmt.Println(password)
	for {
		fmt.Println("login")
		cmd := exec.Command(
			"oc", "login",
			apiURL,
			"-u", username,
			"-p", password,
			"--insecure-skip-tls-verify=true",
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		err := cmd.Start()
		if err != nil {
			fmt.Println(err)
			time.Sleep(15 * time.Second)
			continue
		}

		err = cmd.Wait()
		if err == nil {
			break
		}
		fmt.Println(err)
		time.Sleep(15 * time.Second)
	}
	fmt.Println("login successful")

	cmd := exec.Command(
		"oc", "whoami", "-t",
	)
	out, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}

	restCfg := &rest.Config{
		Host: apiURL,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
		BearerToken: strings.TrimSuffix(string(out), "\n"),
	}
	fmt.Println(string(out))
	client, err := client.New(restCfg, client.Options{})
	if err != nil {
		panic(err)
	}
	return client
}

func runCmd(cmd *exec.Cmd) error {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Start()
	if err != nil {
		return err
	}

	err = cmd.Wait()
	if err != nil {
		return err
	}
	return nil
}

/*
func CreateClusterSecrets(
	ctx context.Context,
	k8sClient client.Client,
	kubeconfig []byte,
	kubeadmin []byte,
	kubeadminpass []byte,
	templateInstance ct.ClusterTemplateInstance,
) error {
	kubeconfigSecret := corev1.Secret{}
	kubeconfigSecret.Name = templateInstance.GetKubeconfigRef()
	kubeconfigSecret.Namespace = templateInstance.Namespace

	err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&kubeconfigSecret), &kubeconfigSecret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			kubeconfigSecret.Data = map[string][]byte{
				"kubeconfig": kubeconfig,
			}
			//kubeconfigSecret.OwnerReferences = []metav1.OwnerReference{
			//	templateInstance.GetOwnerReference(),
			//}

			err := k8sClient.Create(ctx, &kubeconfigSecret)

			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	kubeadminSecret := corev1.Secret{}
	kubeadminSecret.Name = templateInstance.GetKubeadminPassRef()
	kubeadminSecret.Namespace = templateInstance.Namespace

	err = k8sClient.Get(ctx, client.ObjectKeyFromObject(&kubeadminSecret), &kubeadminSecret)

	if err != nil {
		if apierrors.IsNotFound(err) {
			kubeadminSecret.Data = map[string][]byte{
				"username": kubeadmin,
				"password": kubeadminpass,
			}
			//kubeadminSecret.OwnerReferences = []metav1.OwnerReference{
			//	templateInstance.GetOwnerReference(),
			//}

			err = k8sClient.Create(ctx, &kubeadminSecret)

			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	return nil
}
*/
