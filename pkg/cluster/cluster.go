package cluster

// Postgres CustomResourceDefinition object i.e. Spilo

import (
	"database/sql"
	"fmt"
	"reflect"
	"regexp"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/api/apps/v1beta1"
	"k8s.io/api/core/v1"
	policybeta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"encoding/json"

	acidv1 "github.com/zalando-incubator/postgres-operator/pkg/apis/acid.zalan.do/v1"
	"github.com/zalando-incubator/postgres-operator/pkg/spec"
	"github.com/zalando-incubator/postgres-operator/pkg/util"
	"github.com/zalando-incubator/postgres-operator/pkg/util/config"
	"github.com/zalando-incubator/postgres-operator/pkg/util/constants"
	"github.com/zalando-incubator/postgres-operator/pkg/util/k8sutil"
	"github.com/zalando-incubator/postgres-operator/pkg/util/patroni"
	"github.com/zalando-incubator/postgres-operator/pkg/util/teams"
	"github.com/zalando-incubator/postgres-operator/pkg/util/users"
	rbacv1beta1 "k8s.io/api/rbac/v1beta1"
)

var (
	alphaNumericRegexp    = regexp.MustCompile("^[a-zA-Z][a-zA-Z0-9]*$")
	databaseNameRegexp    = regexp.MustCompile("^[a-zA-Z_][a-zA-Z0-9_]*$")
	userRegexp            = regexp.MustCompile(`^[a-z0-9]([-_a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-_a-z0-9]*[a-z0-9])?)*$`)
	patroniObjectSuffixes = []string{"config", "failover", "sync"}
)

// Config contains operator-wide clients and configuration used from a cluster. TODO: remove struct duplication.
type Config struct {
	OpConfig                     config.Config
	RestConfig                   *rest.Config
	InfrastructureRoles          map[string]spec.PgUser // inherited from the controller
	PodServiceAccount            *v1.ServiceAccount
	PodServiceAccountRoleBinding *rbacv1beta1.RoleBinding
}

type kubeResources struct {
	Services            map[PostgresRole]*v1.Service
	Endpoints           map[PostgresRole]*v1.Endpoints
	Secrets             map[types.UID]*v1.Secret
	Statefulset         *v1beta1.StatefulSet
	PodDisruptionBudget *policybeta1.PodDisruptionBudget
	//Pods are treated separately
	//PVCs are treated separately
}

// Cluster describes postgresql cluster
type Cluster struct {
	kubeResources
	acidv1.Postgresql
	Config
	logger           *logrus.Entry
	patroni          patroni.Interface
	pgUsers          map[string]spec.PgUser
	systemUsers      map[string]spec.PgUser
	podSubscribers   map[spec.NamespacedName]chan PodEvent
	podSubscribersMu sync.RWMutex
	pgDb             *sql.DB
	mu               sync.Mutex
	userSyncStrategy spec.UserSyncer
	deleteOptions    *metav1.DeleteOptions
	podEventsQueue   *cache.FIFO

	teamsAPIClient   teams.Interface
	oauthTokenGetter OAuthTokenGetter
	KubeClient       k8sutil.KubernetesClient //TODO: move clients to the better place?
	currentProcess   Process
	processMu        sync.RWMutex // protects the current operation for reporting, no need to hold the master mutex
	specMu           sync.RWMutex // protects the spec for reporting, no need to hold the master mutex
}

type compareStatefulsetResult struct {
	match         bool
	replace       bool
	rollingUpdate bool
	reasons       []string
}

// New creates a new cluster. This function should be called from a controller.
func New(cfg Config, kubeClient k8sutil.KubernetesClient, pgSpec acidv1.Postgresql, logger *logrus.Entry) *Cluster {
	deletePropagationPolicy := metav1.DeletePropagationOrphan

	podEventsQueue := cache.NewFIFO(func(obj interface{}) (string, error) {
		e, ok := obj.(PodEvent)
		if !ok {
			return "", fmt.Errorf("could not cast to PodEvent")
		}

		return fmt.Sprintf("%s-%s", e.PodName, e.ResourceVersion), nil
	})

	cluster := &Cluster{
		Config:         cfg,
		Postgresql:     pgSpec,
		pgUsers:        make(map[string]spec.PgUser),
		systemUsers:    make(map[string]spec.PgUser),
		podSubscribers: make(map[spec.NamespacedName]chan PodEvent),
		kubeResources: kubeResources{
			Secrets:   make(map[types.UID]*v1.Secret),
			Services:  make(map[PostgresRole]*v1.Service),
			Endpoints: make(map[PostgresRole]*v1.Endpoints)},
		userSyncStrategy: users.DefaultUserSyncStrategy{},
		deleteOptions:    &metav1.DeleteOptions{PropagationPolicy: &deletePropagationPolicy},
		podEventsQueue:   podEventsQueue,
		KubeClient:       kubeClient,
	}
	cluster.logger = logger.WithField("pkg", "cluster").WithField("cluster-name", cluster.clusterName())
	cluster.teamsAPIClient = teams.NewTeamsAPI(cfg.OpConfig.TeamsAPIUrl, logger)
	cluster.oauthTokenGetter = newSecretOauthTokenGetter(&kubeClient, cfg.OpConfig.OAuthTokenSecretName)
	cluster.patroni = patroni.New(cluster.logger)

	return cluster
}

