package proker

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/pubsub"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"

	eventingduckv1alpha1 "knative.dev/eventing/pkg/apis/duck/v1alpha1"
	"knative.dev/eventing/pkg/apis/eventing/v1alpha1"
	brokerreconciler "knative.dev/eventing/pkg/client/injection/reconciler/eventing/v1alpha1/broker"
	eventinglisters "knative.dev/eventing/pkg/client/listers/eventing/v1alpha1"
	"knative.dev/eventing/pkg/logging"
	"knative.dev/eventing/pkg/reconciler"
	"knative.dev/eventing/pkg/reconciler/names"
	"knative.dev/pkg/apis"
	duckv1alpha1 "knative.dev/pkg/apis/duck/v1alpha1"
	"knative.dev/pkg/kmeta"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/resolver"
)

const (
	// Name of the corev1.Events emitted from the Broker reconciliation process.
	brokerReconcileError = "BrokerReconcileError"
	brokerReconciled     = "BrokerReconciled"
)

type Reconciler struct {
	*reconciler.Base

	// listers index properties about resources
	brokerLister     eventinglisters.BrokerLister
	serviceLister    corev1listers.ServiceLister
	configmapLister  corev1listers.ConfigMapLister
	deploymentLister appsv1listers.DeploymentLister
	triggerLister    eventinglisters.TriggerLister

	uriResolver *resolver.URIResolver

	fanoutImage  string
	ingressImage string

	project  string
	psClient *pubsub.Client
}

// Check that our Reconciler implements Interface
var _ brokerreconciler.Interface = (*Reconciler)(nil)
var _ brokerreconciler.Finalizer = (*Reconciler)(nil)

var brokerGVK = v1alpha1.SchemeGroupVersion.WithKind("Broker")

func newReconciledNormal(namespace, name string) pkgreconciler.Event {
	return pkgreconciler.NewEvent(corev1.EventTypeNormal, brokerReconciled, "Broker reconciled: \"%s/%s\"", namespace, name)
}

func (r *Reconciler) ReconcileKind(ctx context.Context, b *v1alpha1.Broker) pkgreconciler.Event {
	purgatory, err := r.reconcileKind(ctx, b)

	if b.Status.IsReady() {
		// So, at this point the Broker is ready and everything should be solid
		// for the triggers to act upon, so reconcile them.
		te := r.reconcileTriggers(ctx, b, purgatory)
		if te != nil {
			logging.FromContext(ctx).Error("Problem reconciling triggers", zap.Error(te))
			return fmt.Errorf("failed to reconcile triggers: %v", te)
		}
	} else {
		// Broker is not ready, but propagate it's status to my triggers.
		if te := r.propagateBrokerStatusToTriggers(ctx, b.Namespace, b.Name, &b.Status); te != nil {
			return fmt.Errorf("Trigger reconcile failed: %v", te)
		}
	}
	return err
}

func (r *Reconciler) FinalizeKind(ctx context.Context, b *v1alpha1.Broker) pkgreconciler.Event {
	if err := r.propagateBrokerStatusToTriggers(ctx, b.Namespace, b.Name, nil); err != nil {
		return fmt.Errorf("Trigger reconcile failed: %v", err)
	}
	return newReconciledNormal(b.Namespace, b.Name)
}

