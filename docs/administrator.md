## Create ConfigMap

ConfigMap is used to store the configuration of the operator

```bash
    $ kubectl create -f manifests/configmap.yaml
```

## Deploying the operator

First you need to install the service account definition in your Minikube cluster.

```bash
    $ kubectl create -f manifests/operator-service-account-rbac.yaml
```

Next deploy the postgres-operator from the docker image Zalando is using:

```bash
    $ kubectl create -f manifests/postgres-operator.yaml
```

If you prefer to build the image yourself follow up down below.

## Check if CustomResourceDefinition has been registered

```bash
    $ kubectl get crd

	NAME                          KIND
	postgresqls.acid.zalan.do     CustomResourceDefinition.v1beta1.apiextensions.k8s.io
```

# How to configure PostgreSQL operator

## Select the namespace to deploy to

The operator can run in a namespace other than `default`. For example, to use
the `test` namespace, run the following before deploying the operator's
manifests:

```bash
    $ kubectl create namespace test
    $ kubectl config set-context $(kubectl config current-context) --namespace=test
```

All subsequent `kubectl` commands will work with the `test` namespace. The
operator will run in this namespace and look up needed resources - such as its
config map - there. Please note that the namespace for service accounts and cluster role bindings in [operator RBAC rules](manifests/operator-service-account-rbac.yaml) needs to be adjusted to the non-default value.

## Specify the namespace to watch

Watching a namespace for an operator means tracking requests to change
Postgresql clusters in the namespace such as "increase the number of Postgresql
replicas to 5" and reacting to the requests, in this example by actually
scaling up.

By default, the operator watches the namespace it is deployed to. You can
change this by altering the `WATCHED_NAMESPACE` env var in the operator
deployment manifest or the `watched_namespace` field in the operator configmap.
In the case both are set, the env var takes the precedence. To make the
operator listen to all namespaces, explicitly set the field/env var to "`*`".

Note that for an operator to manage pods in the watched namespace, the
operator's service account (as specified in the operator deployment manifest)
has to have appropriate privileges to access the watched namespace. The
operator may not be able to function in the case it watches all namespaces but
lacks access rights to any of them (except Kubernetes system namespaces like
`kube-system`). The reason is that for multiple namespaces operations such as
'list pods' execute at the cluster scope and fail at the first violation of
access rights.

The watched namespace also needs to have a (possibly different) service account
in the case database pods need to talk to the Kubernetes API (e.g. when using
Kubernetes-native configuration of Patroni). The operator checks that the
`pod_service_account_name` exists in the target namespace, and, if not, deploys
there the `pod_service_account_definition` from the operator
[`Config`](pkg/util/config/config.go) with the default value of:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
 name: operator
```

In this definition, the operator overwrites the account's name to match
`pod_service_account_name` and the `default` namespace to match the target
namespace. The operator  performs **no** further syncing of this account.

## Role-based access control for the operator

The `manifests/operator-service-account-rbac.yaml` defines cluster roles and bindings needed
for the operator to function under access control restrictions. To deploy the
operator with this RBAC policy use:

```bash
    $ kubectl create -f manifests/configmap.yaml
    $ kubectl create -f manifests/operator-service-account-rbac.yaml
    $ kubectl create -f manifests/postgres-operator.yaml
    $ kubectl create -f manifests/minimal-postgres-manifest.yaml
```

Note that the service account in `operator-rbac.yaml` is named
`zalando-postgres-operator`. You may have to change the `service_account_name`
in the operator configmap and `serviceAccountName` in the postgres-operator
deployment appropriately.

This is done intentionally, as to avoid breaking those setups that already work
with the default `operator` account. In the future the operator should ideally
be run under the `zalando-postgres-operator` service account.

The service account defined in  `operator-rbac.yaml` acquires some privileges
not really used by the operator (i.e. we only need list and watch on
configmaps), this is also done intentionally to avoid breaking things if
someone decides to configure the same service account in the operator's
configmap to run postgres clusters.

### Use taints and tolerations for dedicated PostgreSQL nodes

To ensure Postgres pods are running on nodes without any other application
pods, you can use
[taints and tolerations](https://kubernetes.io/docs/concepts/configuration/taint-and-toleration/)
and configure the required toleration in the operator ConfigMap.

As an example you can set following node taint:

```bash
    $ kubectl taint nodes <nodeName> postgres=:NoSchedule