func (c *Cluster) clusterName() spec.NamespacedName {
	return util.NameFromMeta(c.ObjectMeta)
}

func (c *Cluster) clusterNamespace() string {
	return c.ObjectMeta.Namespace
}

func (c *Cluster) teamName() string {
	// TODO: check Teams API for the actual name (in case the user passes an integer Id).
	return c.Spec.TeamID
}

func (c *Cluster) setProcessName(procName string, args ...interface{}) {
	c.processMu.Lock()
	defer c.processMu.Unlock()
	c.currentProcess = Process{
		Name:      fmt.Sprintf(procName, args...),
		StartTime: time.Now(),
	}
}

func (c *Cluster) setStatus(status acidv1.PostgresStatus) {
	// TODO: eventually switch to updateStatus() for kubernetes 1.11 and above
	var (
		err error
		b   []byte
	)
	if b, err = json.Marshal(status); err != nil {
		c.logger.Errorf("could not marshal status: %v", err)
	}

	patch := []byte(fmt.Sprintf(`{"status": %s}`, string(b)))
	// we cannot do a full scale update here without fetching the previous manifest (as the resourceVersion may differ),
	// however, we could do patch without it. In the future, once /status subresource is there (starting Kubernets 1.11)
	// we should take advantage of it.
	newspec, err := c.KubeClient.AcidV1ClientSet.AcidV1().Postgresqls(c.clusterNamespace()).Patch(c.Name, types.MergePatchType, patch)
	if err != nil {
		c.logger.Errorf("could not update status: %v", err)
	}
	// update the spec, maintaining the new resourceVersion.
	c.setSpec(newspec)
}

func (c *Cluster) isNewCluster() bool {
	return c.Status == acidv1.ClusterStatusCreating
}

// initUsers populates c.systemUsers and c.pgUsers maps.
func (c *Cluster) initUsers() error {
	c.setProcessName("initializing users")

	// clear our the previous state of the cluster users (in case we are running a sync).
	c.systemUsers = map[string]spec.PgUser{}
	c.pgUsers = map[string]spec.PgUser{}

	c.initSystemUsers()

	if err := c.initInfrastructureRoles(); err != nil {
		return fmt.Errorf("could not init infrastructure roles: %v", err)
	}

	if err := c.initRobotUsers(); err != nil {
		return fmt.Errorf("could not init robot users: %v", err)
	}

	if err := c.initHumanUsers(); err != nil {
		return fmt.Errorf("could not init human users: %v", err)
	}

	return nil
}

