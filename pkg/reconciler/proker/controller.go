package proker

import (
	"context"
	"log"

	"cloud.google.com/go/pubsub"
	"github.com/kelseyhightower/envconfig"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	"knative.dev/eventing/pkg/apis/eventing"
	"knative.dev/eventing/pkg/apis/eventing/v1alpha1"
	brokerinformer "knative.dev/eventing/pkg/client/injection/informers/eventing/v1alpha1/broker"
	triggerinformer "knative.dev/eventing/pkg/client/injection/informers/eventing/v1alpha1/trigger"
	brokerreconciler "knative.dev/eventing/pkg/client/injection/reconciler/eventing/v1alpha1/broker"

	"knative.dev/eventing/pkg/reconciler"
	deploymentinformer "knative.dev/pkg/client/injection/kube/informers/apps/v1/deployment"
	configmapinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/configmap"
	serviceinformer "knative.dev/pkg/client/injection/kube/informers/core/v1/service"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/resolver"
)

const (
	// ReconcilerName is the name of the reconciler
	ReconcilerName = "Brokers"
	// controllerAgentName is the string used by this controller to identify
	// itself when creating events.
	controllerAgentName = "broker-controller"
)

type envConfig struct {
	FanoutImage  string `envconfig:"FANOUT_IMAGE" required:"true"`
	IngressImage string `envconfig:"INGRESS_IMAGE" required:"true"`
	BrokerClass  string `envconfig:"BROKER_CLASS" required:"true"`
	Project      string `envconfig:"PROJECT" required:"true"`
}

// NewController initializes the controller and is called by the generated code
// Registers event handlers to enqueue events
func NewController(
	ctx context.Context,
	cmw configmap.Watcher,
) *controller.Impl {

	var env envConfig
	if err := envconfig.Process("", &env); err != nil {
		log.Fatal("Failed to process env var", zap.Error(err))
	}

	psclient, err := pubsub.NewClient(ctx, env.Project)
	if err != nil {
		log.Fatal("Failed to create pubsub client", zap.Error(err))
	}

	deploymentInformer := deploymentinformer.Get(ctx)
	brokerInformer := brokerinformer.Get(ctx)
	triggerInformer := triggerinformer.Get(ctx)
	serviceInformer := serviceinformer.Get(ctx)
	configmapInformer := configmapinformer.Get(ctx)

	r := &Reconciler{
		Base:             reconciler.NewBase(ctx, controllerAgentName, cmw),
		brokerLister:     brokerInformer.Lister(),
		serviceLister:    serviceInformer.Lister(),
		configmapLister:  configmapInformer.Lister(),
		deploymentLister: deploymentInformer.Lister(),
		triggerLister:    triggerInformer.Lister(),
		fanoutImage:      env.FanoutImage,
		ingressImage:     env.IngressImage,
		psClient:         psclient,
		project:          env.Project,
	}
	impl := brokerreconciler.NewImpl(ctx, r)

	r.Logger.Info("Setting up event handlers")

	r.uriResolver = resolver.NewURIResolver(ctx, impl.EnqueueKey)

	brokerInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: pkgreconciler.AnnotationFilterFunc(eventing.BrokerClassKey, env.BrokerClass, false),
		Handler:    controller.HandleAll(impl.Enqueue),
	})

	serviceInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.Filter(v1alpha1.SchemeGroupVersion.WithKind("Broker")),
		Handler:    controller.HandleAll(impl.EnqueueControllerOf),
	})

	deploymentInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.Filter(v1alpha1.SchemeGroupVersion.WithKind("Broker")),
		Handler:    controller.HandleAll(impl.EnqueueControllerOf),
	})

	configmapInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: controller.Filter(v1alpha1.SchemeGroupVersion.WithKind("Broker")),
		Handler:    controller.HandleAll(impl.EnqueueControllerOf),
	})

	triggerInformer.Informer().AddEventHandler(controller.HandleAll(
		func(obj interface{}) {
			if trigger, ok := obj.(*v1alpha1.Trigger); ok {
				impl.EnqueueKey(types.NamespacedName{Namespace: trigger.Namespace, Name: trigger.Spec.Broker})
			}
		},
	))

	return impl
}
