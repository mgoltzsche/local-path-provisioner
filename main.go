package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"
	"github.com/urfave/cli"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	pvController "sigs.k8s.io/sig-storage-lib-external-provisioner/controller"
)

var (
	VERSION                   = "0.0.1"
	FlagConfigFile            = "config"
	FlagProvisionerName       = "provisioner-name"
	EnvProvisionerName        = "PROVISIONER_NAME"
	DefaultProvisionerName    = "rancher.io/local-path"
	FlagNamespace             = "namespace"
	EnvNamespace              = "POD_NAMESPACE"
	DefaultNamespace          = "local-path-storage"
	FlagHelperImage           = "helper-image"
	EnvHelperImage            = "HELPER_IMAGE"
	DefaultHelperImage        = "rancher/library-busybox:1.31.1"
	FlagServiceAccountName    = "service-account-name"
	DefaultServiceAccount     = "local-path-provisioner-service-account"
	EnvServiceAccountName     = "SERVICE_ACCOUNT_NAME"
	FlagKubeconfig            = "kubeconfig"
	DefaultConfigFileKey      = "config.json"
	DefaultConfigMapName      = "local-path-config"
	FlagConfigMapName         = "configmap-name"
	EnvConfigMapName          = "CONFIGMAP_NAME"
	FlagHelperPodFile         = "helper-pod-file"
	DefaultHelperPodFile      = "helper-pod.yaml"
	EnvHelperPodFile          = "HELPER_POD_FILE"
	FlagHelperPodTimeout      = "helper-pod-timeout"
	EnvHelperPodTimeout       = "HELPER_POD_TIMEOUT"
	FlagPVCAnnotation         = "pvc-annotation"
	EnvPVCAnnotation          = "PVC_ANNOTATION"
	FlagPVCAnnotationRequired = "pvc-annotation-required"
	EnvPVCAnnotationRequired  = "PVC_ANNOTATION_REQUIRED"
)

func cmdNotFound(c *cli.Context, command string) {
	panic(fmt.Errorf("Unrecognized command: %s", command))
}

func onUsageError(c *cli.Context, err error, isSubcommand bool) error {
	panic(fmt.Errorf("Usage error, please check your command"))
}

func RegisterShutdownChannel(done chan struct{}) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		logrus.Infof("Received %v signal - terminating", sig)
		close(done)
		<-sigs
		logrus.Info("Received 2nd termination signal - exiting forcefully")
		os.Exit(254)
	}()
}

