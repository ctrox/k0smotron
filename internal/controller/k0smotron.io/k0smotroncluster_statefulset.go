/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package k0smotronio

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	km "github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta1"
	"github.com/k0sproject/k0smotron/internal/controller/util"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var entrypointDefaultMode = int32(0744)

const clusterLabel = "k0smotron.io/cluster"

// findStatefulSetPod returns a first running pod from a StatefulSet
func (r *ClusterReconciler) findStatefulSetPod(ctx context.Context, statefulSet string, namespace string) (*v1.Pod, error) {
	return util.FindStatefulSetPod(ctx, r.ClientSet, statefulSet, namespace)
}

func (r *ClusterReconciler) generateStatefulSet(kmc *km.Cluster) (apps.StatefulSet, error) {
	k0sVersion := kmc.Spec.K0sVersion
	if k0sVersion == "" {
		k0sVersion = defaultK0SVersion
	}

	if kmc.Spec.Replicas > 1 && (kmc.Spec.KineDataSourceURL == "" && kmc.Spec.KineDataSourceSecretName == "") {
		return apps.StatefulSet{}, errors.New("kineDataSourceURL can't be empty if replicas > 1")

	}

	labels := labelsForCluster(kmc)

	statefulSet := apps.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "StatefulSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        kmc.GetStatefulSetName(),
			Namespace:   kmc.Namespace,
			Labels:      labels,
			Annotations: annotationsForCluster(kmc),
		},
		Spec: apps.StatefulSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Replicas: &kmc.Spec.Replicas,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{
					Volumes: []v1.Volume{{
						Name: kmc.GetEntrypointConfigMapName(),
						VolumeSource: v1.VolumeSource{
							ConfigMap: &v1.ConfigMapVolumeSource{
								LocalObjectReference: v1.LocalObjectReference{
									Name: kmc.GetEntrypointConfigMapName(),
								},
								DefaultMode: &entrypointDefaultMode,
								Items: []v1.KeyToPath{{
									Key:  "k0smotron-entrypoint.sh",
									Path: "k0smotron-entrypoint.sh",
								}},
							},
						},
					}},
					Containers: []v1.Container{{
						Name:            "controller",
						Image:           fmt.Sprintf("%s:%s", kmc.Spec.K0sImage, k0sVersion),
						ImagePullPolicy: v1.PullIfNotPresent,
						Args:            []string{"/k0smotron-entrypoint.sh"},
						Ports: []v1.ContainerPort{
							{
								Name:          "api",
								Protocol:      v1.ProtocolTCP,
								ContainerPort: int32(kmc.Spec.Service.APIPort),
							},
							{
								Name:          "konnectivity",
								Protocol:      v1.ProtocolTCP,
								ContainerPort: int32(kmc.Spec.Service.KonnectivityPort),
							},
						},
						EnvFrom: []v1.EnvFromSource{{
							ConfigMapRef: &v1.ConfigMapEnvSource{
								LocalObjectReference: v1.LocalObjectReference{
									Name: kmc.GetConfigMapName(),
								},
							},
						}},
						Resources: kmc.Spec.Resources,
						ReadinessProbe: &v1.Probe{
							InitialDelaySeconds: 5,
							ProbeHandler:        v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{"k0s", "status"}}},
						},
						LivenessProbe: &v1.Probe{
							InitialDelaySeconds: 10,
							ProbeHandler:        v1.ProbeHandler{Exec: &v1.ExecAction{Command: []string{"k0s", "status"}}},
						},
						VolumeMounts: []v1.VolumeMount{{
							Name:      kmc.GetEntrypointConfigMapName(),
							MountPath: "/k0smotron-entrypoint.sh",
							SubPath:   "k0smotron-entrypoint.sh",
						}},
					}},
				}},
		}}

	if kmc.Spec.EnableMonitoring {
		if kmc.Spec.Persistence.Type == "" {
			kmc.Spec.Persistence.Type = "emptyDir"
		}
		r.addMonitoringStack(kmc, &statefulSet)
	}

	if kmc.Spec.KineDataSourceSecretName != "" {
		statefulSet.Spec.Template.Spec.Containers[0].EnvFrom = append(statefulSet.Spec.Template.Spec.Containers[0].EnvFrom, v1.EnvFromSource{
			SecretRef: &v1.SecretEnvSource{
				LocalObjectReference: v1.LocalObjectReference{
					Name: kmc.Spec.KineDataSourceSecretName,
				},
			},
		})
	}
	// Mount certificates if they are provided
	if kmc.Spec.CertificateRefs != nil && len(kmc.Spec.CertificateRefs) > 0 {
		r.mountSecrets(kmc, &statefulSet)
	}

	switch kmc.Spec.Persistence.Type {
	case "emptyDir":
		statefulSet.Spec.Template.Spec.Volumes = append(statefulSet.Spec.Template.Spec.Volumes, v1.Volume{
			Name: kmc.GetVolumeName(),
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{},
			},
		})
		statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts = append(statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts, v1.VolumeMount{
			Name:      kmc.GetVolumeName(),
			MountPath: "/var/lib/k0s",
		})
	case "hostPath":
		statefulSet.Spec.Template.Spec.Volumes = append(statefulSet.Spec.Template.Spec.Volumes, v1.Volume{
			Name: kmc.GetVolumeName(),
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: kmc.Spec.Persistence.HostPath,
				},
			},
		})
		statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts = append(statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts, v1.VolumeMount{
			Name:      kmc.GetVolumeName(),
			MountPath: "/var/lib/k0s",
		})
	case "pvc":
		statefulSet.Spec.VolumeClaimTemplates = append(statefulSet.Spec.VolumeClaimTemplates, kmc.Spec.Persistence.PersistentVolumeClaim)

		statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts = append(statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts, v1.VolumeMount{
			Name:      kmc.GetVolumeName(),
			MountPath: "/var/lib/k0s",
		})
	}

	for _, manifest := range kmc.Spec.Manifests {
		statefulSet.Spec.Template.Spec.Volumes = append(statefulSet.Spec.Template.Spec.Volumes, manifest)

		statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts = append(statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts, v1.VolumeMount{
			Name:      manifest.Name,
			MountPath: fmt.Sprintf("/var/lib/k0s/manifests/%s", manifest.Name),
			ReadOnly:  true,
		})
	}

	// Create k0s telemetry config in the configmap and mount it to the controller pod
	// If user disables k0s telemetry this will have not effect.
	cm := &v1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("kmc-%s-telemetry-config", kmc.Name),
			Namespace: kmc.Namespace,
		},
		Data: map[string]string{
			"configmap.yaml": `
apiVersion: v1
kind: ConfigMap
metadata:
  name: k0s-telemetry
  namespace: kube-system
data:
  provider: "k0smotron"
`,
		},
	}
	if err := ctrl.SetControllerReference(kmc, cm, r.Scheme); err != nil {
		return apps.StatefulSet{}, err
	}
	if err := r.Client.Patch(context.Background(), cm, client.Apply, patchOpts...); err != nil {
		return apps.StatefulSet{}, err
	}
	statefulSet.Spec.Template.Spec.Volumes = append(statefulSet.Spec.Template.Spec.Volumes, v1.Volume{
		Name: cm.Name,
		VolumeSource: v1.VolumeSource{
			ConfigMap: &v1.ConfigMapVolumeSource{
				LocalObjectReference: v1.LocalObjectReference{Name: cm.Name},
			},
		},
	})

	statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts = append(statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts, v1.VolumeMount{
		Name:      cm.Name,
		MountPath: "/var/lib/k0s/manifests/k0s-telemetry",
		ReadOnly:  true,
	})

	err := ctrl.SetControllerReference(kmc, &statefulSet, r.Scheme)
	return statefulSet, err
}

