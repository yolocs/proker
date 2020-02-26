package proker

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/yolocs/proker/pkg/utils"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/labels"

	"knative.dev/eventing/pkg/apis/eventing/v1alpha1"
	messagingv1alpha1 "knative.dev/eventing/pkg/apis/messaging/v1alpha1"
	"knative.dev/eventing/pkg/logging"
)

const (
	triggerReconciled         = "TriggerReconciled"
	triggerReadinessChanged   = "TriggerReadinessChanged"
	triggerReconcileFailed    = "TriggerReconcileFailed"
	triggerUpdateStatusFailed = "TriggerUpdateStatusFailed"
	subscriptionDeleteFailed  = "SubscriptionDeleteFailed"
	subscriptionCreateFailed  = "SubscriptionCreateFailed"
	subscriptionGetFailed     = "SubscriptionGetFailed"
	triggerChannelFailed      = "TriggerChannelFailed"
	triggerServiceFailed      = "TriggerServiceFailed"
)

func (r *Reconciler) reconcileTriggers(ctx context.Context, b *v1alpha1.Broker, purgatory *pubsub.Topic) error {
	triggers, err := r.triggerLister.Triggers(b.Namespace).List(labels.Everything())
	if err != nil {
		return err
	}
	cfg, err := r.configmapLister.ConfigMaps(b.Namespace).Get(b.Name)
	if err != nil {
		return err
	}
	desired := cfg.DeepCopy()

	rawCfg := desired.Data["listeners.json"]
	var listeners map[string]utils.Listener
	if rawCfg != "" {
		if err := json.Unmarshal([]byte(rawCfg), &listeners); err != nil {
			return fmt.Errorf("Failed to decode listeners config: %w", err)
		}
	} else {
		listeners = make(map[string]utils.Listener)
	}

	var updateTriggers, failedTriggers []*v1alpha1.Trigger
	triggerSet := make(map[string]*v1alpha1.Trigger)

	for _, t := range triggers {
		if t.Spec.Broker == b.Name {
			trigger := t.DeepCopy()
			triggerSet[string(trigger.UID)] = trigger
			if err := r.reconcileTrigger(ctx, b, trigger, purgatory); err != nil {
				failedTriggers = append(failedTriggers, trigger)
			} else {
				updateTriggers = append(updateTriggers, trigger)
			}
		}
	}

	// Add and update existing triggers in the listeners config.
	for _, trigger := range updateTriggers {
		listeners[string(trigger.UID)] = utils.Listener{
			ID:                    string(trigger.UID),
			Name:                  trigger.Name,
			Namespace:             trigger.Namespace,
			Filters:               *trigger.Spec.Filter.Attributes,
			PurgatorySubscription: fmt.Sprintf("purgatory-%s", trigger.UID),
			Target:                trigger.Status.SubscriberURI.String(),
		}
	}
	// Remove any listener if not in the existing trigger list.
	for id := range listeners {
		t, ok := triggerSet[id]
		if !ok {
			if err := r.ensureTriggerSubscription(ctx, id, purgatory, false); err != nil {
				// This shouldn't really happen if we have finalizer on the triggers.
				logging.FromContext(ctx).Error("Failed to delete trigger subscription", zap.Error(err))
			}
			delete(listeners, id)
		} else if t.DeletionTimestamp != nil {
			delete(listeners, id)
		}
	}

	lb, err := json.Marshal(listeners)
	if err != nil {
		return fmt.Errorf("Failed to encode listeners config: %w", err)
	}
	desired.Data["listeners.json"] = string(lb)

	if !equality.Semantic.DeepDerivative(desired.Data, cfg.Data) {
		// If config map fails to update, marks all trigger as reconcilation failure.
		if _, err = r.KubeClientSet.CoreV1().ConfigMaps(desired.Namespace).Update(desired); err != nil {
			failedTriggers = append(failedTriggers, updateTriggers...)
			updateTriggers = []*v1alpha1.Trigger{}
		}
	}
	for _, trigger := range updateTriggers {
		r.Recorder.Event(trigger, corev1.EventTypeNormal, triggerReconciled, "Trigger reconciled")
		trigger.Status.ObservedGeneration = trigger.Generation
		// Again cheat...
		trigger.Status.PropagateSubscriptionStatus(&messagingv1alpha1.SubscriptionStatus{Status: b.Status.Status})

		if _, updateStatusErr := r.updateTriggerStatus(ctx, trigger); updateStatusErr != nil {
			logging.FromContext(ctx).Error("Failed to update Trigger status", zap.Error(updateStatusErr))
			r.Recorder.Eventf(trigger, corev1.EventTypeWarning, triggerUpdateStatusFailed, "Failed to update Trigger's status: %v", updateStatusErr)
		}
	}
	for _, trigger := range failedTriggers {
		r.Logger.Error("Reconciling trigger failed:", zap.String("name", trigger.Name), zap.Error(err))
		r.Recorder.Eventf(trigger, corev1.EventTypeWarning, triggerReconcileFailed, "Trigger reconcile failed: %v", err)
		trigger.Status.ObservedGeneration = trigger.Generation
		if _, updateStatusErr := r.updateTriggerStatus(ctx, trigger); updateStatusErr != nil {
			logging.FromContext(ctx).Error("Failed to update Trigger status", zap.Error(updateStatusErr))
			r.Recorder.Eventf(trigger, corev1.EventTypeWarning, triggerUpdateStatusFailed, "Failed to update Trigger's status: %v", updateStatusErr)
		}
	}

	return nil
}