// Create creates the new kubernetes objects associated with the cluster.
func (c *Cluster) Create() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var (
		err error

		service *v1.Service
		ep      *v1.Endpoints
		ss      *v1beta1.StatefulSet
	)

	defer func() {
		if err == nil {
			c.setStatus(acidv1.ClusterStatusRunning) //TODO: are you sure it's running?
		} else {
			c.setStatus(acidv1.ClusterStatusAddFailed)
		}
	}()

	c.setStatus(acidv1.ClusterStatusCreating)

	for _, role := range []PostgresRole{Master, Replica} {

		if c.Endpoints[role] != nil {
			return fmt.Errorf("%s endpoint already exists in the cluster", role)
		}
		if role == Master {
			// replica endpoint will be created by the replica service. Master endpoint needs to be created by us,
			// since the corresponding master service doesn't define any selectors.
			ep, err = c.createEndpoint(role)
			if err != nil {
				return fmt.Errorf("could not create %s endpoint: %v", role, err)
			}
			c.logger.Infof("endpoint %q has been successfully created", util.NameFromMeta(ep.ObjectMeta))
		}

		if c.Services[role] != nil {
			return fmt.Errorf("service already exists in the cluster")
		}
		service, err = c.createService(role)
		if err != nil {
			return fmt.Errorf("could not create %s service: %v", role, err)
		}
		c.logger.Infof("%s service %q has been successfully created", role, util.NameFromMeta(service.ObjectMeta))
	}

	if err = c.initUsers(); err != nil {
		return err
	}
	c.logger.Infof("users have been initialized")

	if err = c.syncSecrets(); err != nil {
		return fmt.Errorf("could not create secrets: %v", err)
	}
	c.logger.Infof("secrets have been successfully created")

	if c.PodDisruptionBudget != nil {
		return fmt.Errorf("pod disruption budget already exists in the cluster")
	}
	pdb, err := c.createPodDisruptionBudget()
	if err != nil {
		return fmt.Errorf("could not create pod disruption budget: %v", err)
	}
	c.logger.Infof("pod disruption budget %q has been successfully created", util.NameFromMeta(pdb.ObjectMeta))

	if c.Statefulset != nil {
		return fmt.Errorf("statefulset already exists in the cluster")
	}
	ss, err = c.createStatefulSet()
	if err != nil {
		return fmt.Errorf("could not create statefulset: %v", err)
	}
	c.logger.Infof("statefulset %q has been successfully created", util.NameFromMeta(ss.ObjectMeta))

	c.logger.Info("waiting for the cluster being ready")

	if err = c.waitStatefulsetPodsReady(); err != nil {
		c.logger.Errorf("failed to create cluster: %v", err)
		return err
	}
	c.logger.Infof("pods are ready")

	// create database objects unless we are running without pods or disabled that feature explicitly
	if !(c.databaseAccessDisabled() || c.getNumberOfInstances(&c.Spec) <= 0) {
		if err = c.createRoles(); err != nil {
			return fmt.Errorf("could not create users: %v", err)
		}
		c.logger.Infof("users have been successfully created")

		if err = c.syncDatabases(); err != nil {
			return fmt.Errorf("could not sync databases: %v", err)
		}
		c.logger.Infof("databases have been successfully created")
	}

	if err := c.listResources(); err != nil {
		c.logger.Errorf("could not list resources: %v", err)
	}

	return nil
}

func (c *Cluster) compareStatefulSetWith(statefulSet *v1beta1.StatefulSet) *compareStatefulsetResult {
	reasons := make([]string, 0)
	var match, needsRollUpdate, needsReplace bool

	match = true
	//TODO: improve me
	if *c.Statefulset.Spec.Replicas != *statefulSet.Spec.Replicas {
		match = false
		reasons = append(reasons, "new statefulset's number of replicas doesn't match the current one")
	}
	if !reflect.DeepEqual(c.Statefulset.Annotations, statefulSet.Annotations) {
		match = false
		reasons = append(reasons, "new statefulset's annotations doesn't match the current one")
	}
	if len(c.Statefulset.Spec.Template.Spec.Containers) != len(statefulSet.Spec.Template.Spec.Containers) {
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's container specification doesn't match the current one")
	} else {
		var containerReasons []string
		needsRollUpdate, containerReasons = c.compareContainers(c.Statefulset, statefulSet)
		reasons = append(reasons, containerReasons...)
	}
	if len(c.Statefulset.Spec.Template.Spec.Containers) == 0 {
		c.logger.Warningf("statefulset %q has no container", util.NameFromMeta(c.Statefulset.ObjectMeta))
		return &compareStatefulsetResult{}
	}
	// In the comparisons below, the needsReplace and needsRollUpdate flags are never reset, since checks fall through
	// and the combined effect of all the changes should be applied.
	// TODO: make sure this is in sync with generatePodTemplate, ideally by using the same list of fields to generate
	// the template and the diff
	if c.Statefulset.Spec.Template.Spec.ServiceAccountName != statefulSet.Spec.Template.Spec.ServiceAccountName {
		needsReplace = true
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's serviceAccountName service asccount name doesn't match the current one")
	}
	if *c.Statefulset.Spec.Template.Spec.TerminationGracePeriodSeconds != *statefulSet.Spec.Template.Spec.TerminationGracePeriodSeconds {
		needsReplace = true
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's terminationGracePeriodSeconds doesn't match the current one")
	}
	if !reflect.DeepEqual(c.Statefulset.Spec.Template.Spec.Affinity, statefulSet.Spec.Template.Spec.Affinity) {
		needsReplace = true
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's pod affinity doesn't match the current one")
	}

	// Some generated fields like creationTimestamp make it not possible to use DeepCompare on Spec.Template.ObjectMeta
	if !reflect.DeepEqual(c.Statefulset.Spec.Template.Labels, statefulSet.Spec.Template.Labels) {
		needsReplace = true
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's metadata labels doesn't match the current one")
	}
	if (c.Statefulset.Spec.Selector != nil) && (statefulSet.Spec.Selector != nil) {
		if !reflect.DeepEqual(c.Statefulset.Spec.Selector.MatchLabels, statefulSet.Spec.Selector.MatchLabels) {
			// forbid introducing new labels in the selector on the new statefulset, as it would cripple replacements
			// due to the fact that the new statefulset won't be able to pick up old pods with non-matching labels.
			if !util.MapContains(c.Statefulset.Spec.Selector.MatchLabels, statefulSet.Spec.Selector.MatchLabels) {
				c.logger.Warningf("new statefulset introduces extra labels in the label selector, cannot continue")
				return &compareStatefulsetResult{}
			}
			needsReplace = true
			reasons = append(reasons, "new statefulset's selector doesn't match the current one")
		}
	}

	if !reflect.DeepEqual(c.Statefulset.Spec.Template.Annotations, statefulSet.Spec.Template.Annotations) {
		match = false
		needsReplace = true
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's pod template metadata annotations doesn't match the current one")
	}
	if len(c.Statefulset.Spec.VolumeClaimTemplates) != len(statefulSet.Spec.VolumeClaimTemplates) {
		needsReplace = true
		reasons = append(reasons, "new statefulset's volumeClaimTemplates contains different number of volumes to the old one")
	}
	for i := 0; i < len(c.Statefulset.Spec.VolumeClaimTemplates); i++ {
		name := c.Statefulset.Spec.VolumeClaimTemplates[i].Name
		// Some generated fields like creationTimestamp make it not possible to use DeepCompare on ObjectMeta
		if name != statefulSet.Spec.VolumeClaimTemplates[i].Name {
			needsReplace = true
			reasons = append(reasons, fmt.Sprintf("new statefulset's name for volume %d doesn't match the current one", i))
			continue
		}
		if !reflect.DeepEqual(c.Statefulset.Spec.VolumeClaimTemplates[i].Annotations, statefulSet.Spec.VolumeClaimTemplates[i].Annotations) {
			needsReplace = true
			reasons = append(reasons, fmt.Sprintf("new statefulset's annotations for volume %q doesn't match the current one", name))
		}
		if !reflect.DeepEqual(c.Statefulset.Spec.VolumeClaimTemplates[i].Spec, statefulSet.Spec.VolumeClaimTemplates[i].Spec) {
			name := c.Statefulset.Spec.VolumeClaimTemplates[i].Name
			needsReplace = true
			reasons = append(reasons, fmt.Sprintf("new statefulset's volumeClaimTemplates specification for volume %q doesn't match the current one", name))
		}
	}

	if needsRollUpdate || needsReplace {
		match = false
	}

	return &compareStatefulsetResult{match: match, reasons: reasons, rollingUpdate: needsRollUpdate, replace: needsReplace}
}

