package plugins

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openshift/cluster-policy-controller/pkg/psalabelsyncer/nsexemptions"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/clientset/versioned"
	listerv1alpha1 "github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/listers/operators/v1alpha1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/kubestate"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/operatorclient"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/lib/queueinformer"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const NamespaceLabelSyncerLabelKey = "security.openshift.io/scc.podSecurityLabelSync"
const openshiftPrefix = "openshift-"

const noCopiedCsvSelector = "!" + v1alpha1.CopiedLabelKey

// csvNamespaceLabelerPlugin is responsible for labeling non-payload openshift-* namespaces
// with the label "security.openshift.io/scc.podSecurityLabelSync=true" so that the  PSA Label Syncer
// see https://github.com/openshift/cluster-policy-controller/blob/master/pkg/psalabelsyncer/podsecurity_label_sync_controller.go
// can help ensure that the operator payloads in the namespace continue to work even if they don't yet respect the
// upstream Pod Security Admission controller, which will become active in k8s 1.25.
// see https://kubernetes.io/docs/concepts/security/pod-security-admission/
// If a CSV is created or modified, this controller will look at the csv's namespace. If it is a non-payload namespace,
// if the namespace name is prefixed with 'openshift-', and if the namespace does not contain the label (whatever
// value it may be set to), it will add the "security.openshift.io/scc.podSecurityLabelSync=true" to the namespace.
type csvNamespaceLabelerPlugin struct {
	namespaceLister       listerv1.NamespaceLister
	nonCopiedCsvListerMap map[string]listerv1alpha1.ClusterServiceVersionLister
	kubeClient            operatorclient.ClientInterface
	externalClient        versioned.Interface
	logger                *logrus.Logger
}

func NewCsvNamespaceLabelerPluginFunc(ctx context.Context, config OperatorConfig, hostOperator HostOperator) (OperatorPlugin, error) {

	if hostOperator == nil {
		return nil, fmt.Errorf("cannot initialize plugin: operator undefined")
	}

	plugin := &csvNamespaceLabelerPlugin{
		kubeClient:            config.OperatorClient(),
		externalClient:        config.ExternalClient(),
		logger:                config.Logger(),
		namespaceLister:       nil,
		nonCopiedCsvListerMap: map[string]listerv1alpha1.ClusterServiceVersionLister{},
	}

	plugin.log("setting up csv namespace plug-in for namespaces: %s", config.WatchedNamespaces())

	namespaceInformer := newNamespaceInformer(config.OperatorClient(), config.ResyncPeriod()())

	plugin.log("registering namespace informer")

	plugin.namespaceLister = listerv1.NewNamespaceLister(namespaceInformer.GetIndexer())

	namespaceQueue := workqueue.NewNamedRateLimitingQueue(
		workqueue.DefaultControllerRateLimiter(),
		"csv-ns-labeler-plugin-ns-queue",
	)
	namespaceQueueInformer, err := queueinformer.NewQueueInformer(
		ctx,
		queueinformer.WithInformer(namespaceInformer),
		queueinformer.WithLogger(config.Logger()),
		queueinformer.WithQueue(namespaceQueue),
		queueinformer.WithIndexer(namespaceInformer.GetIndexer()),
		queueinformer.WithSyncer(plugin),
	)
	if err != nil {
		return nil, err
	}
	if err := hostOperator.RegisterQueueInformer(namespaceQueueInformer); err != nil {
		return nil, err
	}

	for _, namespace := range config.WatchedNamespaces() {
		plugin.log("setting up namespace: %s", namespace)
		// ignore namespaces that are *NOT* prefixed with openshift- but accept metav1.NamespaceAll
		if !(hasOpenshiftPrefix(namespace)) && namespace != metav1.NamespaceAll {
			continue
		}

		nonCopiedCsvInformer := newNonCopiedCsvInformerForNamespace(namespace, config.ExternalClient(), config.ResyncPeriod()())

		nonCopiedCsvQueue := workqueue.NewNamedRateLimitingQueue(
			workqueue.DefaultControllerRateLimiter(),
			fmt.Sprintf("%s/csv-ns-labeler-plugin-csv-queue", namespace),
		)
		nonCopiedCsvQueueInformer, err := queueinformer.NewQueueInformer(
			ctx,
			queueinformer.WithInformer(nonCopiedCsvInformer),
			queueinformer.WithLogger(config.Logger()),
			queueinformer.WithQueue(nonCopiedCsvQueue),
			queueinformer.WithIndexer(nonCopiedCsvInformer.GetIndexer()),
			queueinformer.WithSyncer(plugin),
		)
		if err != nil {
			return nil, err
		}
		if err := hostOperator.RegisterQueueInformer(nonCopiedCsvQueueInformer); err != nil {
			return nil, err
		}
		plugin.nonCopiedCsvListerMap[namespace] = listerv1alpha1.NewClusterServiceVersionLister(nonCopiedCsvInformer.GetIndexer())
		plugin.log("registered csv queue informer for: %s", namespace)
	}
	plugin.log("finished setting up csv namespace labeler plugin")

	return plugin, nil
}

