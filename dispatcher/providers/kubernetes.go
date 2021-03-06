package providers

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lawrencegripper/mlops/dispatcher/messaging"

	"github.com/Azure/go-autorest/autorest/to"

	"github.com/lawrencegripper/mlops/dispatcher/types"
	log "github.com/sirupsen/logrus"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	dispatcherNameLabel = "dispatchername"
	messageIDLabel      = "messageid"
	deliverycountlabel  = "deliverycount"
	namespacePrefix     = "mlop-"
)

// Kubernetes schedules jobs onto k8s from the queue and monitors their progress
type Kubernetes struct {
	createJob        func(*batchv1.Job) (*batchv1.Job, error)
	listAllJobs      func() (*batchv1.JobList, error)
	client           *kubernetes.Clientset
	jobConfig        *types.JobConfig
	inflightJobStore map[string]messaging.Message
	dispatcherName   string
	Namespace        string
	sidecarArgs      []string
}

// NewKubernetesProvider Creates an instance and does basic setup
func NewKubernetesProvider(config *types.Configuration, sharedSidecarArgs []string) (*Kubernetes, error) {
	if config == nil {
		return nil, fmt.Errorf("invalid config. Cannot be nil")
	}
	if config.Job == nil {
		return nil, fmt.Errorf("invalid JobConfig. Cannot be nil")
	}

	k := Kubernetes{}
	k.sidecarArgs = sharedSidecarArgs
	client, err := getClientSet()
	if err != nil {
		return nil, err
	}
	k.client = client

	namespace, err := createNamespaceForModule(config.ModuleName, client)
	if err != nil {
		return nil, err
	}
	k.Namespace = namespace
	k.jobConfig = config.Job
	k.dispatcherName = config.Hostname
	k.inflightJobStore = map[string]messaging.Message{}
	k.createJob = func(b *batchv1.Job) (*batchv1.Job, error) {
		return k.client.BatchV1().Jobs(k.Namespace).Create(b)
	}
	k.listAllJobs = func() (*batchv1.JobList, error) {
		return k.client.BatchV1().Jobs(k.Namespace).List(metav1.ListOptions{})
	}
	return &k, nil
}

// Reconcile will review the state of running jobs and accept or reject messages accordingly
func (k *Kubernetes) Reconcile() error {
	if k == nil {
		return fmt.Errorf("invalid properties. Provider cannot be nil")
	}
	// Todo: investigate using the field selector to limit the returned data to only
	// completed or failed jobs
	jobs, err := k.listAllJobs()
	if err != nil {
		return err
	}

	for _, j := range jobs.Items {
		messageID, ok := j.ObjectMeta.Labels[messageIDLabel]
		if !ok {
			log.WithField("job", j).Error("job seen without messageid present in labels... skipping")
			continue
		}

		sourceMessage, ok := k.inflightJobStore[messageID]
		// If we don't have a message in flight for this job check some error cases
		if !ok {
			dipatcherName, ok := j.Labels[dispatcherNameLabel]
			// Is it malformed?
			if !ok {
				log.WithField("job", j).Error("job seen without dispatcher present in labels... skipping")
				continue
			}
			// Is it someone elses?
			if dipatcherName != k.dispatcherName {
				log.WithField("job", j).Debug("job seen with different dispatcher name present in labels... skipping")
				continue
			}
			// Is it ours and we've forgotten
			if dipatcherName != k.dispatcherName {
				log.WithField("job", j).Info("job seen which dispatcher stared but doesn't have source message... likely following a dispatcher restart")
				continue
			}

			log.WithField("job", j).Error("serious reconcile logic error. Malformed job of processing bug. ")
			continue
		}

		// Todo: Handle jobs which have overrun their Max execution time
		// Todo: Should we remove failed/completed jobs?
		for _, condition := range j.Status.Conditions {
			// Job failed - reject the message so it goes back on the queue to be retried
			if condition.Type == batchv1.JobFailed {
				err := sourceMessage.Reject()

				if err != nil {
					log.WithFields(log.Fields{
						"message": sourceMessage,
						"job":     j,
					}).Error("failed to reject message")
					return err
				}
			}

			// Job succeeded - accept the message so it is removed from the queue
			if condition.Type == batchv1.JobComplete {
				err := sourceMessage.Accept()

				if err != nil {
					log.WithFields(log.Fields{
						"message": sourceMessage,
						"job":     j,
					}).Error("failed to accept message")
					return err
				}
			}
		}
	}

	return nil
}

// Dispatch creates a job on kubernetes for the message
func (k *Kubernetes) Dispatch(message messaging.Message) error {
	if message == nil {
		return fmt.Errorf("invalid input. Message cannot be nil")
	}
	if k == nil {
		return fmt.Errorf("invalid properties. Provider cannot be nil")
	}

	perJobArgs, err := getMessageSidecarArgs(message)
	if err != nil {
		return fmt.Errorf("failed generating sidecar args from message: %v", err)
	}
	fullSidecarArgs := append(k.sidecarArgs, perJobArgs...)

	labels := map[string]string{
		dispatcherNameLabel: k.dispatcherName,
		messageIDLabel:      message.ID(),
		deliverycountlabel:  strconv.Itoa(message.DeliveryCount()),
	}

	_, err = k.createJob(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:   getJobName(message),
			Labels: labels,
		},
		Spec: batchv1.JobSpec{
			Completions:  to.Int32Ptr(1),
			BackoffLimit: to.Int32Ptr(1),
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						{
							Name:  "sidecar",
							Image: k.jobConfig.SidecarImage,
							Args:  fullSidecarArgs,
						},
						{
							Name:  "worker",
							Image: k.jobConfig.WorkerImage,
						},
					},
					RestartPolicy: apiv1.RestartPolicyNever,
				},
			},
		},
	})

	if err != nil {
		log.WithError(err).Error("failed scheduling k8s job")
		mErr := message.Reject()
		if mErr != nil {
			log.WithError(mErr).Error("failed rejecting message after failing to schedule k8s job")
		}
		return err
	}

	k.inflightJobStore[message.ID()] = message

	return nil
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func getClientSet() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.WithError(err).Warn("failed getting in-cluster config attempting to use kubeconfig from homedir")
		var kubeconfig string
		if home := homeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}

		if _, err := os.Stat(kubeconfig); os.IsNotExist(err) {
			log.WithError(err).Panic("kubeconfig not found in homedir")
		}

		// use the current context in kubeconfig
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.WithError(err).Panic("getting kubeconf from current context")
			return nil, err
		}
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.WithError(err).Error("Getting clientset from config")
		return nil, err
	}

	return clientset, nil
}

func createNamespaceForModule(moduleName string, client *kubernetes.Clientset) (string, error) {
	// create a namespace for the module
	// Todo: add regex validation to ensure namespace is valid in k8 before submitting
	// a DNS-1123 label must consist of lower case alphanumeric characters or '-', and
	// must start and end with an alphanumeric character (e.g. 'my-name',  or '123-abc', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?'
	namespace := namespacePrefix + strings.ToLower(moduleName)
	_, err := client.CoreV1().Namespaces().Create(&apiv1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	})

	if err != nil && !errors.IsAlreadyExists(err) {
		return "", err
	}

	return namespace, nil
}

func getJobName(m messaging.Message) string {
	return strings.ToLower(m.ID()) + "-a" + strconv.Itoa(m.DeliveryCount())
}