// mountSecrets mounts the certificates as secrets to the controller and creates
// an init container that copies the certificates to the correct location
func (r *ClusterReconciler) mountSecrets(kmc *km.Cluster, sfs *apps.StatefulSet) {
	projectedSecrets := []v1.VolumeProjection{}

	for _, cert := range kmc.Spec.CertificateRefs {
		switch cert.Type {
		case "ca":
			projectedSecrets = append(projectedSecrets, v1.VolumeProjection{
				Secret: &v1.SecretProjection{
					LocalObjectReference: v1.LocalObjectReference{Name: cert.Name},
					Items: []v1.KeyToPath{
						{
							Key:  "tls.crt",
							Path: "ca.crt",
						},
						{
							Key:  "tls.key",
							Path: "ca.key",
						},
					},
				},
			})

		case "sa":
			projectedSecrets = append(projectedSecrets, v1.VolumeProjection{
				Secret: &v1.SecretProjection{
					LocalObjectReference: v1.LocalObjectReference{Name: cert.Name},
					Items: []v1.KeyToPath{
						{
							Key:  "tls.crt",
							Path: "sa.pub",
						},
						{
							Key:  "tls.key",
							Path: "sa.key",
						},
					},
				},
			})
		case "proxy":
			projectedSecrets = append(projectedSecrets, v1.VolumeProjection{
				Secret: &v1.SecretProjection{
					LocalObjectReference: v1.LocalObjectReference{Name: cert.Name},
					Items: []v1.KeyToPath{
						{
							Key:  "tls.crt",
							Path: "front-proxy-ca.crt",
						},
						{
							Key:  "tls.key",
							Path: "front-proxy-ca.key",
						},
					},
				},
			})

		}
	}
	sfs.Spec.Template.Spec.Volumes = append(sfs.Spec.Template.Spec.Volumes, v1.Volume{
		Name: "certs",
		VolumeSource: v1.VolumeSource{
			Projected: &v1.ProjectedVolumeSource{
				Sources: projectedSecrets,
			},
		},
	})

	// We need to copy the certs from the projected volume to the /var/lib/k0s/pki directory
	// Otherwise k0s will trip over the permissions and RO mounts
	sfs.Spec.Template.Spec.InitContainers = append(sfs.Spec.Template.Spec.InitContainers, v1.Container{
		Name:  "certs-init",
		Image: "busybox",
		Command: []string{
			"sh",
			"-c",
			"mkdir -p /var/lib/k0s/pki && cp /certs-init/*.* /var/lib/k0s/pki/",
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      "certs",
				MountPath: "/certs-init",
			},
			{
				Name:      kmc.GetVolumeName(),
				MountPath: "/var/lib/k0s",
			},
		},
	})
}

