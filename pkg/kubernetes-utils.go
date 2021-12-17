package keptnkubeutils

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"k8s.io/client-go/dynamic"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"

	goutils "github.com/keptn/go-utils/pkg/api/utils"
	appsv1 "k8s.io/api/apps/v1"
	typesv1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	apierr "k8s.io/apimachinery/pkg/api/errors"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"

	// Initialize all known client auth plugins.
	_ "github.com/Azure/go-autorest/autorest"
	"github.com/keptn/go-utils/pkg/api/models"
	utils "github.com/keptn/go-utils/pkg/api/utils"

	// Initialize all known client auth plugins.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const keptnFolderName = ".keptn"

// GetKeptnDirectory returns a path, which is used to store logs and possibly creds
func GetKeptnDirectory() (string, error) {

	keptnDir := UserHomeDir() + string(os.PathSeparator) + keptnFolderName + string(os.PathSeparator)

	if _, err := os.Stat(keptnDir); os.IsNotExist(err) {
		err := os.MkdirAll(keptnDir, os.ModePerm)
		fmt.Println("keptn creates the folder " + keptnDir + " to store logs and possibly creds.")
		if err != nil {
			return "", err
		}
	}

	return keptnDir, nil
}

// UserHomeDir returns the HOME directory by taking into account the operating system
func UserHomeDir() string {
	if runtime.GOOS == "windows" {
		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		return home
	}
	return os.Getenv("HOME")
}

// ExpandTilde expands ~ to HOME
func ExpandTilde(fileName string) string {
	if fileName == "~" {
		return UserHomeDir()
	} else if strings.HasPrefix(fileName, "~/") {
		return filepath.Join(UserHomeDir(), fileName[2:])
	}
	return fileName
}

// GetFiles returns a list of files in a directory filtered by the provided suffix
func GetFiles(workingPath string, suffixes ...string) ([]string, error) {
	var files []string
	err := filepath.Walk(workingPath, func(path string, info os.FileInfo, err error) error {
		for _, suffix := range suffixes {
			if strings.HasSuffix(path, suffix) {
				files = append(files, path)
				break
			}
		}
		return nil
	})
	return files, err
}

// RestartPodsWithSelector restarts the pods which are found in the provided namespace and selector
func RestartPodsWithSelector(useInClusterConfig bool, namespace string, selector string) error {
	clientset, err := GetKubeAPI(useInClusterConfig)
	if err != nil {
		return err
	}
	pods, err := clientset.Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if err := clientset.Pods(namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func WaitForPodsWithSelector(useInClusterConfig bool, namespace string, selector string,
	retries int, waitingTime time.Duration) error {

	clientset, err := GetKubeAPI(useInClusterConfig)
	if err != nil {
		return err
	}

	for i := 0; i < retries; i++ {
		pods, err := clientset.Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return err
		}
		for _, pod := range pods.Items {
			for _, cond := range pod.Status.Conditions {
				if cond.Type != typesv1.PodReady {
					time.Sleep(waitingTime)
					break
				}
			}
		}
	}
	return nil
}

func ScaleDeployment(useInClusterConfig bool, deployment string, namespace string, replicas int32) error {
	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return err
	}
	deploymentsClient := clientset.AppsV1().Deployments(namespace)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Retrieve the latest version of Deployment before attempting update
		// RetryOnConflict uses exponential backoff to avoid exhausting the apiserver
		result, getErr := deploymentsClient.Get(context.TODO(), deployment, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("Failed to get latest version of Deployment: %v", getErr)
		}

		result.Spec.Replicas = int32Ptr(replicas)
		_, updateErr := deploymentsClient.Update(context.TODO(), result, metav1.UpdateOptions{})
		return updateErr
	})
	return retryErr
}

func int32Ptr(i int32) *int32 { return &i }

// WaitForDeploymentToBeRolledOut waits until the deployment is Available
func WaitForDeploymentToBeRolledOut(useInClusterConfig bool, deploymentName string, namespace string) error {
	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return err
	}

	const maxWaitForDeploymentRetries = 90
	deployment, err := getDeployment(clientset, namespace, deploymentName)
	retries := 0
	for {

		var cond *appsv1.DeploymentCondition

		for i := range deployment.Status.Conditions {
			c := deployment.Status.Conditions[i]
			if c.Type == appsv1.DeploymentProgressing {
				cond = &c
				break
			}
		}

		if cond != nil && cond.Reason == "ProgressDeadlineExceeded" {
			return fmt.Errorf("Deployment %q exceeded its progress deadline", deployment.Name)
		}
		if !(deployment.Spec.Replicas != nil && deployment.Status.UpdatedReplicas < *deployment.Spec.Replicas ||
			deployment.Status.Replicas > deployment.Status.UpdatedReplicas ||
			deployment.Status.AvailableReplicas < deployment.Status.UpdatedReplicas) {
			return nil
		}

		time.Sleep(2 * time.Second)
		deployment, err = getDeployment(clientset, namespace, deploymentName)
		if err != nil {
			return err
		}
		retries = retries + 1
		if retries >= maxWaitForDeploymentRetries {
			return fmt.Errorf("Timed out waiting for deployment %q", deployment.Name)
		}
	}
}