func (r *Reconciler) reconcileKind(ctx context.Context, b *v1alpha1.Broker) (*pubsub.Topic, pkgreconciler.Event) {
	logging.FromContext(ctx).Debug("Reconciling", zap.Any("Broker", b))
	b.Status.InitializeConditions()
	b.Status.ObservedGeneration = b.Generation

	// Missing GC for GCP resources.

	if err := r.ensureConfigMapExists(ctx, b); err != nil {
		return nil, err
	}

	mtName := fmt.Sprintf("proker-main-%s", b.UID)
	mt := r.psClient.Topic(mtName)
	mtExists, err := mt.Exists(ctx)
	if err != nil {
		return nil, fmt.Errorf("Failed to check if main topic exists: %w", err)
	}
	if !mtExists {
		mt, err = r.psClient.CreateTopic(ctx, fmt.Sprintf("proker-main-%s", b.UID))
		if err != nil {
			return nil, fmt.Errorf("Failed to create main topic: %w", err)
		}
	}

	ptName := fmt.Sprintf("proker-purgatory-%s", b.UID)
	pt := r.psClient.Topic(ptName)
	ptExists, err := pt.Exists(ctx)
	if err != nil {
		return nil, fmt.Errorf("Failed to check if purgatory topic exists: %w", err)
	}
	if !ptExists {
		pt, err = r.psClient.CreateTopic(ctx, fmt.Sprintf("proker-purgatory-%s", b.UID))
		if err != nil {
			return nil, fmt.Errorf("Failed to create purgatory topic: %w", err)
		}
	}

	subName := fmt.Sprintf("proker-main-%s", b.UID)
	sub := r.psClient.Subscription(subName)
	subExists, err := sub.Exists(ctx)
	if err != nil {
		return nil, fmt.Errorf("Failed to check if main topic subscription: %w", err)
	}
	if !subExists {
		sub, err = r.psClient.CreateSubscription(ctx, fmt.Sprintf("proker-main-%s", b.UID), pubsub.SubscriptionConfig{
			Topic:       mt,
			AckDeadline: 10 * time.Minute,
		})
		if err != nil {
			return nil, fmt.Errorf("Failed to create main subscription: %w", err)
		}
	}
	// Cheating here...
	b.Status.PropagateTriggerChannelReadiness(
		&eventingduckv1alpha1.ChannelableStatus{AddressStatus: duckv1alpha1.AddressStatus{Address: &duckv1alpha1.Addressable{}}})

	fanoutDeploy := makeFanoutDeployment(&fanoutArgs{
		Broker:             b,
		Image:              r.fanoutImage,
		ListenersConfigMap: b.Name,
		Project:            r.project,
		Topic:              mt.ID(),
		Purgatory:          pt.ID(),
		Subscription:       sub.ID(),
	})
	fd, err := r.reconcileDeployment(ctx, fanoutDeploy)
	if err != nil {
		return nil, err
	}
	// Use fanout as filter
	b.Status.PropagateFilterDeploymentAvailability(fd)

	ingressDeploy := makeIngressDeployment(&ingressArgs{
		Broker:  b,
		Image:   r.ingressImage,
		Project: r.project,
		Topic:   mt.ID(),
	})
	ind, err := r.reconcileDeployment(ctx, ingressDeploy)
	if err != nil {
		return nil, err
	}
	insvc, err := r.reconcileService(ctx, makeIngressService(b))
	if err != nil {
		return nil, err
	}

	b.Status.PropagateIngressDeploymentAvailability(ind)
	b.Status.SetAddress(&apis.URL{
		Scheme: "http",
		Host:   names.ServiceHostName(insvc.Name, insvc.Namespace),
	})

	return pt, nil
}

func (r *Reconciler) ensureConfigMapExists(ctx context.Context, b *v1alpha1.Broker) error {
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            b.Name,
			Namespace:       b.Namespace,
			OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(b)},
		},
		Data: map[string]string{
			"listeners.json": "",
		},
	}
	_, err := r.configmapLister.ConfigMaps(b.Namespace).Get(b.Name)
	if apierrs.IsNotFound(err) {
		_, err = r.KubeClientSet.CoreV1().ConfigMaps(b.Namespace).Create(desired)
		if err != nil {
			return fmt.Errorf("failed to create configmap: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get configmap: %w", err)
	}

	return nil
}

func (r *Reconciler) propagateBrokerStatusToTriggers(ctx context.Context, namespace, name string, bs *v1alpha1.BrokerStatus) error {
	triggers, err := r.triggerLister.Triggers(namespace).List(labels.Everything())
	if err != nil {
		return err
	}
	for _, t := range triggers {
		if t.Spec.Broker == name {
			// Don't modify informers copy
			trigger := t.DeepCopy()
			trigger.Status.InitializeConditions()
			if bs == nil {
				trigger.Status.MarkBrokerFailed("BrokerDoesNotExist", "Broker %q does not exist", name)
			} else {
				trigger.Status.PropagateBrokerStatus(bs)
			}
			if _, updateStatusErr := r.updateTriggerStatus(ctx, trigger); updateStatusErr != nil {
				logging.FromContext(ctx).Error("Failed to update Trigger status", zap.Error(updateStatusErr))
				r.Recorder.Eventf(trigger, corev1.EventTypeWarning, triggerUpdateStatusFailed, "Failed to update Trigger's status: %v", updateStatusErr)
				return updateStatusErr
			}
		}
	}
	return nil
}