func (r *ClusterReconciler) addMonitoringStack(kmc *km.Cluster, statefulSet *apps.StatefulSet) {
	nginxConfCMName := kmc.GetMonitoringConfigMapName() + "-nginx"
	statefulSet.Spec.Template.Spec.Containers = append(statefulSet.Spec.Template.Spec.Containers, v1.Container{
		Name:            "monitoring-agent",
		Image:           "quay.io/k0sproject/prometheus:v2.44.0",
		ImagePullPolicy: v1.PullIfNotPresent,
		Command:         []string{"prometheus", "--config.file=/prometheus/prometheus.yml"},
		Args:            []string{"--storage.tsdb.retention.size=200MB"},
		Ports: []v1.ContainerPort{{
			Name:          "prometheus",
			Protocol:      v1.ProtocolTCP,
			ContainerPort: int32(9090),
		}},
		VolumeMounts: []v1.VolumeMount{{
			Name:      kmc.GetVolumeName(),
			MountPath: "/var/lib/k0s",
		}, {
			Name:      kmc.GetMonitoringConfigMapName(),
			MountPath: "/prometheus/prometheus.yml",
			SubPath:   "prometheus.yml",
		}},
	}, v1.Container{
		Name:            "monitoring-proxy",
		Image:           "nginx:1.19.10",
		ImagePullPolicy: v1.PullIfNotPresent,
		Ports: []v1.ContainerPort{{
			Name:          "nginx",
			Protocol:      v1.ProtocolTCP,
			ContainerPort: int32(8090),
		}},
		VolumeMounts: []v1.VolumeMount{{
			Name:      nginxConfCMName,
			MountPath: "/etc/nginx/nginx.conf",
			SubPath:   "nginx.conf",
		}},
	})

	statefulSet.Spec.Template.Annotations = map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "8090",
		"prometheus.io/path":   "/metrics",
	}
	statefulSet.Spec.Template.Spec.Volumes = append(statefulSet.Spec.Template.Spec.Volumes, v1.Volume{
		Name: kmc.GetMonitoringConfigMapName(),
		VolumeSource: v1.VolumeSource{
			ConfigMap: &v1.ConfigMapVolumeSource{
				LocalObjectReference: v1.LocalObjectReference{
					Name: kmc.GetMonitoringConfigMapName(),
				},
				DefaultMode: &entrypointDefaultMode,
				Items: []v1.KeyToPath{{
					Key:  "prometheus.yml",
					Path: "prometheus.yml",
				}},
			},
		},
	}, v1.Volume{
		Name: nginxConfCMName,
		VolumeSource: v1.VolumeSource{
			ConfigMap: &v1.ConfigMapVolumeSource{
				LocalObjectReference: v1.LocalObjectReference{
					Name: kmc.GetMonitoringConfigMapName(),
				},
				DefaultMode: &entrypointDefaultMode,
				Items: []v1.KeyToPath{{
					Key:  "nginx.conf",
					Path: "nginx.conf",
				}},
			},
		},
	})
}

func (r *ClusterReconciler) reconcileStatefulSet(ctx context.Context, kmc km.Cluster) error {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling statefulset")
	statefulSet, err := r.generateStatefulSet(&kmc)
	if err != nil {
		return fmt.Errorf("failed to generate statefulset: %w", err)
	}

	foundStatefulSet, err := r.ClientSet.AppsV1().StatefulSets(statefulSet.Namespace).Get(ctx, statefulSet.Name, metav1.GetOptions{})
	if err != nil && apierrors.IsNotFound(err) {
		return r.Client.Patch(ctx, &statefulSet, client.Apply, patchOpts...)
	} else if err == nil {
		if !isStatefulSetsEqual(&statefulSet, foundStatefulSet) {
			return r.Client.Patch(ctx, &statefulSet, client.Apply, patchOpts...)
		}

		if foundStatefulSet.Status.ReadyReplicas == kmc.Spec.Replicas {
			r.updateReadiness(ctx, kmc, true)
		}

		return nil
	}

	return err
}

func isStatefulSetsEqual(new, old *apps.StatefulSet) bool {
	return *new.Spec.Replicas == *old.Spec.Replicas &&
		new.Spec.Template.Spec.Containers[0].Image == old.Spec.Template.Spec.Containers[0].Image &&
		reflect.DeepEqual(new.Spec.Selector, old.Spec.Selector) &&
		reflect.DeepEqual(new.Spec.Template.Labels, old.Spec.Template.Labels) &&
		reflect.DeepEqual(new.Spec.Template.Spec.Containers[0].VolumeMounts, old.Spec.Template.Spec.Containers[0].VolumeMounts)
}
