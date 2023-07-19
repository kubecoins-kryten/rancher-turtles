package controllers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/rancher-sandbox/rancher-turtles/internal/rancher"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util/predicates"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	yamlDecoder "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	importAnnotationName         = "rancher-auto-import"
	defaultRequeueDuration       = 1 * time.Minute
	clusterRegistrationTokenName = "default-token"
)

type CAPIImportReconciler struct {
	client.Client
	recorder         record.EventRecorder
	WatchFilterValue string
	Scheme           *runtime.Scheme

	controller         controller.Controller
	externalTracker    external.ObjectTracker
	remoteClientGetter remote.ClusterClientGetter
}

func (r *CAPIImportReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	log := log.FromContext(ctx)

	if r.remoteClientGetter == nil {
		r.remoteClientGetter = remote.NewClusterClient
	}

	//TODO: we want the control plane initialized but removing for the time being
	//capiPredicates := predicates.All(log, predicates.ClusterControlPlaneInitialized(log), predicates.ResourceHasFilterLabel(log, r.WatchFilterValue))
	capiPredicates := predicates.All(log, predicates.ResourceHasFilterLabel(log, r.WatchFilterValue))

	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&clusterv1.Cluster{}).
		WithOptions(options).
		WithEventFilter(capiPredicates).
		Build(r)
	if err != nil {
		return fmt.Errorf("creating new controller: %w", err)
	}

	// Watch Rancher provisioningv2 clusters
	// NOTE: we will import the types from rancher in the future
	gvk := schema.GroupVersionKind{Group: "provisioning.cattle.io", Version: "v1", Kind: "Cluster"}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)

	err = c.Watch(
		&source.Kind{Type: u},
		handler.EnqueueRequestsFromMapFunc(r.rancherClusterToCapiCluster(ctx)),
		//&handler.EnqueueRequestForOwner{OwnerType: &clusterv1.Cluster{}},
	)
	if err != nil {
		return fmt.Errorf("adding watch for Rancher cluster: %w", err)
	}

	ns := &corev1.Namespace{}
	err = c.Watch(
		&source.Kind{Type: ns},
		handler.EnqueueRequestsFromMapFunc(r.namespaceToCapiClusters(ctx)),
	)
	if err != nil {
		return fmt.Errorf("adding watch for namespaces: %w", err)
	}

	r.recorder = mgr.GetEventRecorderFor("rancher-turtles")
	r.controller = c
	r.externalTracker = external.ObjectTracker{
		Controller: c,
	}

	return nil
}

// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=provisioning.cattle.io,resources=clusters;clusters/status,verbs=get;list;watch;create;update;delete;patch
func (r *CAPIImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, reterr error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling CAPI cluster")

	capiCluster := &clusterv1.Cluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, capiCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{Requeue: true}, err
	}
	log = log.WithValues("cluster", capiCluster.Name)

	// Wait for controlplane to be ready
	if !capiCluster.Status.ControlPlaneReady {
		log.Info("clusters control plane is not ready, requeue")
		return ctrl.Result{RequeueAfter: defaultRequeueDuration}, nil
	}

	// fetch the rancher clusters
	rancherClusterHandler := rancher.NewClusterHandler(ctx, r.Client)
	rancherClusterName := rancherClusterNameFromCAPICluster(capiCluster.Name)
	rancherCluster, err := rancherClusterHandler.Get(client.ObjectKey{Namespace: capiCluster.Namespace, Name: rancherClusterName})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{Requeue: true}, err
		}
	}

	if !capiCluster.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, capiCluster)
	}

	return r.reconcileNormal(ctx, capiCluster, rancherClusterHandler, rancherCluster)
}

