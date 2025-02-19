#!/bin/bash

# This scripts expects the following variables to be set:
# CLUSTER_NUMBER        -> the number of liqo clusters
# K8S_VERSION           -> the Kubernetes version
# CNI                   -> the CNI plugin used
# TMPDIR                -> the directory where the test-related files are stored
# BINDIR                -> the directory where the test-related binaries are stored
# TEMPLATE_DIR          -> the directory where to read the cluster templates
# NAMESPACE             -> the namespace where liqo is running
# KUBECONFIGDIR         -> the directory where the kubeconfigs are stored
# LIQO_VERSION          -> the liqo version to test
# INFRA                 -> the Kubernetes provider for the infrastructure
# LIQOCTL               -> the path where liqoctl is stored
# POD_CIDR_OVERLAPPING  -> the pod CIDR of the clusters is overlapping
# CLUSTER_TEMPLATE_FILE -> the file where the cluster template is stored

set -e           # Fail in case of error
set -o nounset   # Fail if undefined variables are used
set -o pipefail  # Fail if one of the piped commands fails

error() {
   local sourcefile=$1
   local lineno=$2
   echo "An error occurred at $sourcefile:$lineno."
}
trap 'error "${BASH_SOURCE}" "${LINENO}"' ERR

CLUSTER_NAME=cluster

export K8S_VERSION=${K8S_VERSION:-"1.25.5"}
K8S_VERSION=$(echo -n "$K8S_VERSION" | sed 's/v//g') # remove the leading v

export CRI_PATH="/var/run/containerd/containerd.sock"
export NODE_VM_IMAGE_TEMPLATE="quay.io/capk/ubuntu-2004-container-disk:v${K8S_VERSION}"
export IMAGE_REPO=k8s.gcr.io

export SERVICE_CIDR=10.100.0.0/16
export POD_CIDR=10.200.0.0/16
export POD_CIDR_OVERLAPPING=${POD_CIDR_OVERLAPPING:-"false"}

TARGET_NAMESPACE="liqo-ci"

RUNNER_NAME=${RUNNER_NAME:-"test"}
CAPI_CLUSTER_NAME="${RUNNER_NAME}-${CLUSTER_NAME}"

for i in $(seq 1 "${CLUSTER_NUMBER}");
do
	if [[ ${POD_CIDR_OVERLAPPING} != "true" ]]; then
		# this should avoid the ipam to reserve a pod CIDR of another cluster as local external CIDR causing remapping
		export POD_CIDR="10.$((i * 10)).0.0/16"
	fi
	echo "Creating cluster ${CLUSTER_NAME}${i}"
  POD_CIDR_ESC_1=$(echo $POD_CIDR | cut -d'/' -f1)
  POD_CIDR_ESC_2=$(echo $POD_CIDR | cut -d'/' -f2)
  POD_CIDR_ESC="${POD_CIDR_ESC_1}\/${POD_CIDR_ESC_2}"
  clusterctl generate cluster "${CAPI_CLUSTER_NAME}${i}" \
    --kubernetes-version "$K8S_VERSION" \
    --control-plane-machine-count 1 \
    --worker-machine-count 2 \
    --target-namespace "$TARGET_NAMESPACE" \
    --infrastructure kubevirt | sed "s/10.243.0.0\/16/$POD_CIDR_ESC/g" | kubectl apply -f -
done

for i in $(seq 1 "${CLUSTER_NUMBER}");
do
  if [[ ${POD_CIDR_OVERLAPPING} != "true" ]]; then
		# this should avoid the ipam to reserve a pod CIDR of another cluster as local external CIDR causing remapping
		export POD_CIDR="10.$((i * 10)).0.0/16"
	fi
  echo "Waiting for cluster ${CLUSTER_NAME}${i} to be ready"
  kubectl wait --for condition=Ready=true -n "$TARGET_NAMESPACE" "clusters.cluster.x-k8s.io/${CAPI_CLUSTER_NAME}${i}" --timeout=-1s

  echo "Getting kubeconfig for cluster ${CLUSTER_NAME}${i}"
  mkdir -p "${TMPDIR}/kubeconfigs"
  clusterctl get kubeconfig -n "$TARGET_NAMESPACE" "${CAPI_CLUSTER_NAME}${i}" > "${TMPDIR}/kubeconfigs/liqo_kubeconf_${i}"

  # install calico
  curl https://raw.githubusercontent.com/projectcalico/calico/v3.24.4/manifests/calico.yaml \
    | sed -E 's|^( +)# (- name: CALICO_IPV4POOL_CIDR)$|\1\2|g;'\
"s|^( +)# (  value: )\"192.168.0.0/16\"|\1\2\"$POD_CIDR\"|g;"\
'/- name: CLUSTER_TYPE/{ n; s/( +value: ").+/\1k8s"/g };'\
'/- name: CALICO_IPV4POOL_IPIP/{ n; s/value: "Always"/value: "Never"/ };'\
'/- name: CALICO_IPV4POOL_VXLAN/{ n; s/value: "Never"/value: "Always"/};'\
'/# Set Felix endpoint to host default action to ACCEPT./a\            - name: FELIX_VXLANPORT\n              value: "6789"' \
    | kubectl apply -f - --kubeconfig "${TMPDIR}/kubeconfigs/liqo_kubeconf_${i}"

  # install local-path storage class
  kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.24/deploy/local-path-storage.yaml --kubeconfig "${TMPDIR}/kubeconfigs/liqo_kubeconf_${i}"
  kubectl annotate storageclass local-path storageclass.kubernetes.io/is-default-class=true --kubeconfig "${TMPDIR}/kubeconfigs/liqo_kubeconf_${i}"
done