func (p *csvNamespaceLabelerPlugin) Shutdown() error {
	return nil
}

func (p *csvNamespaceLabelerPlugin) Sync(ctx context.Context, event kubestate.ResourceEvent) error {
	// only act on csv added and updated events
	if event.Type() != kubestate.ResourceAdded && event.Type() != kubestate.ResourceUpdated {
		return nil
	}

	var namespace *v1.Namespace
	var err error

	// get namespace from the event resource
	switch eventResource := event.Resource().(type) {

	// handle csv events
	case *v1alpha1.ClusterServiceVersion:
		// ignore copied csvs and namespaces that should be ignored
		if eventResource.IsCopied() || ignoreNamespace(eventResource.GetNamespace()) {
			return nil
		}

		namespace, err = p.getNamespace(eventResource.GetNamespace())
		if err != nil {
			return fmt.Errorf("error getting csv namespace (%s) for label sync'er labeling", eventResource.GetNamespace())
		}

	// handle namespace events
	case *v1.Namespace:
		// ignore namespaces that should be ignored and ones that are already labeled
		if ignoreNamespace(eventResource.GetName()) || hasLabelSyncerLabel(eventResource) {
			return nil
		}

		// get csv count for namespace
		csvCount, err := p.countClusterServiceVersions(eventResource.GetName())
		if err != nil {
			return fmt.Errorf("error counting csvs in namespace=%s: %s", eventResource.GetName(), err)
		}

		// ignore namespaces with no csvs
		if csvCount <= 0 {
			return nil
		}

		namespace = eventResource
	default:
		return fmt.Errorf("event resource is neither a ClusterServiceVersion or a Namespace")
	}

	// add label sync'er label if it does not exist
	if !(hasLabelSyncerLabel(namespace)) {
		if err := applyLabelSyncerLabel(ctx, p.kubeClient, namespace); err != nil {
			return fmt.Errorf("error updating csv namespace (%s) with label sync'er label", namespace.GetNamespace())
		}
		p.log("applied %s=true label to namespace %s", NamespaceLabelSyncerLabelKey, namespace.GetNamespace())
	}

	return nil
}

func (p *csvNamespaceLabelerPlugin) getNamespace(namespace string) (*v1.Namespace, error) {
	ns, err := p.namespaceLister.Get(namespace)
	if err != nil {
		return nil, err
	}
	return ns, nil
}

func (p *csvNamespaceLabelerPlugin) countClusterServiceVersions(namespace string) (int, error) {
	lister, ok := p.nonCopiedCsvListerMap[namespace]
	if !ok {
		lister, ok = p.nonCopiedCsvListerMap[metav1.NamespaceAll]
		if !ok {
			return 0, fmt.Errorf("no csv indexer found for namespace: %s", namespace)
		}
	}
	labelSelector, err := labels.Parse(noCopiedCsvSelector)
	if err != nil {
		return 0, err
	}

	csvList, err := lister.ClusterServiceVersions(namespace).List(labelSelector)
	if err != nil {
		return 0, err
	}
	return len(csvList), nil
}

func (p *csvNamespaceLabelerPlugin) log(format string, args ...interface{}) {
	if p.logger != nil {
		p.logger.Infof("[CSV NS Plug-in] "+format, args...)
	}
}