func (r *CAPIImportReconciler) reconcileNormal(ctx context.Context, capiCluster *clusterv1.Cluster, rancherClusterHandler *rancher.ClusterHandler, rancherCluster *rancher.Cluster) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling CAPI Cluster")

	if rancherCluster == nil {
		shouldImport, err := r.shouldAutoImport(ctx, capiCluster)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !shouldImport {
			log.Info("not auto importing cluster as namespace or cluster isn't marked auto import", "clustername", capiCluster.Name, "namespace", capiCluster.Namespace)
			return ctrl.Result{}, nil
		}

		if err := rancherClusterHandler.Create(&rancher.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rancherClusterNameFromCAPICluster(capiCluster.Name),
				Namespace: capiCluster.Namespace,
			},
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("error creating rancher cluster: %w", err)
		}

		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("found cluster name", "name", rancherCluster.Status.ClusterName)

	if rancherCluster.Status.AgentDeployed {
		log.Info("agent already deployed, no action needed")
		return ctrl.Result{}, nil
	}

	// get the registration manifest
	manifest, err := r.getClusterRegistrationManifest(ctx, rancherCluster.Status.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if manifest == "" {
		log.Info("Import manifest URL not set yet, requeue")
		return ctrl.Result{Requeue: true}, nil
	}

	if applyErr := r.applyImportManifest(ctx, capiCluster, manifest); applyErr != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *CAPIImportReconciler) shouldAutoImport(ctx context.Context, capiCluster *clusterv1.Cluster) (bool, error) {
	log := log.FromContext(ctx)
	log.V(2).Info("should we auto import the capi cluster", "name", capiCluster.Name, "namespace", capiCluster.Namespace)

	// Check CAPI cluster for label first
	hasLabel, autoImport := shouldImport(capiCluster)
	if hasLabel && autoImport {
		log.V(2).Info("cluster contains import annotation")

		return true, nil
	}
	if hasLabel && !autoImport {
		log.V(2).Info("cluster contains annotation to not import")

		return false, nil
	}

	// Check namespace wide
	ns := &corev1.Namespace{}
	key := client.ObjectKey{Name: capiCluster.Namespace}
	if err := r.Client.Get(ctx, key, ns); err != nil {
		log.Error(err, "getting namespace")
		return false, err
	}

	_, autoImport = shouldImport(ns)

	return autoImport, nil
}

func (r *CAPIImportReconciler) reconcileDelete(ctx context.Context, capiCluster *clusterv1.Cluster) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling CAPI Cluster Delete")

	//TODO: add implementation

	return ctrl.Result{}, nil
}

func (r *CAPIImportReconciler) getClusterRegistrationManifest(ctx context.Context, clusterName string) (string, error) {
	log := log.FromContext(ctx)

	key := client.ObjectKey{Name: clusterRegistrationTokenName, Namespace: clusterName}
	tokenHandler := rancher.NewClusterRegistrationTokenHandler(ctx, r.Client)
	token, err := tokenHandler.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("error getting registration token for cluster %s: %w", clusterName, err)
	}

	if token == nil {
		return "", nil
	}

	manifestData, err := r.downloadManifest(token.Status.ManifestURL)
	if err != nil {
		log.Error(err, "failed downloading import manifest")
		return "", err
	}

	return manifestData, nil
}

func (r *CAPIImportReconciler) applyImportManifest(ctx context.Context, capiCluster *clusterv1.Cluster, manifest string) error {
	log := log.FromContext(ctx)
	log.Info("Applying import manifest")

	clusterKey := client.ObjectKey{
		Name:      capiCluster.Name,
		Namespace: capiCluster.Namespace,
	}

	client, err := r.remoteClientGetter(ctx, capiCluster.Name, r.Client, clusterKey)
	if err != nil {
		return fmt.Errorf("getting remote cluster client: %w", err)
	}

	objs, err := manifestToObjects(strings.NewReader(manifest))
	if err != nil {
		return fmt.Errorf("getting objects from manifest: %w", err)
	}

	for _, obj := range objs {
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return err
		}
		unstructuredObj := &unstructured.Unstructured{}
		unstructuredObj.SetUnstructuredContent(u)

		if createErr := createObject(ctx, client, unstructuredObj, &log); createErr != nil {
			return createErr
		}
	}

	return nil
}

func shouldImport(obj metav1.Object) (hasLabel bool, labelValue bool) {
	labelVal, ok := obj.GetAnnotations()[importAnnotationName]
	if !ok {
		return false, false
	}
	autoImport, err := strconv.ParseBool(labelVal)
	if err != nil {
		return true, false
	}
	return true, autoImport
}