// WaitForDeploymentsInNamespace waits until all deployments in a namespace are available
func WaitForDeploymentsInNamespace(useInClusterConfig bool, namespace string) error {
	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return err
	}
	deps, err := clientset.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
	for _, dep := range deps.Items {
		if err := WaitForDeploymentToBeRolledOut(useInClusterConfig, dep.Name, namespace); err != nil {
			return err
		}
	}
	return nil
}

func getDeployment(clientset *kubernetes.Clientset, namespace string, deploymentName string) (*appsv1.Deployment, error) {
	dep, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	if err != nil &&
		strings.Contains(err.Error(), "the object has been modified; please apply your changes to the latest version and try again") {
		time.Sleep(10 * time.Second)
		return clientset.AppsV1().Deployments(namespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
	}
	return dep, nil
}

// GetKubeAPI returns the CoreV1Interface
func GetKubeAPI(useInClusterConfig bool) (v1.CoreV1Interface, error) {

	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return nil, err
	}

	return clientset.CoreV1(), nil
}

// GetClientset returns the kubernetes Clientset
func GetClientset(useInClusterConfig bool) (*kubernetes.Clientset, error) {
	config, err := getKubeConfig(useInClusterConfig)
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(config)
}

// GetDynamicClient returns a dynamic K8s client
func GetDynamicClient(useInClusterConfig bool) (dynamic.Interface, error) {
	config, err := getKubeConfig(useInClusterConfig)
	if err != nil {
		return nil, err
	}

	return dynamic.NewForConfig(config)
}

func getKubeConfig(useInClusterConfig bool) (*rest.Config, error) {
	var config *rest.Config
	var err error
	if useInClusterConfig {
		config, err = rest.InClusterConfig()
	} else {
		var kubeconfig string
		if os.Getenv("KUBECONFIG") != "" {
			kubeconfig = ExpandTilde(os.Getenv("KUBECONFIG"))
		} else {
			kubeconfig = filepath.Join(
				UserHomeDir(), ".kube", "config",
			)
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, err
	}
	return config, nil
}

// CreateNamespace creates a new Kubernetes namespace with the provided name
func CreateNamespace(useInClusterConfig bool, namespace string, namespaceMetadata ...metav1.ObjectMeta) error {

	var buildNamespaceMetadata metav1.ObjectMeta
	if len(namespaceMetadata) > 0 {
		buildNamespaceMetadata = namespaceMetadata[0]
	}

	buildNamespaceMetadata.Name = namespace

	ns := &typesv1.Namespace{ObjectMeta: buildNamespaceMetadata}
	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return err
	}
	_, err = clientset.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
	return err
}

// ExistsNamespace checks whether a namespace with the provided name exists
func ExistsNamespace(useInClusterConfig bool, namespace string) (bool, error) {
	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return false, err
	}
	_, err = clientset.CoreV1().Namespaces().Get(context.TODO(), namespace, metav1.GetOptions{})
	if err != nil {
		if statusErr, ok := err.(*apierr.StatusError); ok && statusErr.ErrStatus.Reason == metav1.StatusReasonNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// GetKeptnManagedNamespace returns the list of namespace with the annotation & label `keptn.sh/managed-by: keptn`
func GetKeptnManagedNamespace(useInClusterConfig bool) ([]string, error) {
	var namespaceList *typesv1.NamespaceList
	var namespaces []string
	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return nil, err
	}
	namespaceList, err = clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{
		LabelSelector: "keptn.sh/managed-by",
	})
	if err != nil {
		return nil, err
	}
	for _, namespace := range namespaceList.Items {
		if metav1.HasAnnotation(namespace.ObjectMeta, "keptn.sh/managed-by") {
			namespaces = append(namespaces, namespace.GetObjectMeta().GetName())
		}
	}
	return namespaces, nil
}

// PatchKeptnManagedNamespace to patch the namespace with the annotation & label `keptn.sh/managed-by: keptn`
func PatchKeptnManagedNamespace(useInClusterConfig bool, namespace string) error {
	var patchData = []byte(`{"metadata": {"annotations": {"keptn.sh/managed-by": "keptn"}, "labels": {"keptn.sh/managed-by": "keptn"}}}`)
	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return err
	}
	_, err = clientset.CoreV1().Namespaces().Patch(context.TODO(), namespace, types.StrategicMergePatchType, patchData,
		metav1.PatchOptions{})
	if err != nil {
		return err
	}
	return nil
}

