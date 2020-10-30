package outgoing

import (
	"context"
	apimgmt "github.com/liqotech/liqo/pkg/virtualKubelet/apiReflection"
	ri "github.com/liqotech/liqo/pkg/virtualKubelet/apiReflection/reflectors/reflectorsInterfaces"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
	"strings"
)

type SecretsReflector struct {
	ri.APIReflector
}

func (r *SecretsReflector) SetSpecializedPreProcessingHandlers() {
	r.SetPreProcessingHandlers(ri.PreProcessingHandlers{
		IsAllowed:  r.isAllowed,
		AddFunc:    r.PreAdd,
		UpdateFunc: r.PreUpdate,
		DeleteFunc: r.PreDelete})
}

func (r *SecretsReflector) HandleEvent(e interface{}) {
	var err error

	event := e.(watch.Event)
	secret, ok := event.Object.(*corev1.Secret)
	if !ok {
		klog.Error("REFLECTION: cannot cast object to Secret")
		return
	}
	klog.V(3).Infof("REFLECTION: received %v for Secret %v/%v", event.Type, secret.Namespace, secret.Name)

	switch event.Type {
	case watch.Added:
		_, err := r.GetForeignClient().CoreV1().Secrets(secret.Namespace).Create(context.TODO(), secret, metav1.CreateOptions{})
		if kerrors.IsAlreadyExists(err) {
			klog.V(3).Infof("REFLECTION: The remote Secret %v/%v has not been created: %v", secret.Namespace, secret.Name, err)
		}
		if err != nil && !kerrors.IsAlreadyExists(err) {
			klog.Errorf("REFLECTION: Error while updating the remote Secret %v/%v - ERR: %v", secret.Namespace, secret.Name, err)
		} else {
			klog.V(3).Infof("REFLECTION: remote Secret %v/%v correctly created", secret.Namespace, secret.Name)
		}

	case watch.Modified:
		if _, err = r.GetForeignClient().CoreV1().Secrets(secret.Namespace).Update(context.TODO(), secret, metav1.UpdateOptions{}); err != nil {
			klog.Errorf("REFLECTION: Error while updating the remote Secret %v/%v - ERR: %v", secret.Namespace, secret.Name, err)
		} else {
			klog.V(3).Infof("REFLECTION: remote Secret %v/%v correctly updated", secret.Namespace, secret.Name)
		}

	case watch.Deleted:
		if err := r.GetForeignClient().CoreV1().Secrets(secret.Namespace).Delete(context.TODO(), secret.Name, metav1.DeleteOptions{}); err != nil {
			klog.Errorf("REFLECTION: Error while deleting the remote Secret %v/%v - ERR: %v", secret.Namespace, secret.Name, err)
		} else {
			klog.V(3).Infof("REFLECTION: remote Secret %v/%v correctly deleted", secret.Namespace, secret.Name)
		}
	}
}

func (r *SecretsReflector) KeyerFromObj(obj interface{}, remoteNamespace string) string {
	cm, ok := obj.(*corev1.Secret)
	if !ok {
		return ""
	}
	return strings.Join([]string{remoteNamespace, cm.Name}, "/")
}

func (r *SecretsReflector) CleanupNamespace(localNamespace string) {
	foreignNamespace, err := r.NattingTable().NatNamespace(localNamespace, false)
	if err != nil {
		klog.Error(err)
		return
	}

	// resync for ensuring to be remotely aligned with the foreign cluster state
	err = r.ForeignInformer(foreignNamespace).GetStore().Resync()
	if err != nil {
		klog.Errorf("error while resyncing secrets foreign cache - ERR: %v", err)
		return
	}

	objects := r.ForeignInformer(foreignNamespace).GetStore().List()

	retriable := func(err error) bool {
		switch kerrors.ReasonForError(err) {
		case metav1.StatusReasonNotFound:
			return false
		default:
			klog.Warningf("retrying while deleting secret because of- ERR; %v", err)
			return true
		}
	}
	for _, obj := range objects {
		sec := obj.(*corev1.Secret)
		if err := retry.OnError(retry.DefaultBackoff, retriable, func() error {
			return r.GetForeignClient().CoreV1().Secrets(foreignNamespace).Delete(context.TODO(), sec.Name, metav1.DeleteOptions{})
		}); err != nil {
			klog.Errorf("Error while deleting secret %v/%v", sec.Namespace, sec.Name)
		}
	}
}