type containerCondition func(a, b v1.Container) bool

type containerCheck struct {
	condition containerCondition
	reason    string
}

func newCheck(msg string, cond containerCondition) containerCheck {
	return containerCheck{reason: msg, condition: cond}
}

// compareContainers: compare containers from two stateful sets
// and return:
// * whether or not a rolling update is needed
// * a list of reasons in a human readable format
func (c *Cluster) compareContainers(setA, setB *v1beta1.StatefulSet) (bool, []string) {
	reasons := make([]string, 0)
	needsRollUpdate := false
	checks := []containerCheck{
		newCheck("new statefulset's container %s (index %d) name doesn't match the current one",
			func(a, b v1.Container) bool { return a.Name != b.Name }),
		newCheck("new statefulset's container %s (index %d) image doesn't match the current one",
			func(a, b v1.Container) bool { return a.Image != b.Image }),
		newCheck("new statefulset's container %s (index %d) ports don't match the current one",
			func(a, b v1.Container) bool { return !reflect.DeepEqual(a.Ports, b.Ports) }),
		newCheck("new statefulset's container %s (index %d) resources don't match the current ones",
			func(a, b v1.Container) bool { return !compareResources(&a.Resources, &b.Resources) }),
		newCheck("new statefulset's container %s (index %d) environment doesn't match the current one",
			func(a, b v1.Container) bool { return !reflect.DeepEqual(a.Env, b.Env) }),
		newCheck("new statefulset's container %s (index %d) environment sources don't match the current one",
			func(a, b v1.Container) bool { return !reflect.DeepEqual(a.EnvFrom, b.EnvFrom) }),
	}

	for index, containerA := range setA.Spec.Template.Spec.Containers {
		containerB := setB.Spec.Template.Spec.Containers[index]
		for _, check := range checks {
			if check.condition(containerA, containerB) {
				needsRollUpdate = true
				reasons = append(reasons, fmt.Sprintf(check.reason, containerA.Name, index))
			}
		}
	}

	return needsRollUpdate, reasons
}

func compareResources(a *v1.ResourceRequirements, b *v1.ResourceRequirements) bool {
	equal := true
	if a != nil {
		equal = compareResoucesAssumeFirstNotNil(a, b)
	}
	if equal && (b != nil) {
		equal = compareResoucesAssumeFirstNotNil(b, a)
	}

	return equal
}

