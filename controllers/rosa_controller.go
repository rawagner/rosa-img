package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	argo "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/kubernetes-client/go-base/config/api"
	ct "github.com/stolostron/cluster-templates-operator/api/v1alpha1"
	"github.com/stolostron/cluster-templates-operator/clusterprovider"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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

const ROSAFinalizer = "rosa.openshift.io/finalizer"

type RosaReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	EnableHypershift bool
}

func (r *RosaReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	fmt.Println("Reconcile")
	fmt.Println(req)
	app := &argo.Application{}
	if err := r.Client.Get(
		context.TODO(),
		types.NamespacedName{
			Name:      os.Getenv("CLUSTER_NAME"),
			Namespace: "argocd", //os.Getenv("CLUSTER_NAMESPACE"), TODO - Release.Namespace is destination.namespace ??
		},
		app,
	); err != nil {
		fmt.Println(err.Error())
		return ctrl.Result{}, err
	}

	ctiName := app.Labels[ct.CTINameLabel]
	ctiNamespace := app.Labels[ct.CTINamespaceLabel]

	cti := &ct.ClusterTemplateInstance{}
	if err := r.Client.Get(
		context.TODO(),
		types.NamespacedName{
			Name:      ctiName,
			Namespace: ctiNamespace,
		},
		cti,
	); err != nil {
		fmt.Println("2")
		fmt.Println(err.Error())
		return ctrl.Result{}, err
	}

	//delete event
	if req.NamespacedName.Name != "" && req.NamespacedName.Namespace != "" {
		if cti.GetName() == req.NamespacedName.Name && cti.GetNamespace() == req.NamespacedName.Namespace {
			cmdDelete := exec.Command(
				"rosa", "delete", "cluster",
				"--region", os.Getenv("AWS_REGION"),
				"-c", os.Getenv("CLUSTER_NAME"),
				"-y",
			)
			err := runCmd(cmdDelete)
			if err != nil {
				fmt.Println(err)
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(
				cti,
				ROSAFinalizer,
			)
			if err := r.Update(ctx, cti); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	if !controllerutil.ContainsFinalizer(cti, ROSAFinalizer) {
		fmt.Println("Add finalizer")
		cti.Finalizers = append(cti.Finalizers, ROSAFinalizer)
		if err := r.Client.Update(ctx, cti); err != nil {
			return ctrl.Result{}, err
		}
	}

	fmt.Println("Logging into ROSA")
	cmd := exec.Command("rosa", "login", "--token", os.Getenv("ROSA_TOKEN"))

	err := runCmd(cmd)
	if err != nil {
		return ctrl.Result{}, err
	}

	fmt.Println("find ROSA cluster")
	cmd = exec.Command(
		"rosa", "list", "cluster",
		"--region", os.Getenv("AWS_REGION"),
		"-o", "yaml",
	)

	out, err := cmd.Output()
	if err != nil {
		return ctrl.Result{}, err
	}
	clusterList := []ClusterDescribe{}
	err = yaml.Unmarshal(out, &clusterList)
	if err != nil {
		return ctrl.Result{}, err
	}
	found := false
	for _, cluster := range clusterList {
		if cluster.Name == os.Getenv("CLUSTER_NAME") {
			found = true
		}
	}

	if !found {
		fmt.Println("Creating ROSA cluster")
		cmd = exec.Command(
			"rosa", "create", "cluster",
			"--region", os.Getenv("AWS_REGION"),
			"--cluster-name", os.Getenv("CLUSTER_NAME"),
		)
		err = runCmd(cmd)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	} else {
		fmt.Println("ROSA cluster already exists")
	}

	fmt.Println("Get ROSA cluster status")
	clusterDescribe := &ClusterDescribe{}
	cmd = exec.Command(
		"rosa", "describe", "cluster",
		"-c", os.Getenv("CLUSTER_NAME"),
		"-o", "yaml",
	)
	out, err = cmd.Output()
	if err != nil {
		return ctrl.Result{}, err
	}

	err = yaml.Unmarshal(out, clusterDescribe)
	if err != nil {
		return ctrl.Result{}, err
	}
	fmt.Println("Status: " + clusterDescribe.Status.State)
	if clusterDescribe.Status.State != "ready" {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	fmt.Println("Create admin user")
	cmd = exec.Command(
		"rosa", "create", "admin",
		"-c", os.Getenv("CLUSTER_NAME"),
		"--region", os.Getenv("AWS_REGION"),
		"-o", "yaml",
	)

	out, err = cmd.Output()
	if err != nil {
		fmt.Println("err creating cluster admin")
		fmt.Println(err.Error())
		fmt.Println(out)
		return ctrl.Result{}, err
	}

	adminCreds := &AdminCreds{}
	err = yaml.Unmarshal(out, adminCreds)
	if err != nil {
		fmt.Println("err unmarshalling creds")
		fmt.Println("out: " + string(out))
		fmt.Println(err.Error())
		return ctrl.Result{}, err
	}

	fmt.Println("Cluster installed!")

	fmt.Println("Wait before cluster login")
	time.Sleep(5 * time.Minute)
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
		return ctrl.Result{}, err
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
		return ctrl.Result{}, err
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
		return ctrl.Result{}, err
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
		return ctrl.Result{}, err
	}

	//wait for secret to populate
	time.Sleep(1 * time.Minute)
	err = newClusterClient.Get(context.TODO(), client.ObjectKeyFromObject(tokenSecret), tokenSecret)
	if err != nil {
		return ctrl.Result{}, err
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
		return ctrl.Result{}, err
	}

	err = clusterprovider.CreateClusterSecrets(
		context.TODO(),
		r.Client,
		kubeconfigBytes,
		[]byte(adminCreds.Username),
		[]byte(adminCreds.Password),
		*cti,
	)

	if err != nil {
		return ctrl.Result{}, err
	}

	cti.SetClusterInstallCondition(
		metav1.ConditionTrue,
		ct.ClusterInstalled,
		"Available",
	)

	err = r.Client.Status().Update(context.TODO(), cti)
	return ctrl.Result{}, err
}

func (r *RosaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	fmt.Println("Setup with manager")
	initialSync := make(chan event.GenericEvent)
	err := ctrl.NewControllerManagedBy(mgr).
		For(&ct.ClusterTemplateInstance{}, builder.Predicates{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				fmt.Println("Create")
				fmt.Println(e.Object)
				fmt.Println("----")
				return false
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				fmt.Println("Update")
				fmt.Println(e.ObjectNew)
				fmt.Println("----")
				annotations := e.ObjectNew.GetAnnotations()
				if annotations != nil {
					_, ok := annotations[clusterprovider.ClusterProviderExperimentalAnnotation]
					return ok &&
						e.ObjectNew.GetDeletionTimestamp() != nil &&
						controllerutil.ContainsFinalizer(e.ObjectNew, ROSAFinalizer)
				}
				return false
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				fmt.Println("Delete")
				fmt.Println(e.Object)
				fmt.Println("----")
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				fmt.Println("Generic")
				fmt.Println(e.Object)
				fmt.Println("----")
				return e.Object.GetName() == "" && e.Object.GetNamespace() == ""
			},
		}).
		Watches(&source.Channel{Source: initialSync}, &handler.EnqueueRequestForObject{}).
		Complete(r)

	go func() {
		fmt.Println("Init sync run")
		initialSync <- event.GenericEvent{Object: &ct.ClusterTemplateInstance{}}
		fmt.Println("Init sync sent")
	}()
	return err
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