func (r *SecretsReflector) PreAdd(obj interface{}) interface{} {
	secretLocal := obj.(*corev1.Secret).DeepCopy()
	klog.V(3).Infof("PreAdd routine started for Secret %v/%v", secretLocal.Namespace, secretLocal.Name)

	nattedNs, err := r.NattingTable().NatNamespace(secretLocal.Namespace, false)
	if err != nil {
		klog.Error(err)
		return nil
	}

	secretRemote := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        secretLocal.Name,
			Namespace:   nattedNs,
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
		},
		Data:       secretLocal.Data,
		StringData: secretLocal.StringData,
		Type:       secretLocal.Type,
	}

	for k, v := range secretLocal.Annotations {
		secretRemote.Annotations[k] = v
	}
	for k, v := range secretLocal.Labels {
		secretRemote.Labels[k] = v
	}
	secretRemote.Labels[apimgmt.LiqoLabelKey] = apimgmt.LiqoLabelValue

	// if the secret was generated by a ServiceAccount we can reflect it because
	// creation of secret with ServiceAccountToken type is not allowed by the API Server
	//
	// so we change the type of the secret and we label it with the name of the
	// ServiceAccount that originated it to be easy retrieved when we need it
	if secretRemote.Type == corev1.SecretTypeServiceAccountToken {
		secretRemote.Type = corev1.SecretTypeOpaque
		secretRemote.Labels["kubernetes.io/service-account.name"] = secretLocal.Annotations["kubernetes.io/service-account.name"]
		delete(secretRemote.Annotations, "kubernetes.io/service-account.name")
		delete(secretRemote.Annotations, "kubernetes.io/service-account.uid")
	}

	klog.V(3).Infof("PreAdd routine completed for secret %v/%v", secretLocal.Namespace, secretLocal.Name)
	return secretRemote
}

func (r *SecretsReflector) PreUpdate(newObj interface{}, _ interface{}) interface{} {
	newSecret := newObj.(*corev1.Secret).DeepCopy()

	nattedNs, err := r.NattingTable().NatNamespace(newSecret.Namespace, false)
	if err != nil {
		klog.Error(err)
		return nil
	}

	key := r.KeyerFromObj(newObj, nattedNs)
	oldRemoteObj, err := r.GetObjFromForeignCache(nattedNs, key)
	if err != nil {
		err = errors.Wrapf(err, "secret %v", key)
		klog.Error(err)
		return nil
	}
	oldRemoteSec := oldRemoteObj.(*corev1.Secret)

	newSecret.SetNamespace(nattedNs)
	newSecret.SetResourceVersion(oldRemoteSec.ResourceVersion)
	newSecret.SetUID(oldRemoteSec.UID)

	if newSecret.Labels == nil {
		newSecret.Labels = make(map[string]string)
	}
	for k, v := range oldRemoteSec.Labels {
		newSecret.Labels[k] = v
	}
	newSecret.Labels[apimgmt.LiqoLabelKey] = apimgmt.LiqoLabelValue

	if newSecret.Annotations == nil {
		newSecret.Annotations = make(map[string]string)
	}
	for k, v := range oldRemoteSec.Annotations {
		newSecret.Annotations[k] = v
	}

	if newSecret.Type == corev1.SecretTypeServiceAccountToken {
		newSecret.Type = corev1.SecretTypeOpaque
		newSecret.Labels["kubernetes.io/service-account.name"] = newSecret.Annotations["kubernetes.io/service-account.name"]
		delete(newSecret.Annotations, "kubernetes.io/service-account.name")
		delete(newSecret.Annotations, "kubernetes.io/service-account.uid")
	}

	return newSecret
}

func (r *SecretsReflector) PreDelete(obj interface{}) interface{} {
	serviceLocal := obj.(*corev1.Secret).DeepCopy()

	klog.V(3).Infof("PreDelete routine started for secret %v/%v", serviceLocal.Namespace, serviceLocal.Name)

	nattedNs, err := r.NattingTable().NatNamespace(serviceLocal.Namespace, false)
	if err != nil {
		klog.Error(err)
		return nil
	}
	serviceLocal.Namespace = nattedNs

	klog.V(3).Infof("PreDelete routine completed for secret %v/%v", serviceLocal.Namespace, serviceLocal.Name)
	return serviceLocal
}

func (r *SecretsReflector) isAllowed(obj interface{}) bool {
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		klog.Error("cannot convert obj to secret")
		return false
	}
	// if this annotation is set, this secret will not be reflected to the remote cluster
	val, ok := sec.Annotations["liqo.io/not-reflect"]
	return !ok || val != "true"
}

func addSecretsIndexers() cache.Indexers {
	i := cache.Indexers{}
	i["secrets"] = func(obj interface{}) ([]string, error) {
		secret, ok := obj.(*corev1.Secret)
		if !ok {
			return []string{}, errors.New("cannot convert obj to secret")
		}
		return []string{
			strings.Join([]string{secret.Namespace, secret.Name}, "/"),
		}, nil
	}
	return i
}
