package cvo

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/openshift/cluster-version-operator/lib/resourceapply"
	cvv1 "github.com/openshift/cluster-version-operator/pkg/apis/clusterversion.openshift.io/v1"
	osv1 "github.com/openshift/cluster-version-operator/pkg/apis/operatorstatus.openshift.io/v1"
	clientset "github.com/openshift/cluster-version-operator/pkg/generated/clientset/versioned"
	cvinformersv1 "github.com/openshift/cluster-version-operator/pkg/generated/informers/externalversions/clusterversion.openshift.io/v1"
	osinformersv1 "github.com/openshift/cluster-version-operator/pkg/generated/informers/externalversions/operatorstatus.openshift.io/v1"
	cvlistersv1 "github.com/openshift/cluster-version-operator/pkg/generated/listers/clusterversion.openshift.io/v1"
	oslistersv1 "github.com/openshift/cluster-version-operator/pkg/generated/listers/operatorstatus.openshift.io/v1"
	corev1 "k8s.io/api/core/v1"
	apiextclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextinformersv1beta1 "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1beta1"
	apiextlistersv1beta1 "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformersv1 "k8s.io/client-go/informers/apps/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	coreclientsetv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisterv1 "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

const (
	// maxRetries is the number of times a machineconfig pool will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a machineconfig pool is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	// installconfigKey is the key in ConfigMap that stores the InstallConfig.
	installconfigKey = "installconfig"

	workQueueKey = "kube-system/installconfig"
)

// ownerKind contains the schema.GroupVersionKind for type that owns objects managed by CVO.
var ownerKind = cvv1.SchemeGroupVersion.WithKind("CVOConfig")

// Operator defines cluster version operator.
type Operator struct {
	// nodename allows CVO to sync fetchPayload to same node as itself.
	nodename string
	// namespace and name are used to find the CVOConfig, OperatorStatus.
	namespace, name string

	// restConfig is used to create resourcebuilder.
	restConfig *rest.Config

	client        clientset.Interface
	kubeClient    kubernetes.Interface
	apiExtClient  apiextclientset.Interface
	eventRecorder record.EventRecorder

	syncHandler func(key string) error

	cvoConfigLister      cvlistersv1.CVOConfigLister
	operatorStatusLister oslistersv1.OperatorStatusLister

	crdLister          apiextlistersv1beta1.CustomResourceDefinitionLister
	deployLister       appslisterv1.DeploymentLister
	crdListerSynced    cache.InformerSynced
	deployListerSynced cache.InformerSynced

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue workqueue.RateLimitingInterface
}

// New returns a new cluster version operator.
func New(
	nodename string,
	namespace, name string,
	cvoConfigInformer cvinformersv1.CVOConfigInformer,
	operatorStatusInformer osinformersv1.OperatorStatusInformer,
	crdInformer apiextinformersv1beta1.CustomResourceDefinitionInformer,
	deployInformer appsinformersv1.DeploymentInformer,
	restConfig *rest.Config,
	client clientset.Interface,
	kubeClient kubernetes.Interface,
	apiExtClient apiextclientset.Interface,
) *Operator {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&coreclientsetv1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	optr := &Operator{
		nodename:      nodename,
		namespace:     namespace,
		name:          name,
		restConfig:    restConfig,
		client:        client,
		kubeClient:    kubeClient,
		apiExtClient:  apiExtClient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "clusterversionoperator"}),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "clusterversionoperator"),
	}

	cvoConfigInformer.Informer().AddEventHandler(optr.eventHandler())
	crdInformer.Informer().AddEventHandler(optr.eventHandler())

	optr.syncHandler = optr.sync

	optr.cvoConfigLister = cvoConfigInformer.Lister()
	optr.operatorStatusLister = operatorStatusInformer.Lister()

	optr.crdLister = crdInformer.Lister()
	optr.crdListerSynced = crdInformer.Informer().HasSynced
	optr.deployLister = deployInformer.Lister()
	optr.deployListerSynced = deployInformer.Informer().HasSynced

	return optr
}

// Run runs the cluster version operator.
func (optr *Operator) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer optr.queue.ShutDown()

	glog.Info("Starting ClusterVersionOperator")
	defer glog.Info("Shutting down ClusterVersionOperator")

	if !cache.WaitForCacheSync(stopCh,
		optr.crdListerSynced,
		optr.deployListerSynced,
	) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(optr.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (optr *Operator) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { optr.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { optr.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { optr.queue.Add(workQueueKey) },
	}
}

func (optr *Operator) worker() {
	for optr.processNextWorkItem() {
	}
}

func (optr *Operator) processNextWorkItem() bool {
	key, quit := optr.queue.Get()
	if quit {
		return false
	}
	defer optr.queue.Done(key)

	err := optr.syncHandler(key.(string))
	optr.handleErr(err, key)

	return true
}

func (optr *Operator) handleErr(err error, key interface{}) {
	if err == nil {
		optr.queue.Forget(key)
		return
	}

	if optr.queue.NumRequeues(key) < maxRetries {
		glog.V(2).Infof("Error syncing operator %v: %v", key, err)
		optr.queue.AddRateLimited(key)
		return
	}

	err = optr.syncDegradedStatus(err)
	utilruntime.HandleError(err)
	glog.V(2).Infof("Dropping operator %q out of the queue: %v", key, err)
	optr.queue.Forget(key)
}

func (optr *Operator) sync(key string) error {
	startTime := time.Now()
	glog.V(4).Infof("Started syncing operator %q (%v)", key, startTime)
	defer func() {
		glog.V(4).Infof("Finished syncing operator %q (%v)", key, time.Since(startTime))
	}()

	// We always run this to make sure CVOConfig can be synced.
	if err := optr.syncCVOCRDs(); err != nil {
		return err
	}

	config, err := optr.getConfig()
	if err != nil {
		return err
	}

	if err := optr.syncStatus(config, osv1.OperatorStatusCondition{Type: osv1.OperatorStatusConditionTypeWorking, Message: fmt.Sprintf("Working towards %s", config)}); err != nil {
		return err
	}

	payload, err := optr.syncUpdatePayloadContents(updatePayloadsPathPrefix, config)
	if err != nil {
		return err
	}

	if err := optr.syncUpdatePayload(config, payload); err != nil {
		return err
	}

	return optr.syncStatus(config, osv1.OperatorStatusCondition{Type: osv1.OperatorStatusConditionTypeDone, Message: fmt.Sprintf("Done applying %s", config)})
}

func (optr *Operator) getConfig() (*cvv1.CVOConfig, error) {
	// XXX: fetch upstream, channel, cluster ID from InstallConfig
	upstream := cvv1.URL("http://localhost:8080/graph")
	channel := "fast"
	id, _ := uuid.NewRandom()

	// XXX: generate CVOConfig from options calculated above.
	config := &cvv1.CVOConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: optr.namespace,
			Name:      optr.name,
		},
		Upstream:  upstream,
		Channel:   channel,
		ClusterID: id,
	}
	if config.ClusterID.Variant() != uuid.RFC4122 {
		return nil, fmt.Errorf("invalid ClusterID %q, must be an RFC4122-variant UUID: found %s", config.ClusterID, config.ClusterID.Variant())
	}
	if config.ClusterID.Version() != 4 {
		return nil, fmt.Errorf("Invalid ClusterID %q, must be a version-4 UUID: found %s", config.ClusterID, config.ClusterID.Version())
	}

	actual, _, err := resourceapply.ApplyCVOConfigFromCache(optr.cvoConfigLister, optr.client.ClusterversionV1(), config)
	return actual, err
}