func compareResoucesAssumeFirstNotNil(a *v1.ResourceRequirements, b *v1.ResourceRequirements) bool {
	if b == nil || (len(b.Requests) == 0) {
		return len(a.Requests) == 0
	}
	for k, v := range a.Requests {
		if (&v).Cmp(b.Requests[k]) != 0 {
			return false
		}
	}
	for k, v := range a.Limits {
		if (&v).Cmp(b.Limits[k]) != 0 {
			return false
		}
	}
	return true

}

// Update changes Kubernetes objects according to the new specification. Unlike the sync case, the missing object.
// (i.e. service) is treated as an error.
func (c *Cluster) Update(oldSpec, newSpec *acidv1.Postgresql) error {
	updateFailed := false

	c.mu.Lock()
	defer c.mu.Unlock()

	c.setStatus(acidv1.ClusterStatusUpdating)
	c.setSpec(newSpec)

	defer func() {
		if updateFailed {
			c.setStatus(acidv1.ClusterStatusUpdateFailed)
		} else if c.Status != acidv1.ClusterStatusRunning {
			c.setStatus(acidv1.ClusterStatusRunning)
		}
	}()

	if oldSpec.Spec.PgVersion != newSpec.Spec.PgVersion { // PG versions comparison
		c.logger.Warningf("postgresql version change(%q -> %q) has no effect", oldSpec.Spec.PgVersion, newSpec.Spec.PgVersion)
		//we need that hack to generate statefulset with the old version
		newSpec.Spec.PgVersion = oldSpec.Spec.PgVersion
	}

	// Service
	if !reflect.DeepEqual(c.generateService(Master, &oldSpec.Spec), c.generateService(Master, &newSpec.Spec)) ||
		!reflect.DeepEqual(c.generateService(Replica, &oldSpec.Spec), c.generateService(Replica, &newSpec.Spec)) {
		c.logger.Debugf("syncing services")
		if err := c.syncServices(); err != nil {
			c.logger.Errorf("could not sync services: %v", err)
			updateFailed = true
		}
	}

	if !reflect.DeepEqual(oldSpec.Spec.Users, newSpec.Spec.Users) {
		c.logger.Debugf("syncing secrets")
		if err := c.initUsers(); err != nil {
			c.logger.Errorf("could not init users: %v", err)
			updateFailed = true
		}

		c.logger.Debugf("syncing secrets")

		//TODO: mind the secrets of the deleted/new users
		if err := c.syncSecrets(); err != nil {
			c.logger.Errorf("could not sync secrets: %v", err)
			updateFailed = true
		}
	}

	// Volume
	if oldSpec.Spec.Size != newSpec.Spec.Size {
		c.logger.Debugf("syncing persistent volumes")
		c.logVolumeChanges(oldSpec.Spec.Volume, newSpec.Spec.Volume)

		if err := c.syncVolumes(); err != nil {
			c.logger.Errorf("could not sync persistent volumes: %v", err)
			updateFailed = true
		}
	}

	// Statefulset
	func() {
		oldSs, err := c.generateStatefulSet(&oldSpec.Spec)
		if err != nil {
			c.logger.Errorf("could not generate old statefulset spec: %v", err)
			updateFailed = true
			return
		}

		newSs, err := c.generateStatefulSet(&newSpec.Spec)
		if err != nil {
			c.logger.Errorf("could not generate new statefulset spec: %v", err)
			updateFailed = true
			return
		}

		if !reflect.DeepEqual(oldSs, newSs) {
			c.logger.Debugf("syncing statefulsets")
			// TODO: avoid generating the StatefulSet object twice by passing it to syncStatefulSet
			if err := c.syncStatefulSet(); err != nil {
				c.logger.Errorf("could not sync statefulsets: %v", err)
				updateFailed = true
			}
		}
	}()

	// Roles and Databases
	if !(c.databaseAccessDisabled() || c.getNumberOfInstances(&c.Spec) <= 0) {
		c.logger.Debugf("syncing roles")
		if err := c.syncRoles(); err != nil {
			c.logger.Errorf("could not sync roles: %v", err)
			updateFailed = true
		}
		if !reflect.DeepEqual(oldSpec.Spec.Databases, newSpec.Spec.Databases) {
			c.logger.Infof("syncing databases")
			if err := c.syncDatabases(); err != nil {
				c.logger.Errorf("could not sync databases: %v", err)
				updateFailed = true
			}
		}
	}

	return nil
}