// GetKeptnEndpointFromIngress returns the host of ingress object Keptn Installation
func GetKeptnEndpointFromIngress(useInClusterConfig bool, namespace string, ingressName string) (string, error) {
	var keptnIngress *v1beta1.Ingress
	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return "", err
	}
	keptnIngress, err = clientset.ExtensionsV1beta1().Ingresses(namespace).Get(context.TODO(), ingressName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return keptnIngress.Spec.Rules[0].Host, nil
}

// GetKeptnEndpointFromService returns the loadbalancer service IP from Keptn Installation
func GetKeptnEndpointFromService(useInClusterConfig bool, namespace string, serviceName string) (string, error) {
	var keptnService *typesv1.Service
	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return "", err
	}
	keptnService, err = clientset.CoreV1().Services(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	switch keptnService.Spec.Type {
	case "LoadBalancer":
		if len(keptnService.Status.LoadBalancer.Ingress) > 0 {
			return keptnService.Status.LoadBalancer.Ingress[0].IP, nil
		}
		return "", fmt.Errorf("Loadbalancer IP isn't found")
	default:
		return "", fmt.Errorf("It doesn't support ClusterIP & NodePort type service for fetching endpoint automatically")
	}
}

// GetKeptnAPITokenFromSecret returns the `keptn-api-token` data secret from Keptn Installation
func GetKeptnAPITokenFromSecret(useInClusterConfig bool, namespace string, secretName string) (string, error) {
	var keptnSecret *typesv1.Secret
	clientset, err := GetClientset(useInClusterConfig)
	if err != nil {
		return "", err
	}
	keptnSecret, err = clientset.CoreV1().Secrets(namespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if apitoken, ok := keptnSecret.Data["keptn-api-token"]; ok {
		return string(apitoken), nil
	}
	return "", fmt.Errorf("data 'keptn-api-token' not found")
}

// ExecuteCommand exectues the command using the args
func ExecuteCommand(command string, args []string) (string, error) {
	cmd := exec.Command(command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("Error executing command %s %s: %s\n%s", command, strings.Join(args, " "), err.Error(), string(out))
	}
	return string(out), nil
}

// ExecuteCommandInDirectory executes the command using the args within the specified directory
func ExecuteCommandInDirectory(command string, args []string, directory string) (string, error) {
	cmd := exec.Command(command, args...)
	cmd.Dir = directory
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("Error executing command %s %s: %s\n%s", command, strings.Join(args, " "), err.Error(), string(out))
	}
	return string(out), nil
}

func getHelmChartURI(chartName string) string {
	return "helm/" + chartName + ".tgz"
}

// StoreChart stores a chart in the configuration service
//Deprecated: StoreChart is deprecated, use chartStorer.Store instead
func StoreChart(project string, service string, stage string, chartName string, helmChart []byte, configServiceURL string) (string, error) {

	cs := chartStorer{
		resourceHandler: utils.NewResourceHandler(configServiceURL),
	}

	opts := StoreChartOptions{
		Project:   project,
		Service:   service,
		Stage:     stage,
		ChartName: chartName,
		HelmChart: helmChart,
	}
	return cs.Store(opts)

}

// GetChart returns the chart and the related git commit ID from the configuration service
func GetChart(project string, service string, stage string, chartName string, configServiceURL string) (*chart.Chart, string, error) {
	resourceHandler := utils.NewResourceHandler(configServiceURL)

	resource, err := resourceHandler.GetServiceResource(project, stage, service, getHelmChartURI(chartName))
	if err != nil {
		return nil, "", fmt.Errorf("Error when reading chart %s from project %s: %s",
			chartName, project, err.Error())
	}
	commitID := resource.Metadata.Version

	ch, err := LoadChart([]byte(resource.ResourceContent))
	if err != nil {
		return nil, "", fmt.Errorf("Error when reading chart %s from project %s: %s",
			chartName, project, err.Error())
	}
	return ch, commitID, nil
}

// LoadChart converts a byte array into a Chart
func LoadChart(data []byte) (*chart.Chart, error) {
	return loader.LoadArchive(bytes.NewReader(data))
}

// LoadChartFromPath loads a directory or Helm chart into a Chart
func LoadChartFromPath(path string) (*chart.Chart, error) {
	return loader.Load(path)
}

// PackageChart packages the chart and returns it
//Deprecated: PackageChart is deprecated, use chartPackager.Package instead
func PackageChart(ch *chart.Chart) ([]byte, error) {
	cp := chartPackager{}
	return cp.Package(ch)
}

