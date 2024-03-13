package k0smotronio

import (
	"context"

	km "github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func (r *ClusterReconciler) reconcileControllerJoinToken(ctx context.Context, kmc km.Cluster) error {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling jointoken")

	jtr := km.JoinTokenRequest{
		TypeMeta: metav1.TypeMeta{
			Kind:       "JoinTokenRequest",
			APIVersion: km.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        kmc.GetControllerJoinTokenName(),
			Namespace:   kmc.Namespace,
			Labels:      labelsForCluster(&kmc),
			Annotations: annotationsForCluster(&kmc),
		},
		Spec: km.JoinTokenRequestSpec{
			ClusterRef: km.ClusterRef{
				Name:      kmc.Name,
				Namespace: kmc.Namespace,
			},
			Role: "controller",
		},
	}
	if err := r.preCreateSecret(ctx, jtr); err != nil {
		return err
	}

	_ = ctrl.SetControllerReference(&kmc, &jtr, r.Scheme)
	return r.Client.Patch(ctx, &jtr, client.Apply, patchOpts...)
}

// TODO: this is a bit of a hack to get the StatefulSet up and running before
// the actual token is created as the pod needs to mount the jtr secret.
func (r *ClusterReconciler) preCreateSecret(ctx context.Context, jtr km.JoinTokenRequest) error {
	secret := v1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        jtr.Name,
			Namespace:   jtr.Namespace,
			Annotations: jtr.Annotations,
		},
	}

	return r.Client.Patch(ctx, &secret, client.Apply, patchOpts...)
}