// Delete deletes the cluster and cleans up all objects associated with it (including statefulsets).
// The deletion order here is somewhat significant, because Patroni, when running with the Kubernetes
// DCS, reuses the master's endpoint to store the leader related metadata. If we remove the endpoint
// before the pods, it will be re-created by the current master pod and will remain, obstructing the
// creation of the new cluster with the same name. Therefore, the endpoints should be deleted last.
func (c *Cluster) Delete() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.deleteStatefulSet(); err != nil {
		c.logger.Warningf("could not delete statefulset: %v", err)
	}

	for _, obj := range c.Secrets {
		if doDelete, user := c.shouldDeleteSecret(obj); !doDelete {
			c.logger.Warningf("not removing secret %q for the system user %q", obj.GetName(), user)
			continue
		}
		if err := c.deleteSecret(obj); err != nil {
			c.logger.Warningf("could not delete secret: %v", err)
		}
	}

	if err := c.deletePodDisruptionBudget(); err != nil {
		c.logger.Warningf("could not delete pod disruption budget: %v", err)
	}

	for _, role := range []PostgresRole{Master, Replica} {

		if err := c.deleteEndpoint(role); err != nil {
			c.logger.Warningf("could not delete %s endpoint: %v", role, err)
		}

		if err := c.deleteService(role); err != nil {
			c.logger.Warningf("could not delete %s service: %v", role, err)
		}
	}

	if err := c.deletePatroniClusterObjects(); err != nil {
		c.logger.Warningf("could not remove leftover patroni objects; %v", err)
	}
}

//NeedsRepair returns true if the cluster should be included in the repair scan (based on its in-memory status).
func (c *Cluster) NeedsRepair() (bool, acidv1.PostgresStatus) {
	c.specMu.RLock()
	defer c.specMu.RUnlock()
	return !c.Status.Success(), c.Status

}

// ReceivePodEvent is called back by the controller in order to add the cluster's pod event to the queue.
func (c *Cluster) ReceivePodEvent(event PodEvent) {
	if err := c.podEventsQueue.Add(event); err != nil {
		c.logger.Errorf("error when receiving pod events: %v", err)
	}
}

func (c *Cluster) processPodEvent(obj interface{}) error {
	event, ok := obj.(PodEvent)
	if !ok {
		return fmt.Errorf("could not cast to PodEvent")
	}

	c.podSubscribersMu.RLock()
	subscriber, ok := c.podSubscribers[spec.NamespacedName(event.PodName)]
	c.podSubscribersMu.RUnlock()
	if ok {
		subscriber <- event
	}

	return nil
}

// Run starts the pod event dispatching for the given cluster.
func (c *Cluster) Run(stopCh <-chan struct{}) {
	go c.processPodEventQueue(stopCh)
}

func (c *Cluster) processPodEventQueue(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		default:
			if _, err := c.podEventsQueue.Pop(cache.PopProcessFunc(c.processPodEvent)); err != nil {
				c.logger.Errorf("error when processing pod event queue %v", err)
			}
		}
	}
}

func (c *Cluster) initSystemUsers() {
	// We don't actually use that to create users, delegating this
	// task to Patroni. Those definitions are only used to create
	// secrets, therefore, setting flags like SUPERUSER or REPLICATION
	// is not necessary here
	c.systemUsers[constants.SuperuserKeyName] = spec.PgUser{
		Origin:   spec.RoleOriginSystem,
		Name:     c.OpConfig.SuperUsername,
		Password: util.RandomPassword(constants.PasswordLength),
	}
	c.systemUsers[constants.ReplicationUserKeyName] = spec.PgUser{
		Origin:   spec.RoleOriginSystem,
		Name:     c.OpConfig.ReplicationUsername,
		Password: util.RandomPassword(constants.PasswordLength),
	}
}

func (c *Cluster) initRobotUsers() error {
	for username, userFlags := range c.Spec.Users {
		if !isValidUsername(username) {
			return fmt.Errorf("invalid username: %q", username)
		}

		if c.shouldAvoidProtectedOrSystemRole(username, "manifest robot role") {
			continue
		}
		flags, err := normalizeUserFlags(userFlags)
		if err != nil {
			return fmt.Errorf("invalid flags for user %q: %v", username, err)
		}
		newRole := spec.PgUser{
			Origin:   spec.RoleOriginManifest,
			Name:     username,
			Password: util.RandomPassword(constants.PasswordLength),
			Flags:    flags,
		}
		if currentRole, present := c.pgUsers[username]; present {
			c.pgUsers[username] = c.resolveNameConflict(&currentRole, &newRole)
		} else {
			c.pgUsers[username] = newRole
		}
	}
	return nil
}

