package extension

import (
	"context"

	"code.cloudfoundry.org/diego-ssh/keys"
	. "code.cloudfoundry.org/eirini-ssh/pkg/logger"
	eirinix "code.cloudfoundry.org/eirinix"
        
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	admissionapi "k8s.io/api/admission/v1beta1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type SSH struct{ Namespace string }

func generateSecretNameForPod(pod *v1.Pod) (string, error) {
	guid, ok := pod.GetLabels()[eirinix.LabelGUID]
	version, ok := pod.GetLabels()[eirinix.LabelVersion]
	if !ok {
		return "", errors.New("Couldn't get Eirini APP UID")
	}

	index := extractInstanceID(pod.Name)

	return guid + "-" + version + "-" + index + "-ssh-key-meta", nil
}

func getVolume(name, path string) (v1.Volume, v1.VolumeMount) {
	mount := v1.VolumeMount{
		Name:      name,
		MountPath: path,
	}

	vol := v1.Volume{
		Name: name,
	}

	return vol, mount
}

func extractInstanceID(s string) string {
	var res string
	el := strings.Split(s, "-")
	if len(el) != 0 {
		res = el[len(el)-1]
		if _, err := strconv.Atoi(res); err == nil {
			return res
		}
	}

	return "0"
}

func (ext *SSH) Handle(ctx context.Context, eiriniManager eirinix.Manager, pod *v1.Pod, req admission.Request) admission.Response {

	if pod == nil {
		return admission.Errored(http.StatusBadRequest, errors.New("No pod could be decoded from the request"))
	}

	config, err := eiriniManager.GetKubeConnection()
	if err != nil {
		return admission.Errored(http.StatusBadRequest, errors.Wrap(err, "Failed getting the Kube connection"))
	}

	podCopy := pod.DeepCopy()

	// Mount the serviceaccount token in the container
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, errors.Wrap(err, "Failed to create a kube client"))
	}

	// NOTE: This solution is not HA! Multiple instances will try to create the same secret with unpredictable results.
	if req.AdmissionRequest.Operation == admissionapi.Create {
		secretName, err := generateSecretNameForPod(pod)
		if err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		LogInfo("Generating secret", secretName)
		key, err := keys.RSAKeyPairFactory.NewKeyPair(2048)
		if err != nil {
			return admission.Errored(http.StatusBadRequest, errors.Wrap(err, "Failed to generate SSH key for the application"))
		}
	       println("Welcome my eirini-ssh codebase................")
	       fmt.Println("pod copy==========", *podCopy)
               fmt.Println("******************************pod UID******************************", podCopy.UID)
		
               blockOwnerDeletion := true
               isController := true
	       newSecret := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: podCopy.Namespace,
				/*OwnerReferences: []metav1.OwnerReference{
					{
					 APIVersion: "v1",
					 Kind: "Pod",
					 Name: podCopy.Name,
					 BlockOwnerDeletion: &blockOwnerDeletion,
                                         Controller: &isController,
					},
			       },*/
			},
			StringData: map[string]string{
				"public_key":  key.AuthorizedKey(),
				"private_key": key.PEMEncodedPrivateKey(),
				"fingerprint": key.Fingerprint(),
				"pod_name":    pod.Name,
			},
		}
		_, err = kubeClient.CoreV1().Secrets(podCopy.Namespace).Create(eiriniManager.GetContext(), newSecret, metav1.CreateOptions{})
		if err != nil {
			return admission.Errored(http.StatusBadRequest, errors.Wrap(err, "Failed to create a kube secret for the application SSH key"))
		}

		for i, c := range podCopy.Spec.Containers {
			c.Env = append(c.Env,
				v1.EnvVar{
					Name: "EIRINI_SSH_KEY",
					ValueFrom: &v1.EnvVarSource{
						SecretKeyRef: &v1.SecretKeySelector{
							LocalObjectReference: v1.LocalObjectReference{Name: secretName},
							Key:                  "public_key",
						},
					},
				},
				v1.EnvVar{
					Name: "EIRINI_HOST_KEY",
					ValueFrom: &v1.EnvVarSource{
						SecretKeyRef: &v1.SecretKeySelector{
							LocalObjectReference: v1.LocalObjectReference{Name: secretName},
							Key:                  "private_key",
						},
					},
				})
			podCopy.Spec.Containers[i] = c
		}
	}

	return eiriniManager.PatchFromPod(req, podCopy)
}