func StartCmd() cli.Command {
	return cli.Command{
		Name: "start",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  FlagConfigFile,
				Usage: "Required. Provisioner configuration file.",
				Value: "",
			},
			cli.StringFlag{
				Name:   FlagProvisionerName,
				Usage:  "Required. Specify Provisioner name.",
				EnvVar: EnvProvisionerName,
				Value:  DefaultProvisionerName,
			},
			cli.StringFlag{
				Name:   FlagNamespace,
				Usage:  "Required. The namespace that Provisioner is running in",
				EnvVar: EnvNamespace,
				Value:  DefaultNamespace,
			},
			cli.StringFlag{
				Name:  FlagKubeconfig,
				Usage: "Paths to a kubeconfig. Only required when it is out-of-cluster.",
				Value: "",
			},
			cli.StringFlag{
				Name:   FlagConfigMapName,
				Usage:  "Required. Specify configmap name.",
				EnvVar: EnvConfigMapName,
				Value:  DefaultConfigMapName,
			},
			cli.StringFlag{
				Name:   FlagServiceAccountName,
				Usage:  "Required. The ServiceAccountName for deployment",
				EnvVar: EnvServiceAccountName,
				Value:  DefaultServiceAccount,
			},
			cli.StringFlag{
				Name:   FlagHelperPodFile,
				Usage:  "Paths to the Helper pod yaml file",
				EnvVar: EnvHelperPodFile,
				Value:  "",
			},
			cli.StringFlag{
				Name:   FlagHelperPodTimeout,
				Usage:  "Helper pod command execution timeout",
				EnvVar: EnvHelperPodTimeout,
				Value:  "2m",
			},
			cli.StringFlag{
				Name:   FlagPVCAnnotation,
				Usage:  "A PersistentVolumeClaim annotation or prefix passed through to the helper pod as env vars (PVC_ANNOTATION[_<ANNOTATION_PATH_SUFFIX>]=<ANNOTATION_VALUE>)",
				EnvVar: EnvPVCAnnotation,
				Value:  "",
			},
			cli.StringFlag{
				Name:   FlagPVCAnnotationRequired,
				Usage:  "Annotation that must be specified on PersistentVolumeClaims (multiple comma-separated)",
				EnvVar: EnvPVCAnnotationRequired,
				Value:  "",
			},
		},
		Action: func(c *cli.Context) {
			if err := startDaemon(c); err != nil {
				logrus.Fatalf("Error starting daemon: %v", err)
			}
		},
	}
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func loadConfig(kubeconfig string) (*rest.Config, error) {
	if len(kubeconfig) > 0 {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	kubeconfigPath := os.Getenv(clientcmd.RecommendedConfigPathEnvVar)
	if len(kubeconfigPath) > 0 {
		envVarFiles := filepath.SplitList(kubeconfigPath)
		for _, f := range envVarFiles {
			if _, err := os.Stat(f); err == nil {
				return clientcmd.BuildConfigFromFlags("", f)
			}
		}
	}

	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}

	kubeconfig = filepath.Join(homeDir(), clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func findConfigFileFromConfigMap(kubeClient clientset.Interface, namespace, configMapName, key string) (string, error) {
	cm, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(configMapName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	value, ok := cm.Data[key]
	if !ok {
		return "", fmt.Errorf("%v is not exist in local-path-config ConfigMap", key)
	}
	return value, nil
}

func startDaemon(c *cli.Context) error {
	stopCh := make(chan struct{})
	RegisterShutdownChannel(stopCh)

	config, err := loadConfig(c.String(FlagKubeconfig))
	if err != nil {
		return errors.Wrap(err, "unable to get client config")
	}

	kubeClient, err := clientset.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "unable to get k8s client")
	}

	serverVersion, err := kubeClient.Discovery().ServerVersion()
	if err != nil {
		return errors.Wrap(err, "Cannot start Provisioner: failed to get Kubernetes server version")
	}

	provisionerName := c.String(FlagProvisionerName)
	if provisionerName == "" {
		return fmt.Errorf("invalid empty flag %v", FlagProvisionerName)
	}
	namespace := c.String(FlagNamespace)
	if namespace == "" {
		return fmt.Errorf("invalid empty flag %v", FlagNamespace)
	}
	configMapName := c.String(FlagConfigMapName)
	if configMapName == "" {
		return fmt.Errorf("invalid empty flag %v", FlagConfigMapName)
	}
	configFile := c.String(FlagConfigFile)
	if configFile == "" {
		configFile, err = findConfigFileFromConfigMap(kubeClient, namespace, configMapName, DefaultConfigFileKey)
		if err != nil {
			return fmt.Errorf("invalid empty flag %v and it also does not exist at ConfigMap %v/%v with err: %v", FlagConfigFile, namespace, configMapName, err)
		}
	}
	serviceAccountName := c.String(FlagServiceAccountName)
	if serviceAccountName == "" {
		return fmt.Errorf("invalid empty flag %v", FlagServiceAccountName)
	}

	pvcAnnotation := c.String(FlagPVCAnnotation)
	pvcAnnotationsRequired := c.String(FlagPVCAnnotationRequired)
	var requiredPVCAnnotations []string
	if pvcAnnotationsRequired != "" {
		requiredPVCAnnotations = strings.Split(pvcAnnotationsRequired, ",")
	}

	// if helper pod file is not specified, then find the helper pod by configmap with key = helper-pod.yaml
	// if helper pod file is specified with flag FlagHelperPodFile, then load the file
	helperPodFile := c.String(FlagHelperPodFile)
	helperPodYaml := ""
	if helperPodFile == "" {
		helperPodYaml, err = findConfigFileFromConfigMap(kubeClient, namespace, configMapName, DefaultHelperPodFile)
		if err != nil {
			return fmt.Errorf("invalid empty flag %v and it also does not exist at ConfigMap %v/%v with err: %v", FlagHelperPodFile, namespace, configMapName, err)
		}
	} else {
		helperPodYaml, err = loadFile(helperPodFile)
		if err != nil {
			return fmt.Errorf("could not open file %v with err: %v", helperPodFile, err)
		}
	}
	helperPodTimeoutStr := c.String(FlagHelperPodTimeout)
	helperPodTimeout, err := time.ParseDuration(helperPodTimeoutStr)
	if err != nil {
		return fmt.Errorf("invalid %s option provided: %s", FlagHelperPodTimeout, err)
	}

	provisioner, err := NewProvisioner(stopCh, kubeClient, configFile, namespace, configMapName, serviceAccountName, helperPodYaml, helperPodTimeout, pvcAnnotation, requiredPVCAnnotations)
	if err != nil {
		return err
	}
	pc := pvController.NewProvisionController(
		kubeClient,
		provisionerName,
		provisioner,
		serverVersion.GitVersion,
		pvController.LeaderElection(false),
	)
	logrus.Debug("Provisioner started")
	pc.Run(stopCh)
	logrus.Debug("Provisioner stopped")
	return nil
}

func main() {
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	a := cli.NewApp()
	a.Version = VERSION
	a.Usage = "Local Path Provisioner"

	a.Before = func(c *cli.Context) error {
		if c.GlobalBool("debug") {
			logrus.SetLevel(logrus.DebugLevel)
		}
		return nil
	}

	a.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:   "debug, d",
			Usage:  "enable debug logging level",
			EnvVar: "RANCHER_DEBUG",
		},
	}
	a.Commands = []cli.Command{
		StartCmd(),
	}
	a.CommandNotFound = cmdNotFound
	a.OnUsageError = onUsageError

	if err := a.Run(os.Args); err != nil {
		logrus.Fatalf("Critical error: %v", err)
	}
}