func (c *Cluster) initTeamMembers(teamID string, isPostgresSuperuserTeam bool) error {
	teamMembers, err := c.getTeamMembers(teamID)

	if err != nil {
		return fmt.Errorf("could not get list of team members for team %q: %v", teamID, err)
	}

	for _, username := range teamMembers {
		flags := []string{constants.RoleFlagLogin}
		memberOf := []string{c.OpConfig.PamRoleName}

		if c.shouldAvoidProtectedOrSystemRole(username, "API role") {
			continue
		}
		if c.OpConfig.EnableTeamSuperuser || isPostgresSuperuserTeam {
			flags = append(flags, constants.RoleFlagSuperuser)
		} else {
			if c.OpConfig.TeamAdminRole != "" {
				memberOf = append(memberOf, c.OpConfig.TeamAdminRole)
			}
		}

		newRole := spec.PgUser{
			Origin:     spec.RoleOriginTeamsAPI,
			Name:       username,
			Flags:      flags,
			MemberOf:   memberOf,
			Parameters: c.OpConfig.TeamAPIRoleConfiguration,
		}

		if currentRole, present := c.pgUsers[username]; present {
			c.pgUsers[username] = c.resolveNameConflict(&currentRole, &newRole)
		} else {
			c.pgUsers[username] = newRole
		}
	}

	return nil
}

func (c *Cluster) initHumanUsers() error {

	var clusterIsOwnedBySuperuserTeam bool

	for _, postgresSuperuserTeam := range c.OpConfig.PostgresSuperuserTeams {
		err := c.initTeamMembers(postgresSuperuserTeam, true)
		if err != nil {
			return fmt.Errorf("Cannot create a team %q of Postgres superusers: %v", postgresSuperuserTeam, err)
		}
		if postgresSuperuserTeam == c.Spec.TeamID {
			clusterIsOwnedBySuperuserTeam = true
		}
	}

	if clusterIsOwnedBySuperuserTeam {
		c.logger.Infof("Team %q owning the cluster is also a team of superusers. Created superuser roles for its members instead of admin roles.", c.Spec.TeamID)
		return nil
	}

	err := c.initTeamMembers(c.Spec.TeamID, false)
	if err != nil {
		return fmt.Errorf("Cannot create a team %q of admins owning the PG cluster: %v", c.Spec.TeamID, err)
	}

	return nil
}

func (c *Cluster) initInfrastructureRoles() error {
	// add infrastructure roles from the operator's definition
	for username, newRole := range c.InfrastructureRoles {
		if !isValidUsername(username) {
			return fmt.Errorf("invalid username: '%v'", username)
		}
		if c.shouldAvoidProtectedOrSystemRole(username, "infrastructure role") {
			continue
		}
		flags, err := normalizeUserFlags(newRole.Flags)
		if err != nil {
			return fmt.Errorf("invalid flags for user '%v': %v", username, err)
		}
		newRole.Flags = flags

		if currentRole, present := c.pgUsers[username]; present {
			c.pgUsers[username] = c.resolveNameConflict(&currentRole, &newRole)
		} else {
			c.pgUsers[username] = newRole
		}
	}
	return nil
}

// resolves naming conflicts between existing and new roles by chosing either of them.
func (c *Cluster) resolveNameConflict(currentRole, newRole *spec.PgUser) spec.PgUser {
	var result spec.PgUser
	if newRole.Origin >= currentRole.Origin {
		result = *newRole
	} else {
		result = *currentRole
	}
	c.logger.Debugf("resolved a conflict of role %q between %s and %s to %s",
		newRole.Name, newRole.Origin, currentRole.Origin, result.Origin)
	return result
}

func (c *Cluster) shouldAvoidProtectedOrSystemRole(username, purpose string) bool {
	if c.isProtectedUsername(username) {
		c.logger.Warnf("cannot initialize a new %s with the name of the protected user %q", purpose, username)
		return true
	}
	if c.isSystemUsername(username) {
		c.logger.Warnf("cannot initialize a new %s with the name of the system user %q", purpose, username)
		return true
	}
	return false
}

// GetCurrentProcess provides name of the last process of the cluster
func (c *Cluster) GetCurrentProcess() Process {
	c.processMu.RLock()
	defer c.processMu.RUnlock()

	return c.currentProcess
}

// GetStatus provides status of the cluster
func (c *Cluster) GetStatus() *ClusterStatus {
	return &ClusterStatus{
		Cluster: c.Spec.ClusterName,
		Team:    c.Spec.TeamID,
		Status:  c.Status,
		Spec:    c.Spec,

		MasterService:       c.GetServiceMaster(),
		ReplicaService:      c.GetServiceReplica(),
		MasterEndpoint:      c.GetEndpointMaster(),
		ReplicaEndpoint:     c.GetEndpointReplica(),
		StatefulSet:         c.GetStatefulSet(),
		PodDisruptionBudget: c.GetPodDisruptionBudget(),
		CurrentProcess:      c.GetCurrentProcess(),

		Error: fmt.Errorf("error: %s", c.Error),
	}
}

