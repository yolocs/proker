package proker

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"knative.dev/eventing/pkg/apis/eventing"
	"knative.dev/eventing/pkg/apis/eventing/v1alpha1"
	"knative.dev/pkg/kmeta"
)

// reconcileDeployment reconciles the K8s Deployment 'd'.
func (r *Reconciler) reconcileDeployment(ctx context.Context, d *v1.Deployment) (*v1.Deployment, error) {
	current, err := r.deploymentLister.Deployments(d.Namespace).Get(d.Name)
	if apierrs.IsNotFound(err) {
		current, err = r.KubeClientSet.AppsV1().Deployments(d.Namespace).Create(d)
		if err != nil {
			return nil, err
		}
		return current, nil
	} else if err != nil {
		return nil, err
	}

	if !equality.Semantic.DeepDerivative(d.Spec, current.Spec) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = d.Spec
		current, err = r.KubeClientSet.AppsV1().Deployments(current.Namespace).Update(desired)
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

// reconcileService reconciles the K8s Service 'svc'.
func (r *Reconciler) reconcileService(ctx context.Context, svc *corev1.Service) (*corev1.Service, error) {
	current, err := r.serviceLister.Services(svc.Namespace).Get(svc.Name)
	if apierrs.IsNotFound(err) {
		current, err = r.KubeClientSet.CoreV1().Services(svc.Namespace).Create(svc)
		if err != nil {
			return nil, err
		}
		return current, nil
	} else if err != nil {
		return nil, err
	}

	// spec.clusterIP is immutable and is set on existing services. If we don't set this to the same value, we will
	// encounter an error while updating.
	svc.Spec.ClusterIP = current.Spec.ClusterIP
	if !equality.Semantic.DeepDerivative(svc.Spec, current.Spec) {
		// Don't modify the informers copy.
		desired := current.DeepCopy()
		desired.Spec = svc.Spec
		current, err = r.KubeClientSet.CoreV1().Services(current.Namespace).Update(desired)
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

func fanoutLabels(brokerName string) map[string]string {
	return map[string]string{
		eventing.BrokerLabelKey:           brokerName,
		"eventing.knative.dev/brokerRole": "fanout",
	}
}

type fanoutArgs struct {
	Broker             *v1alpha1.Broker
	Image              string
	Project            string
	Topic              string
	Purgatory          string
	Subscription       string
	ListenersConfigMap string
}

func makeFanoutDeployment(args *fanoutArgs) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: args.Broker.Namespace,
			Name:      fmt.Sprintf("%s-broker-fanout", args.Broker.Name),
			OwnerReferences: []metav1.OwnerReference{
				*kmeta.NewControllerRef(args.Broker),
			},
			Labels: fanoutLabels(args.Broker.Name),
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: fanoutLabels(args.Broker.Name),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: fanoutLabels(args.Broker.Name),
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "fanout-filter",
							Image: args.Image,
							Env: []corev1.EnvVar{
								{
									Name:  "PROJECT",
									Value: args.Project,
								},
								{
									Name:  "TOPIC",
									Value: args.Topic,
								},
								{
									Name:  "PURGATORY",
									Value: args.Purgatory,
								},
								{
									Name:  "SUBSCRIPTION",
									Value: args.Subscription,
								},
								{
									Name:  "LISTENERS_CONFIG",
									Value: "/var/knative/listeners.json",
								},
								{
									Name:  "GOOGLE_APPLICATION_CREDENTIALS",
									Value: "/var/secrets/google/key.json",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "listeners-cfg",
									MountPath: "/var/knative",
									ReadOnly:  true,
								},
								{
									Name:      "google-cloud-key",
									MountPath: "/var/secrets/google",
									ReadOnly:  true,
								},
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 8080,
									Name:          "http",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "listeners-cfg",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: args.ListenersConfigMap,
									},
								},
							},
						},
						{
							Name: "google-cloud-key",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "google-cloud-key",
								},
							},
						},
					},
				},
			},
		},
	}
}

type ingressArgs struct {
	Broker  *v1alpha1.Broker
	Image   string
	Project string
	Topic   string
}

func ingressLabels(brokerName string) map[string]string {
	return map[string]string{
		eventing.BrokerLabelKey:           brokerName,
		"eventing.knative.dev/brokerRole": "ingress",
	}
}

func makeIngressDeployment(args *ingressArgs) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: args.Broker.Namespace,
			Name:      fmt.Sprintf("%s-broker-ingress", args.Broker.Name),
			OwnerReferences: []metav1.OwnerReference{
				*kmeta.NewControllerRef(args.Broker),
			},
			Labels: ingressLabels(args.Broker.Name),
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: ingressLabels(args.Broker.Name),
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ingressLabels(args.Broker.Name),
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "ingress",
							Image: args.Image,
							Env: []corev1.EnvVar{
								{
									Name:  "PROJECT",
									Value: args.Project,
								},
								{
									Name:  "TOPIC",
									Value: args.Topic,
								},
								{
									Name:  "GOOGLE_APPLICATION_CREDENTIALS",
									Value: "/var/secrets/google/key.json",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "google-cloud-key",
									MountPath: "/var/secrets/google",
									ReadOnly:  true,
								},
							},
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 8080,
									Name:          "http",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "google-cloud-key",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "google-cloud-key",
								},
							},
						},
					},
				},
			},
		},
	}
}

func makeIngressService(b *v1alpha1.Broker) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: b.Namespace,
			Name:      fmt.Sprintf("%s-broker", b.Name),
			Labels:    ingressLabels(b.Name),
			OwnerReferences: []metav1.OwnerReference{
				*kmeta.NewControllerRef(b),
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: ingressLabels(b.Name),
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: intstr.FromInt(8080),
				},
			},
		},
	}
}