func (r *Reconciler) reconcileTrigger(ctx context.Context, b *v1alpha1.Broker, t *v1alpha1.Trigger, purgatory *pubsub.Topic) error {
	t.Status.InitializeConditions()

	if t.DeletionTimestamp != nil {
		// Everything is cleaned up by the garbage collector.
		return r.ensureTriggerSubscription(ctx, string(t.UID), purgatory, false)
	}

	t.Status.PropagateBrokerStatus(&b.Status)
	// Cheat, we have no dependency...
	t.Status.MarkDependencySucceeded()

	if t.Spec.Subscriber.Ref != nil {
		// To call URIFromDestination(dest apisv1alpha1.Destination, parent interface{}), dest.Ref must have a Namespace
		// We will use the Namespace of Trigger as the Namespace of dest.Ref
		t.Spec.Subscriber.Ref.Namespace = t.GetNamespace()
	}

	subscriberURI, err := r.uriResolver.URIFromDestinationV1(t.Spec.Subscriber, b)
	if err != nil {
		logging.FromContext(ctx).Error("Unable to get the Subscriber's URI", zap.Error(err))
		t.Status.MarkSubscriberResolvedFailed("Unable to get the Subscriber's URI", "%v", err)
		t.Status.SubscriberURI = nil
		return err
	}
	t.Status.SubscriberURI = subscriberURI
	t.Status.MarkSubscriberResolvedSucceeded()

	return r.ensureTriggerSubscription(ctx, string(t.UID), purgatory, true)
}

func (r *Reconciler) ensureTriggerSubscription(ctx context.Context, triggerID string, purgatory *pubsub.Topic, exists bool) error {
	subName := fmt.Sprintf("purgatory-%s", triggerID)
	sub := r.psClient.Subscription(subName)
	subExists, err := sub.Exists(ctx)
	if err != nil {
		return fmt.Errorf("Failed to check if purgatory subscription exists for trigger: %w", err)
	}
	if exists && !subExists {
		sub, err = r.psClient.CreateSubscription(ctx, subName, pubsub.SubscriptionConfig{
			Topic:       purgatory,
			AckDeadline: 30 * time.Second,
		})
		if err != nil {
			return fmt.Errorf("Failed to create purgatory subscription for trigger: %w", err)
		}
	} else if !exists && subExists {
		if err := sub.Delete(ctx); err != nil {
			return fmt.Errorf("Failed to delete purgatory subscription for trigger: %w", err)
		}
	}
	return nil
}

func (r *Reconciler) updateTriggerStatus(ctx context.Context, desired *v1alpha1.Trigger) (*v1alpha1.Trigger, error) {
	trigger, err := r.triggerLister.Triggers(desired.Namespace).Get(desired.Name)
	if err != nil {
		return nil, err
	}

	if reflect.DeepEqual(trigger.Status, desired.Status) {
		return trigger, nil
	}

	becomesReady := desired.Status.IsReady() && !trigger.Status.IsReady()

	// Don't modify the informers copy.
	existing := trigger.DeepCopy()
	existing.Status = desired.Status

	trig, err := r.EventingClientSet.EventingV1alpha1().Triggers(desired.Namespace).UpdateStatus(existing)
	if err == nil && becomesReady {
		duration := time.Since(trig.ObjectMeta.CreationTimestamp.Time)
		r.Logger.Infof("Trigger %q became ready after %v", trigger.Name, duration)
		r.Recorder.Event(trigger, corev1.EventTypeNormal, triggerReadinessChanged, fmt.Sprintf("Trigger %q became ready", trigger.Name))
		if err := r.StatsReporter.ReportReady("Trigger", trigger.Namespace, trigger.Name, duration); err != nil {
			logging.FromContext(ctx).Sugar().Infof("failed to record ready for Trigger, %v", err)
		}
	}

	return trig, err
}
