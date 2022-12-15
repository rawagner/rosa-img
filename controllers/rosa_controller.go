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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"

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

const PodFinalizer = "rosa.openshift.io/finalizer"

type RosaReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	EnableHypershift bool
}

func (r *RosaReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if pod.GetName() != os.Getenv("POD_NAME") || pod.GetNamespace() != os.Getenv("POD_NAMESPACE") {
		return ctrl.Result{}, nil
	}
	fmt.Println("Logging into ROSA")
	cmd := exec.Command("rosa", "login", "--token", os.Getenv("ROSA_TOKEN"))

	err := runCmd(cmd)
	if err != nil {
		return ctrl.Result{}, err
	}

	if pod.GetDeletionTimestamp() != nil {
		if controllerutil.ContainsFinalizer(
			pod,
			PodFinalizer,
		) {
			cmdDelete := exec.Command(
				"rosa", "delete", "cluster",
				"--region", os.Getenv("AWS_REGION"),
				"-c", os.Getenv("CLUSTER_NAME"),
			)
			err = runCmd(cmdDelete)
			if err != nil {
				fmt.Println(err)
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(
				pod,
				PodFinalizer,
			)
			if err := r.Update(ctx, pod); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
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
		return ctrl.Result{}, err
	}

	adminCreds := &AdminCreds{}
	err = yaml.Unmarshal(out, adminCreds)
	if err != nil {
		return ctrl.Result{}, err
	}

	fmt.Println("Cluster installed!")

	app := &argo.Application{}
	err = r.Client.Get(
		context.TODO(),
		types.NamespacedName{
			Name:      os.Getenv("CLUSTER_NAME"),
			Namespace: os.Getenv("CLUSTER_NAMESPACE"),
		},
		app,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	ctiName := app.Labels[ct.CTINameLabel]
	ctiNamespace := app.Labels[ct.CTINamespaceLabel]

	cti := &ct.ClusterTemplateInstance{}
	err = r.Client.Get(
		context.TODO(),
		types.NamespacedName{
			Name:      ctiName,
			Namespace: ctiNamespace,
		},
		cti,
	)
	if err != nil {
		return ctrl.Result{}, err
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
	/*
		mapPod := func(pod client.Object) []reconcile.Request {
			reply := []reconcile.Request{}
			if pod.GetName() == os.Getenv("POD_NAME") && pod.GetNamespace() == os.Getenv("POD_NAMESPACE") {
				reply = append(reply, reconcile.Request{NamespacedName: types.NamespacedName{
					Namespace: pod.GetNamespace(),
					Name:      pod.GetName(),
				}})
			}
			return reply
		}
	*/

	err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).Complete(r)

	initialSync := make(chan event.GenericEvent)
	go func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      os.Getenv("POD_NAME"),
				Namespace: os.Getenv("POD_NAMESPACE"),
			},
		}
		initialSync <- event.GenericEvent{Object: pod}
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