```

And configure the toleration for the PostgreSQL pods by adding following line
to the ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: postgres-operator
data:
  toleration: "key:postgres,operator:Exists,effect:NoSchedule"
  ...
```

## Custom Pod Environment Variables

It is possible to configure a config map which is used by the Postgres pods as
an additional provider for environment variables.

One use case is to customize the Spilo image and configure it with environment
variables. The config map with the additional settings is configured in the
operator's main config map:

**postgres-operator ConfigMap**

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: postgres-operator
data:
  # referencing config map with custom settings
  pod_environment_configmap: postgres-pod-config
  ...
```

**referenced ConfigMap `postgres-pod-config`**

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: postgres-pod-config
  namespace: default
data:
  MY_CUSTOM_VAR: value
```

This ConfigMap is then added as a source of environment variables to the
Postgres StatefulSet/pods.

## Limiting the number of instances in clusters with `min_instances` and `max_instances`

As a preventive measure, one can restrict the minimum and the maximum number of
instances permitted by each Postgres cluster managed by the operator. If either
`min_instances` or `max_instances` is set to a non-zero value, the operator may
adjust the number of instances specified in the cluster manifest to match
either the min or the max boundary. For instance, of a cluster manifest has 1
instance and the min_instances is set to 3, the cluster will be created with 3
instances. By default, both parameters are set to -1.

## Load balancers

For any Postgresql/Spilo cluster, the operator creates two separate k8s
services: one for the master pod and one for replica pods. To expose these
services to an outer network, one can attach load balancers to them by setting
`enableMasterLoadBalancer` and/or `enableReplicaLoadBalancer` to `true` in the
cluster manifest. In the case any of these variables are omitted from the
manifest, the operator configmap's settings `enable_master_load_balancer` and
`enable_replica_load_balancer` apply. Note that the operator settings affect
all Postgresql services running in all namespaces watched by the operator.

To limit the range of IP adresses that can reach a load balancer, specify desired ranges in the `allowedSourceRanges` field (applies to both master and replica LBs). To prevent exposing LBs to the entire Internet, this field is set at cluster creation time to `127.0.0.1/32` unless overwritten explicitly. If you want to revoke all IP ranges from an existing cluster, please set the `allowedSourceRanges` field to `127.0.0.1/32` or to the empty sequence `[]`. Setting the field to `null` or omitting entirely may lead to k8s removing this field from the manifest due to [the k8s handling of null fields](https://kubernetes.io/docs/concepts/overview/object-management-kubectl/declarative-config/#how-apply-calculates-differences-and-merges-changes). Then the resultant manifest will not have the necessary change, and the operator will respectively do noting with the existing source ranges.

## Running periodic 'autorepair' scans of Kubernetes objects

The Postgres operator periodically scans all Kubernetes objects belonging to
each cluster and repairs all discrepancies between them and the definitions
generated from the current cluster manifest. There are two types of scans: a
`sync scan`, running every `resync_period` seconds for every cluster, and the
`repair scan`, coming every `repair_period` only for those clusters that didn't
report success as a result of the last operation applied to them.

## Postgres roles supported by the operator

The operator is capable of maintaining roles of multiple kinds within a Postgres database cluster:

1. **System roles** are roles necessary for the proper work of Postgres itself such as a replication role or the initial superuser role. The operator delegates creating such roles to Patroni and only establishes relevant secrets.

2. **Infrastructure roles** are roles for processes originating from external systems, e.g. monitoring robots. The operator creates such roles in all PG clusters it manages assuming k8s secrets with the relevant credentials exist beforehand.

3. **Per-cluster robot users** are also roles for processes originating from external systems but defined for an individual Postgres cluster in its manifest. A typical example is a role for connections from an application that uses the database.

4. **Human users** originate from the Teams API that returns list of the team members given a team id. Operator differentiates between (a) product teams that own a particular Postgres cluster and are granted admin rights to maintain it, and (b) Postgres superuser teams that get the superuser access to all PG databases running in a k8s cluster for the purposes of maintaining and troubleshooting.

## Understanding rolling update of Spilo pods

The operator logs reasons for a rolling update with the `info` level and a diff between the old and new StatefulSet specs with the `debug` level. To benefit from numerous escape characters in the latter log entry, view it in CLI with `echo -e`. Note that the resultant message will contain some noise because the `PodTemplate` used by the operator is yet to be updated with the default values used internally in Kubernetes.