// Switchover does a switchover (via Patroni) to a candidate pod
func (c *Cluster) Switchover(curMaster *v1.Pod, candidate spec.NamespacedName) error {

	var err error
	c.logger.Debugf("failing over from %q to %q", curMaster.Name, candidate)

	var wg sync.WaitGroup

	podLabelErr := make(chan error)
	stopCh := make(chan struct{})

	wg.Add(1)

	go func() {
		defer wg.Done()
		ch := c.registerPodSubscriber(candidate)
		defer c.unregisterPodSubscriber(candidate)

		role := Master

		select {
		case <-stopCh:
		case podLabelErr <- func() (err2 error) {
			_, err2 = c.waitForPodLabel(ch, stopCh, &role)
			return
		}():
		}
	}()

	if err = c.patroni.Switchover(curMaster, candidate.Name); err == nil {
		c.logger.Debugf("successfully failed over from %q to %q", curMaster.Name, candidate)
		if err = <-podLabelErr; err != nil {
			err = fmt.Errorf("could not get master pod label: %v", err)
		}
	} else {
		err = fmt.Errorf("could not failover: %v", err)
	}

	// signal the role label waiting goroutine to close the shop and go home
	close(stopCh)
	// wait until the goroutine terminates, since unregisterPodSubscriber
	// must be called before the outer return; otherwsise we risk subscribing to the same pod twice.
	wg.Wait()
	// close the label waiting channel no sooner than the waiting goroutine terminates.
	close(podLabelErr)

	return err

}

// Lock locks the cluster
func (c *Cluster) Lock() {
	c.mu.Lock()
}

// Unlock unlocks the cluster
func (c *Cluster) Unlock() {
	c.mu.Unlock()
}

func (c *Cluster) shouldDeleteSecret(secret *v1.Secret) (delete bool, userName string) {
	secretUser := string(secret.Data["username"])
	return (secretUser != c.OpConfig.ReplicationUsername && secretUser != c.OpConfig.SuperUsername), secretUser
}

type simpleActionWithResult func() error

type clusterObjectGet func(name string) (spec.NamespacedName, error)

type clusterObjectDelete func(name string) error

func (c *Cluster) deletePatroniClusterObjects() error {
	// TODO: figure out how to remove leftover patroni objects in other cases
	if !c.patroniUsesKubernetes() {
		c.logger.Infof("not cleaning up Etcd Patroni objects on cluster delete")
	}
	c.logger.Debugf("removing leftover Patroni objects (endpoints or configmaps)")
	for _, deleter := range []simpleActionWithResult{c.deletePatroniClusterEndpoints, c.deletePatroniClusterConfigMaps} {
		if err := deleter(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cluster) deleteClusterObject(
	get clusterObjectGet,
	del clusterObjectDelete,
	objType string) error {
	for _, suffix := range patroniObjectSuffixes {
		name := fmt.Sprintf("%s-%s", c.Name, suffix)

		if namespacedName, err := get(name); err == nil {
			c.logger.Debugf("deleting Patroni cluster object %q with name %q",
				objType, namespacedName)

			if err = del(name); err != nil {
				return fmt.Errorf("could not Patroni delete cluster object %q with name %q: %v",
					objType, namespacedName, err)
			}

		} else if !k8sutil.ResourceNotFound(err) {
			return fmt.Errorf("could not fetch Patroni Endpoint %q: %v",
				namespacedName, err)
		}
	}
	return nil
}

func (c *Cluster) deletePatroniClusterEndpoints() error {
	get := func(name string) (spec.NamespacedName, error) {
		ep, err := c.KubeClient.Endpoints(c.Namespace).Get(name, metav1.GetOptions{})
		return util.NameFromMeta(ep.ObjectMeta), err
	}

	deleteEndpointFn := func(name string) error {
		return c.KubeClient.Endpoints(c.Namespace).Delete(name, c.deleteOptions)
	}

	return c.deleteClusterObject(get, deleteEndpointFn, "endpoint")
}

func (c *Cluster) deletePatroniClusterConfigMaps() error {
	get := func(name string) (spec.NamespacedName, error) {
		cm, err := c.KubeClient.ConfigMaps(c.Namespace).Get(name, metav1.GetOptions{})
		return util.NameFromMeta(cm.ObjectMeta), err
	}

	deleteConfigMapFn := func(name string) error {
		return c.KubeClient.ConfigMaps(c.Namespace).Delete(name, c.deleteOptions)
	}

	return c.deleteClusterObject(get, deleteConfigMapFn, "configmap")
}
