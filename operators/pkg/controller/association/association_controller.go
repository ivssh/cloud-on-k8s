// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package association

import (
	"reflect"
	"sync/atomic"
	"time"

	assoctype "github.com/elastic/k8s-operators/operators/pkg/apis/associations/v1alpha1"
	estype "github.com/elastic/k8s-operators/operators/pkg/apis/elasticsearch/v1alpha1"
	kbtype "github.com/elastic/k8s-operators/operators/pkg/apis/kibana/v1alpha1"
	"github.com/elastic/k8s-operators/operators/pkg/controller/common"
	"github.com/elastic/k8s-operators/operators/pkg/controller/common/finalizer"
	"github.com/elastic/k8s-operators/operators/pkg/controller/common/operator"
	"github.com/elastic/k8s-operators/operators/pkg/controller/common/watches"
	"github.com/elastic/k8s-operators/operators/pkg/controller/elasticsearch/secret"
	"github.com/elastic/k8s-operators/operators/pkg/controller/elasticsearch/services"
	"github.com/elastic/k8s-operators/operators/pkg/utils/k8s"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	log            = logf.Log.WithName("association-controller")
	defaultRequeue = reconcile.Result{Requeue: true, RequeueAfter: 10 * time.Second}
)

// Add creates a new Assocation Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, _ operator.Parameters) error {
	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	c, err := add(mgr, r)
	if err != nil {
		return err
	}
	return addWatches(c, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (*ReconcileAssociation, error) {
	client := k8s.WrapClient(mgr.GetClient())
	return &ReconcileAssociation{
		Client:   client,
		scheme:   mgr.GetScheme(),
		watches:  watches.NewDynamicWatches(),
		recorder: mgr.GetRecorder("association-controller"),
	}, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) (controller.Controller, error) {
	// Create a new controller
	c, err := controller.New("association-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return c, err
	}
	return c, nil
}

func addWatches(c controller.Controller, r *ReconcileAssociation) error {
	// Watch for changes to the association
	if err := c.Watch(&source.Kind{Type: &assoctype.KibanaElasticsearchAssociation{}}, &handler.EnqueueRequestForObject{}); err != nil {
		return err
	}

	// Watch Elasticsearch cluster objects
	if err := c.Watch(&source.Kind{Type: &estype.ElasticsearchCluster{}}, r.watches.ElasticsearchClusters); err != nil {
		return err
	}

	// Watch Kibana objects
	if err := c.Watch(&source.Kind{Type: &kbtype.Kibana{}}, r.watches.Kibanas); err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileAssociation{}

// ReconcileAssociation reconciles a Kibana-Elasticsearch association object
type ReconcileAssociation struct {
	k8s.Client
	scheme   *runtime.Scheme
	recorder record.EventRecorder
	watches  watches.DynamicWatches

	// iteration is the number of times this controller has run its Reconcile method
	iteration int64
}

// Reconcile reads that state of the cluster for an Association object and makes changes based on the state read and what is in
// the Association.Spec
func (r *ReconcileAssociation) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// atomically update the iteration to support concurrent runs.
	currentIteration := atomic.AddInt64(&r.iteration, 1)
	iterationStartTime := time.Now()
	log.Info("Start reconcile iteration", "iteration", currentIteration)
	defer func() {
		log.Info("End reconcile iteration", "iteration", currentIteration, "took", time.Since(iterationStartTime))
	}()

	var association assoctype.KibanaElasticsearchAssociation
	err := r.Get(request.NamespacedName, &association)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if common.IsPaused(association.ObjectMeta) {
		log.Info("Paused : skipping reconciliation", "iteration", currentIteration)
		return common.PauseRequeue, nil
	}

	handler := finalizer.NewHandler(r)
	err = handler.Handle(&association, watchFinalizer(k8s.ExtractNamespacedName(&association), r.watches))
	if err != nil {
		// failed to prepare finalizer or run finalizer: retry
		return defaultRequeue, err
	}

	// Association is being deleted short-circuit reconciliation
	if !association.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	newStatus, err := r.reconcileInternal(association)
	// maybe update status
	origStatus := association.Status.DeepCopy()
	association.Status.AssociationStatus = newStatus

	if !reflect.DeepEqual(*origStatus, association.Status) {
		if err := r.Status().Update(&association); err != nil {
			return defaultRequeue, err
		}
	}
	return resultFromStatus(newStatus), err

}

func elasticsearchWatchName(assocKey types.NamespacedName) string {
	return assocKey.Namespace + "-" + assocKey.Name + "-es-watch"
}

func kibanaWatchName(assocKey types.NamespacedName) string {
	return assocKey.Namespace + "-" + assocKey.Name + "-kb-watch"
}

// watchFinalizer ensure that we remove watches for Kibanas and Elasticsearch clusters that we are no longer interested in
// because the assocation has been deleted.
func watchFinalizer(assocKey types.NamespacedName, w watches.DynamicWatches) finalizer.Finalizer {
	return finalizer.Finalizer{
		Name: "dynamic-watches",
		Execute: func() error {
			w.Kibanas.RemoveHandlerForKey(kibanaWatchName(assocKey))
			w.ElasticsearchClusters.RemoveHandlerForKey(elasticsearchWatchName(assocKey))
			return nil
		},
	}
}

func resultFromStatus(status assoctype.AssociationStatus) reconcile.Result {
	switch status {
	case assoctype.AssociationPending:
		return defaultRequeue // retry again
	case assoctype.AssociationEstablished, assoctype.AssociationFailed:
		return reconcile.Result{} // we are done or there is not much we can do
	default:
		return reconcile.Result{} // make the compiler happy
	}
}

func (r *ReconcileAssociation) reconcileInternal(association assoctype.KibanaElasticsearchAssociation) (assoctype.AssociationStatus, error) {
	assocKey := k8s.ExtractNamespacedName(&association)

	// Make sure we see events from Kibana+Elasticsearch using a dynamic watch
	// will become more relevant once we refactor user handling to CRDs and implement
	// syncing of user credentials across namespaces
	err := r.watches.ElasticsearchClusters.AddHandler(watches.NamedWatch{
		Name:    elasticsearchWatchName(assocKey),
		Watched: association.Spec.Elasticsearch.NamespacedName(),
		Watcher: assocKey,
	})
	if err != nil {
		return assoctype.AssociationFailed, err
	}
	err = r.watches.Kibanas.AddHandler(watches.NamedWatch{
		Name:    kibanaWatchName(assocKey),
		Watched: association.Spec.Kibana.NamespacedName(),
		Watcher: assocKey,
	})
	if err != nil {
		return assoctype.AssociationFailed, err
	}

	var es estype.ElasticsearchCluster
	err = r.Get(association.Spec.Elasticsearch.NamespacedName(), &es)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Es not found, could be deleted or not yet created? Recheck in a while
			return assoctype.AssociationPending, nil
		}
		return assoctype.AssociationFailed, err
	}

	// TODO reconcile external user CRD here

	var expectedEsConfig kbtype.BackendElasticsearch

	internalUsersSecretName := secret.ElasticInternalUsersSecretName(es.Name)
	var internalUsersSecret corev1.Secret
	internalUsersSecretKey := types.NamespacedName{Namespace: es.Namespace, Name: internalUsersSecretName}
	if err := r.Get(internalUsersSecretKey, &internalUsersSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return assoctype.AssociationPending, err
		}
		return assoctype.AssociationFailed, err
	}

	// TODO: can deliver through a shared secret instead?
	expectedEsConfig.Auth.Inline = &kbtype.ElasticsearchInlineAuth{
		Username: secret.InternalKibanaServerUserName,
		// TODO: error checking
		Password: string(internalUsersSecret.Data[secret.InternalKibanaServerUserName]),
	}

	var publicCACertSecret corev1.Secret
	publicCACertSecretKey := types.NamespacedName{Namespace: es.Namespace, Name: es.Name}
	if err = r.Get(publicCACertSecretKey, &publicCACertSecret); err != nil {
		return assoctype.AssociationPending, err // maybe not created yet
	}
	// TODO this is currently limiting the association to the same namespace
	expectedEsConfig.CaCertSecret = &publicCACertSecret.Name
	expectedEsConfig.URL = services.ExternalServiceURL(es)

	var currentKb kbtype.Kibana
	err = r.Get(association.Spec.Kibana.NamespacedName(), &currentKb)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return assoctype.AssociationPending, err
		}
		return assoctype.AssociationFailed, err
	}

	// TODO: this is a bit rough
	if !reflect.DeepEqual(currentKb.Spec.Elasticsearch, expectedEsConfig) {
		currentKb.Spec.Elasticsearch = expectedEsConfig
		log.Info("Updating Kibana spec with Elasticsearch backend configuration")
		if err := r.Update(&currentKb); err != nil {
			return assoctype.AssociationPending, err
		}
	}
	return assoctype.AssociationEstablished, nil
}