// GetRenderedDeployments returns all deployments contained in the provided chart
func GetRenderedDeployments(ch *chart.Chart) ([]*appsv1.Deployment, error) {

	renderedTemplates, err := renderTemplatesWithKeptnValues(ch)
	if err != nil {
		return nil, err
	}

	deployments := make([]*appsv1.Deployment, 0, 0)

	for _, v := range renderedTemplates {
		dec := kyaml.NewYAMLToJSONDecoder(strings.NewReader(v))
		for {
			var dpl appsv1.Deployment
			err := dec.Decode(&dpl)
			if err == io.EOF {
				break
			}
			if err != nil {
				fmt.Println(err)
				continue
			}

			if IsDeployment(&dpl) {
				deployments = append(deployments, &dpl)
			}
		}
	}

	return deployments, nil
}

// GetRenderedServices returns all services contained in the provided chart
func GetRenderedServices(ch *chart.Chart) ([]*typesv1.Service, error) {

	renderedTemplates, err := renderTemplatesWithKeptnValues(ch)
	if err != nil {
		return nil, err
	}

	services := make([]*typesv1.Service, 0, 0)

	for _, v := range renderedTemplates {
		dec := kyaml.NewYAMLToJSONDecoder(strings.NewReader(v))
		for {
			var svc typesv1.Service
			err := dec.Decode(&svc)
			if err == io.EOF {
				break
			}
			if err != nil {
				continue
			}

			if IsService(&svc) {
				services = append(services, &svc)
			}
		}
	}

	return services, nil
}

func renderTemplatesWithKeptnValues(ch *chart.Chart) (map[string]string, error) {
	keptnValues := map[string]interface{}{
		"keptn": map[string]interface{}{
			"project":    "prj",
			"stage":      "stage",
			"service":    "svc",
			"deployment": "dpl",
		},
	}

	cvals, err := chartutil.CoalesceValues(ch, keptnValues)
	if err != nil {
		return nil, err
	}
	options := chartutil.ReleaseOptions{
		Name: "testRelease",
	}
	valuesToRender, err := chartutil.ToRenderValues(ch, cvals, options, nil)

	renderedTemplates, err := engine.Render(ch, valuesToRender)
	if err != nil {
		return nil, err
	}
	return renderedTemplates, nil
}

// IsService tests whether the provided struct is a service
func IsService(svc *typesv1.Service) bool {
	return strings.ToLower(svc.Kind) == "service"
}

// IsDeployment tests whether the provided struct is a deployment
func IsDeployment(dpl *appsv1.Deployment) bool {
	return strings.ToLower(dpl.Kind) == "deployment"
}

//chartStorer  is able to store a helm chart
type chartStorer struct {
	resourceHandler *goutils.ResourceHandler
}

//StoreChartOptions are the parameters for storing a chart
type StoreChartOptions struct {
	Project   string
	Service   string
	Stage     string
	ChartName string
	HelmChart []byte
}

//NewChartStorer creates a new chartStorer instance
func NewChartStorer(resourceHandler *goutils.ResourceHandler) *chartStorer {
	return &chartStorer{
		resourceHandler: resourceHandler,
	}
}

//Store stores a chart in the configuration service
func (cs chartStorer) Store(storeChartOpts StoreChartOptions) (string, error) {

	uri := getHelmChartURI(storeChartOpts.ChartName)
	resource := models.Resource{ResourceURI: &uri, ResourceContent: string(storeChartOpts.HelmChart)}

	version, err := cs.resourceHandler.CreateServiceResources(storeChartOpts.Project, storeChartOpts.Stage, storeChartOpts.Service, []*models.Resource{&resource})
	if err != nil {
		return "", fmt.Errorf("Error when storing chart %s of service %s in project %s: %s",
			storeChartOpts.ChartName, storeChartOpts.Service, storeChartOpts.Project, err.Error())
	}
	return version, nil
}

//chartPackager is able to package a helm chart
type chartPackager struct {
}

//NewChartPackager creates a new chartPackager instance
func NewChartPackager() *chartPackager {
	return &chartPackager{}
}

//packages a helm chart into its byte representation
func (pc chartPackager) Package(ch *chart.Chart) ([]byte, error) {
	helmPackage, err := ioutil.TempDir("", "")
	if err != nil {
		return nil, fmt.Errorf("Error when packaging chart: %s", err.Error())
	}
	defer os.RemoveAll(helmPackage)

	// Marshal values into values.yaml
	// This step is necessary as chartutil.Save uses the Raw content
	for _, f := range ch.Raw {
		if f.Name == chartutil.ValuesfileName {
			f.Data, err = yaml.Marshal(ch.Values)
			if err != nil {
				return nil, err
			}
			break
		}
	}

	name, err := chartutil.Save(ch, helmPackage)
	if err != nil {
		return nil, fmt.Errorf("Error when packaging chart: %s", err.Error())
	}

	data, err := ioutil.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("Error when packaging chart: %s", err.Error())
	}
	return data, nil
}