// newNamespaceInformer creates a namespace informer that filters namespaces the plug-in is not interested in:
// payload namespaces (except openshift-operators) and non openshift- prefixed namespaces
// the informer also prunes the namespace objects to only keep basic type and object metadata and annotations
func newNamespaceInformer(k8sClient operatorclient.ClientInterface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	// create a namespace informer
	pruneNamespace := func(namespace *v1.Namespace) {
		namespace = &v1.Namespace{
			TypeMeta: namespace.TypeMeta,
			ObjectMeta: metav1.ObjectMeta{
				Name:        namespace.GetName(),
				Namespace:   namespace.GetNamespace(),
				Annotations: namespace.GetAnnotations(),
			},
		}
	}

	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				list, err := k8sClient.KubernetesInterface().CoreV1().Namespaces().List(context.Background(), options)
				if err != nil {
					return list, err
				}

				// filter and prune namespaces
				var filteredList []v1.Namespace
				for i := range list.Items {
					ns := list.Items[i]
					if !(ignoreNamespace(ns.GetName())) {
						pruneNamespace(&ns)
						filteredList = append(filteredList, ns)
					}
				}
				return &v1.NamespaceList{
					Items: filteredList,
				}, nil
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				nsWatch, err := k8sClient.KubernetesInterface().CoreV1().Namespaces().Watch(context.Background(), options)
				if err != nil {
					return nsWatch, err
				}
				return watch.Filter(nsWatch, func(e watch.Event) (watch.Event, bool) {
					if ns, ok := e.Object.(*v1.Namespace); ok {
						if !(ignoreNamespace(ns.GetName())) {
							pruneNamespace(ns)
							return e, true
						}
					}
					return e, false
				}), nil
			},
		},
		&v1.Namespace{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
}

// newNonCopiedCsvInformerForNamespace creates a csv-based informer that filters out copied csvs and csv events coming
// from namespaces the plug-in is not interested in: payload namespaces (except openshift-operators) and
// non openshift- prefixed namespaces
// the informer also prunes the csvs to only keep basic type and object metadata and annotations
func newNonCopiedCsvInformerForNamespace(namespace string, externalClient versioned.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	// create a new csv informer and prune status to reduce memory footprint
	pruneCSV := func(csv *v1alpha1.ClusterServiceVersion) {
		csv = &v1alpha1.ClusterServiceVersion{
			TypeMeta: csv.TypeMeta,
			ObjectMeta: metav1.ObjectMeta{
				Name:        csv.GetName(),
				Namespace:   csv.GetNamespace(),
				Annotations: csv.GetAnnotations(),
			},
		}
	}

	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.LabelSelector = noCopiedCsvSelector
				list, err := externalClient.OperatorsV1alpha1().ClusterServiceVersions(namespace).List(context.Background(), options)
				if err != nil {
					return list, err
				}

				// filter and prune csvs
				var filteredList []v1alpha1.ClusterServiceVersion
				for i := range list.Items {
					csv := list.Items[i]
					if !(ignoreNamespace(csv.GetNamespace())) {
						pruneCSV(&csv)
						filteredList = append(filteredList, csv)
					}
				}
				return &v1alpha1.ClusterServiceVersionList{
					Items: filteredList,
				}, nil
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = noCopiedCsvSelector
				csvWatch, err := externalClient.OperatorsV1alpha1().ClusterServiceVersions(namespace).Watch(context.Background(), options)
				if err != nil {
					return csvWatch, err
				}
				return watch.Filter(csvWatch, func(e watch.Event) (watch.Event, bool) {
					if csv, ok := e.Object.(*v1alpha1.ClusterServiceVersion); ok {
						if !(ignoreNamespace(csv.GetNamespace())) && !csv.IsCopied() {
							pruneCSV(csv)
							return e, true
						}
					}
					return e, false
				}), nil
			},
		},
		&v1alpha1.ClusterServiceVersion{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
}

func hasOpenshiftPrefix(namespaceName string) bool {
	return strings.HasPrefix(namespaceName, openshiftPrefix)
}

func ignoreNamespace(namespace string) bool {
	// ignore non-openshift-* and payload openshift-* namespaces
	return !hasOpenshiftPrefix(namespace) || nsexemptions.IsNamespacePSALabelSyncExemptedInVendoredOCPVersion(namespace)
}

func applyLabelSyncerLabel(ctx context.Context, kubeClient operatorclient.ClientInterface, namespace *v1.Namespace) error {
	if _, ok := namespace.GetLabels()[NamespaceLabelSyncerLabelKey]; !ok {
		nsCopy := namespace.DeepCopy()
		if nsCopy.GetLabels() == nil {
			nsCopy.SetLabels(map[string]string{})
		}
		nsCopy.GetLabels()[NamespaceLabelSyncerLabelKey] = "true"
		if _, err := kubeClient.KubernetesInterface().CoreV1().Namespaces().Update(ctx, nsCopy, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func hasLabelSyncerLabel(namespace *v1.Namespace) bool {
	_, ok := namespace.GetLabels()[NamespaceLabelSyncerLabelKey]
	return ok
}