func createObject(ctx context.Context, c client.Client, obj client.Object, log *logr.Logger) error {
	gvk := obj.GetObjectKind().GroupVersionKind()

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := c.Get(ctx, client.ObjectKey{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}, u)

	if err == nil {
		// Already exists
		log.V(4).Info("object already exists in remote cluster", "gvk", gvk, "name", obj.GetName(), "namespace", obj.GetNamespace())
		//TODO: should we patch?
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting object from remote cluster: %w", err)
	}

	if err := c.Create(ctx, obj); err != nil {
		return fmt.Errorf("creating object in remote cluster: %w", err)
	}

	return nil
}

func (r *CAPIImportReconciler) rancherClusterToCapiCluster(ctx context.Context) handler.MapFunc {
	log := log.FromContext(ctx)

	return func(o client.Object) []ctrl.Request {
		key := client.ObjectKey{Name: o.GetName(), Namespace: o.GetNamespace()}

		capiCluster := &clusterv1.Cluster{}
		if err := r.Client.Get(ctx, key, capiCluster); err != nil {
			if !apierrors.IsNotFound(err) {
				log.Error(err, "getting capi cluster")
			}
			return nil
		}

		return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: capiCluster.Namespace, Name: capiCluster.Name}}}
	}
}

func (r *CAPIImportReconciler) namespaceToCapiClusters(ctx context.Context) handler.MapFunc {
	log := log.FromContext(ctx)

	return func(o client.Object) []ctrl.Request {
		ns, ok := o.(*corev1.Namespace)
		if !ok {
			log.Error(nil, fmt.Sprintf("Expected a Namespace but got a %T", o))
			return nil
		}

		_, autoImport := shouldImport(ns)
		if !autoImport {
			log.V(2).Info("Namespace doesn't have import annotation label with a true value, skipping")
			return nil
		}

		capiClusters := &clusterv1.ClusterList{}
		if err := r.Client.List(ctx, capiClusters, client.InNamespace(o.GetNamespace())); err != nil {
			log.Error(err, "getting capi cluster")
			return nil
		}

		if len(capiClusters.Items) == 0 {
			log.V(2).Info("No CAPI clusters in namespace, no action")
			return nil
		}

		reqs := []ctrl.Request{}
		for _, cluster := range capiClusters.Items {
			reqs = append(reqs, ctrl.Request{
				NamespacedName: client.ObjectKey{
					Namespace: cluster.Namespace,
					Name:      cluster.Name,
				},
			})
		}

		return reqs
	}
}

func rancherClusterNameFromCAPICluster(capiClusterName string) string {
	return fmt.Sprintf("%s-capi", capiClusterName)
}

func (r *CAPIImportReconciler) downloadManifest(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("downloading manifest: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading manifest: %w", err)
	}

	return string(data), err
}

func manifestToObjects(in io.Reader) ([]runtime.Object, error) {
	var result []runtime.Object
	reader := yamlDecoder.NewYAMLReader(bufio.NewReaderSize(in, 4096))
	for {
		raw, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		obj, err := toObjects(raw)
		if err != nil {
			return nil, err
		}

		result = append(result, obj...)
	}

	return result, nil
}

func toObjects(bytes []byte) ([]runtime.Object, error) {
	bytes, err := yamlDecoder.ToJSON(bytes)
	if err != nil {
		return nil, err
	}

	check := map[string]interface{}{}
	if err := json.Unmarshal(bytes, &check); err != nil || len(check) == 0 {
		return nil, err
	}

	obj, _, err := unstructured.UnstructuredJSONScheme.Decode(bytes, nil, nil)
	if err != nil {
		return nil, err
	}

	if l, ok := obj.(*unstructured.UnstructuredList); ok {
		var result []runtime.Object
		for _, obj := range l.Items {
			copy := obj
			result = append(result, &copy)
		}
		return result, nil
	}

	return []runtime.Object{obj}, nil
}
