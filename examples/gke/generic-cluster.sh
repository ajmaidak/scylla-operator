#!/bin/bash

GCP_PROJECT=cass-mvp
GCP_REGION=us-east4
GCP_ZONE=us-east4-a
CLUSTER_NAME=scylla-generic

gcloud container --project "${GCP_PROJECT}" \
clusters create "${CLUSTER_NAME}" \
--no-enable-basic-auth \
--enable-ip-alias \
--network "projects/cass-mvp/global/networks/cas-network-1" \
--subnetwork "projects/cass-mvp/regions/us-east4/subnetworks/cas-us-east4" \
--zone "${GCP_ZONE}" \
--cluster-version "${CLUSTER_VERSION}" \
--node-version "${CLUSTER_VERSION}" \
--machine-type "n1-standard-32" \
--num-nodes "4" \
--disk-type "pd-ssd" --disk-size "20" \
--node-taints role=scylla-clusters:NoSchedule \
--enable-cloud-logging --enable-stackdriver-kubernetes \
--no-enable-autoupgrade --no-enable-autorepair

# skinny nodes
gcloud container --project "${GCP_PROJECT}" \
node-pools create "skinny-nodes" \
--cluster "${CLUSTER_NAME}" \
--zone "${GCP_ZONE}" \
--node-version "${CLUSTER_VERSION}" \
--machine-type "n1-standard-16" \
--num-nodes "4" \
--disk-type "pd-ssd" --disk-size "20" \
--node-taints role=scylla-clusters:NoSchedule \
--no-enable-autoupgrade --no-enable-autorepair

# runs the operator
gcloud container --project "${GCP_PROJECT}" \
node-pools create "operator-pool" \
--cluster "${CLUSTER_NAME}" \
--zone "${GCP_ZONE}" \
--node-version "${CLUSTER_VERSION}" \
--machine-type "n1-standard-8" \
--num-nodes "1" \
--disk-type "pd-ssd" --disk-size "20" \
--no-enable-autoupgrade --no-enable-autorepair

# one-ssd nodepool
gcloud container --project "${GCP_PROJECT}" \
node-pools create "scylla-one-ssd" \
--cluster "${CLUSTER_NAME}" \
--zone "${GCP_ZONE}" \
--machine-type "n1-standard-16" \
--num-nodes "4" \
--disk-type "pd-ssd" --disk-size "20" \
--local-ssd-count "1" \
--node-taints role=scylla-clusters-onessd:NoSchedule \
--no-enable-autoupgrade --no-enable-autorepair

#gcloud beta container --project "cass-mvp" clusters create "cluster-1" --zone "us-central1-c" --no-enable-basic-auth --cluster-version "1.16.15-gke.4300" --release-channel "None" --machine-type "e2-medium" --image-type "COS" --disk-type "pd-standard" --disk-size "100" --metadata disable-legacy-endpoints=true --scopes "https://www.googleapis.com/auth/devstorage.read_only","https://www.googleapis.com/auth/logging.write","https://www.googleapis.com/auth/monitoring","https://www.googleapis.com/auth/servicecontrol","https://www.googleapis.com/auth/service.management.readonly","https://www.googleapis.com/auth/trace.append" --num-nodes "3" --enable-stackdriver-kubernetes --enable-ip-alias --network "projects/cass-mvp/global/networks/default" --subnetwork "projects/cass-mvp/regions/us-central1/subnetworks/default" --default-max-pods-per-node "110" --no-enable-master-authorized-networks --addons HorizontalPodAutoscaling,HttpLoadBalancing --enable-autoupgrade --enable-autorepair --max-surge-upgrade 1 --max-unavailable-upgrade 0

#gcloud beta container --project "cass-mvp" clusters create "cluster-1" --zone "us-east4-a" --no-enable-basic-auth --cluster-version "1.16.15-gke.4300" --release-channel "None" --machine-type "e2-medium" --image-type "COS" --disk-type "pd-standard" --disk-size "100" --metadata disable-legacy-endpoints=true --scopes "https://www.googleapis.com/auth/devstorage.read_only","https://www.googleapis.com/auth/logging.write","https://www.googleapis.com/auth/monitoring","https://www.googleapis.com/auth/servicecontrol","https://www.googleapis.com/auth/service.management.readonly","https://www.googleapis.com/auth/trace.append" --num-nodes "3" --enable-stackdriver-kubernetes --enable-ip-alias --network "projects/cass-mvp/global/networks/cas-network-1" --subnetwork "projects/cass-mvp/regions/us-east4/subnetworks/cas-us-east4" --default-max-pods-per-node "110" --no-enable-master-authorized-networks --addons HorizontalPodAutoscaling,HttpLoadBalancing --enable-autoupgrade --enable-autorepair --max-surge-upgrade 1 --max-unavailable-upgrade 0

