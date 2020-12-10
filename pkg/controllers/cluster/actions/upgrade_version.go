package actions

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/gocql/gocql"
	"github.com/pkg/errors"
	"github.com/scylladb/go-log"
	"github.com/scylladb/go-set/strset"
	"github.com/scylladb/gocqlx/v2"
	scyllav1alpha1 "github.com/scylladb/scylla-operator/pkg/api/v1alpha1"
	"github.com/scylladb/scylla-operator/pkg/controllers/cluster/resource"
	"github.com/scylladb/scylla-operator/pkg/controllers/cluster/util"
	"github.com/scylladb/scylla-operator/pkg/naming"
	"github.com/scylladb/scylla-operator/pkg/scyllaclient"
	"github.com/scylladb/scylla-operator/pkg/util/fsm"
	"github.com/scylladb/scylla-operator/pkg/util/parallel"
	"github.com/scylladb/scylla-operator/pkg/util/timeutc"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ClusterVersionUpgradeAction = "rack-version-upgrade"
	actionTimeout               = time.Minute
)

type ClusterVersionUpgrade struct {
	Cluster        *scyllav1alpha1.ScyllaCluster
	ScyllaClient   *scyllaclient.Client
	ClusterSession cqlSession

	ipMapping map[string]string

	currentRack *scyllav1alpha1.RackSpec
	currentNode *corev1.Pod

	cc         client.Client
	kubeClient kubernetes.Interface
	recorder   record.EventRecorder
	logger     log.Logger
}

func NewClusterVersionUpgradeAction(c *scyllav1alpha1.ScyllaCluster, l log.Logger) *ClusterVersionUpgrade {
	return &ClusterVersionUpgrade{
		Cluster:   c,
		logger:    l,
		ipMapping: map[string]string{},
	}
}

func (a *ClusterVersionUpgrade) Name() string {
	return ClusterVersionUpgradeAction
}

var ScyllaClientForClusterFunc = func(ctx context.Context, cc client.Client, hosts []string, logger log.Logger) (*scyllaclient.Client, error) {
	cfg := scyllaclient.DefaultConfig(hosts...)
	return scyllaclient.NewClient(cfg, logger)
}

type cqlSession interface {
	AwaitSchemaAgreement(ctx context.Context) error
}

var NewSessionFunc = func(hosts []string) (cqlSession, error) {
	cluster := gocql.NewCluster(hosts...)
	return gocqlx.WrapSession(cluster.CreateSession())
}

func (a *ClusterVersionUpgrade) nonMaintenanceHosts(ctx context.Context) ([]string, error) {
	var hosts []string
	for _, r := range a.Cluster.Spec.Datacenter.Racks {
		services, err := util.GetMemberServicesForRack(ctx, r, a.Cluster, a.cc)
		if err != nil {
			return nil, errors.Wrap(err, "get member services for rack")
		}

		for _, s := range services {
			a.ipMapping[s.Name] = s.Spec.ClusterIP
			if _, ok := s.Labels[naming.NodeMaintenanceLabel]; !ok {
				hosts = append(hosts, s.Spec.ClusterIP)
			}
		}
	}

	return hosts, nil
}

type upgradeProcedure int

const (
	majorUpgradeProcedure = iota
	patchUpgradeProcedure
)

func (a *ClusterVersionUpgrade) upgradeProcedure(ctx context.Context) upgradeProcedure {
	if a.Cluster.Status.Upgrade != nil {
		return majorUpgradeProcedure
	}
	var fromVersion string
	// Fetch version from any rack status, they should be in the same version.
	for _, r := range a.Cluster.Spec.Datacenter.Racks {
		fromVersion = a.Cluster.Status.Racks[r.Name].Version
		break
	}

	oldVersion, err := semver.Parse(fromVersion)
	if err != nil {
		a.logger.Info(ctx, "Invalid from semantic version", "version", fromVersion)
		return majorUpgradeProcedure
	}
	newVersion, err := semver.Parse(a.Cluster.Spec.Version)
	if err != nil {
		a.logger.Info(ctx, "Invalid to semantic version", "version", fromVersion)
		return majorUpgradeProcedure
	}

	// Check that version remained the same
	if newVersion.Major == oldVersion.Major && newVersion.Minor == oldVersion.Minor {
		return patchUpgradeProcedure
	}
	return majorUpgradeProcedure
}

func (a *ClusterVersionUpgrade) Execute(ctx context.Context, s *State) error {
	a.cc = s.Client
	a.kubeClient = s.kubeclient
	a.recorder = s.recorder

	switch a.upgradeProcedure(ctx) {
	case majorUpgradeProcedure:
		a.logger.Info(ctx, "Using major upgrade procedure")
		return a.majorUpgrade(ctx)
	case patchUpgradeProcedure:
		a.logger.Info(ctx, "Using patch upgrade procedure")
		return a.patchUpgrade(ctx)
	default:
		panic("unknown upgrade procedure")
	}
}

func (a *ClusterVersionUpgrade) majorUpgrade(ctx context.Context) error {
	hosts, err := a.nonMaintenanceHosts(ctx)
	if err != nil {
		return errors.Wrap(err, "host discovery")
	}

	a.ScyllaClient, err = ScyllaClientForClusterFunc(ctx, a.cc, hosts, a.logger)
	if err != nil {
		return errors.Wrap(err, "create scylla client")
	}

	a.ClusterSession, err = NewSessionFunc(hosts)
	if err != nil {
		return errors.Wrap(err, "create scylla session")
	}

	if err := a.fsm().Transition(ctx); err != nil {
		return errors.Wrap(err, "upgrade fsm transition")
	}

	return nil
}

func (a *ClusterVersionUpgrade) patchUpgrade(ctx context.Context) error {
	c := a.Cluster
	for _, r := range c.Spec.Datacenter.Racks {
		a.logger.Debug(ctx, fmt.Sprintf("Rack: %s, Rack Members: %d, Spec members: %d\n", r.Name, r.Members, c.Status.Racks[r.Name].Members))
		if c.Status.Racks[r.Name].Version != c.Spec.Version {
			sts := &appsv1.StatefulSet{}
			if err := a.cc.Get(ctx, naming.NamespacedName(naming.StatefulSetNameForRack(r, c), c.Namespace), sts); err != nil {
				return errors.Wrap(err, "failed to get statefulset")
			}
			image := resource.ImageForCluster(c)
			if err := util.UpgradeStatefulSetScyllaImage(ctx, sts, image, a.kubeClient); err != nil {
				return errors.Wrap(err, "failed to upgrade statefulset")
			}
			// Record event for successful version upgrade
			a.recorder.Event(c, corev1.EventTypeNormal, naming.SuccessSynced, fmt.Sprintf("Rack %s upgraded up to version %s", r.Name, c.Spec.Version))
			return nil
		}
	}
	return nil
}

const (
	ActionFailure fsm.EventType = fsm.NoOp
	ActionSuccess fsm.EventType = "action_success"

	AllNodesUpgraded fsm.EventType = "all_nodes_upgraded"
	AllRacksUpgraded fsm.EventType = "all_racks_upgraded"

	BeginUpgrade           fsm.StateType = "begin_upgrade"
	CheckSchemaAgreement   fsm.StateType = "check_schema_agreement"
	CreateSystemBackup     fsm.StateType = "create_system_backup"
	FindNextRack           fsm.StateType = "find_next_rack"
	UpgradeImageInPodSpec  fsm.StateType = "upgrade_image_in_pod_spec"
	FindNextNode           fsm.StateType = "find_next_node"
	EnableMaintenanceMode  fsm.StateType = "enable_maintenance_mode"
	DrainNode              fsm.StateType = "drain_node"
	BackupData             fsm.StateType = "backup_data"
	DisableMaintenanceMode fsm.StateType = "disable_maintenance_mode"
	DeletePod              fsm.StateType = "delete_pod"
	ValidateUpgrade        fsm.StateType = "validate_upgrade"
	ClearDataBackup        fsm.StateType = "clear_data_backup"
	ClearSystemBackup      fsm.StateType = "clear_system_backup"
	RestoreUpgradeStrategy fsm.StateType = "restore_upgrade_strategy"
	FinishUpgrade          fsm.StateType = "finish_upgrade"
)

func snapshotTag(prefix string, t time.Time) string {
	return fmt.Sprintf("so_%s_%sUTC", prefix, t.UTC().Format("20060102150405"))
}

func (a *ClusterVersionUpgrade) beginUpgrade(ctx context.Context) (fsm.EventType, error) {
	var fromVersion string
	// Fetch version from any rack status, they should be in the same version.
	for _, r := range a.Cluster.Spec.Datacenter.Racks {
		fromVersion = a.Cluster.Status.Racks[r.Name].Version
		break
	}

	now := timeutc.Now()
	err := a.upgradeClusterStatus(ctx, func(status *scyllav1alpha1.ClusterStatus) {
		status.Upgrade = &scyllav1alpha1.UpgradeStatus{
			State:             string(BeginUpgrade),
			FromVersion:       fromVersion,
			ToVersion:         a.Cluster.Spec.Version,
			SystemSnapshotTag: snapshotTag("system", now),
			DataSnapshotTag:   snapshotTag("data", now),
		}
	})
	if err != nil {
		return ActionFailure, errors.Wrap(err, "patch upgrade status")
	}

	// Wait until patch is applied, other functions relies on Status.Upgrade being non-nil.
	err = wait.PollImmediate(retryInterval, actionTimeout, func() (done bool, err error) {
		cluster := &scyllav1alpha1.ScyllaCluster{}
		if err := a.cc.Get(ctx, naming.NamespacedName(a.Cluster.Name, a.Cluster.Namespace), cluster); err != nil {
			return false, err
		}
		return cluster.Status.Upgrade != nil, nil
	})
	if err != nil {
		return ActionFailure, errors.Wrap(err, "wait for patch apply")
	}

	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) checkSchemaAgreement(ctx context.Context) (fsm.EventType, error) {
	if err := wait.PollImmediate(retryInterval, actionTimeout, func() (bool, error) {
		if err := a.ClusterSession.AwaitSchemaAgreement(ctx); err != nil {
			if strings.Contains(err.Error(), "cluster schema versions not consistent") {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}); err != nil {
		return ActionFailure, errors.Wrap(err, "await schema agreement")
	}

	return ActionSuccess, nil
}

var systemKeyspaces = []string{"system", "system_schema"}

func (a *ClusterVersionUpgrade) backupKeyspaces(ctx context.Context, tag string, keyspaces []string, hosts ...string) error {
	if hosts == nil {
		for _, r := range a.Cluster.Spec.Datacenter.Racks {
			for i := 0; i < int(r.Members); i++ {
				host := naming.MemberServiceName(r, a.Cluster, i)
				hosts = append(hosts, a.ipMapping[host])
			}
		}
	}

	err := parallel.Run(len(hosts), parallel.NoLimit, func(i int) error {
		host := hosts[i]

		snapshots, err := a.ScyllaClient.Snapshots(ctx, host)
		if err != nil {
			return errors.Wrap(err, "list snapshots")
		}

		if contains(snapshots, tag) {
			return nil
		}

		for _, keyspace := range keyspaces {
			if err := a.ScyllaClient.TakeSnapshot(ctx, host, tag, keyspace); err != nil {
				return errors.Wrap(err, "take snapshot")
			}
		}

		return nil
	})

	return err
}

func (a *ClusterVersionUpgrade) clearBackup(ctx context.Context, tag string, hosts ...string) error {
	if hosts == nil {
		for _, r := range a.Cluster.Spec.Datacenter.Racks {
			for i := 0; i < int(r.Members); i++ {
				host := naming.MemberServiceName(r, a.Cluster, i)
				hosts = append(hosts, a.ipMapping[host])
			}
		}
	}

	err := parallel.Run(len(hosts), parallel.NoLimit, func(i int) error {
		host := hosts[i]

		snapshots, err := a.ScyllaClient.Snapshots(ctx, host)
		if err != nil {
			return errors.Wrap(err, "list snapshots")
		}

		if !contains(snapshots, tag) {
			return nil
		}

		if err := a.ScyllaClient.DeleteSnapshot(ctx, host, tag); err != nil {
			return errors.Wrap(err, "take snapshot")
		}

		return nil
	})

	return err
}

func (a *ClusterVersionUpgrade) createSystemBackup(ctx context.Context) (fsm.EventType, error) {
	if err := a.backupKeyspaces(ctx, a.Cluster.Status.Upgrade.SystemSnapshotTag, systemKeyspaces); err != nil {
		return ActionFailure, errors.Wrap(err, "system backup")
	}

	return ActionSuccess, nil
}

func versionFromImage(image string) (string, error) {
	parts := strings.Split(image, ":")
	if len(parts) != 2 {
		return "", errors.Errorf("can't extract version from image %q", image)
	}
	return parts[1], nil
}

func (a *ClusterVersionUpgrade) enableMaintenanceMode(ctx context.Context) (fsm.EventType, error) {
	node, err := a.getCurrentNode(ctx)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "get node")
	}

	service := &corev1.Service{}
	if err := a.cc.Get(ctx, naming.NamespacedName(naming.ServiceNameFromPod(node), a.Cluster.Namespace), service); err != nil {
		a.logger.Error(ctx, "", "error", err)
		return ActionFailure, errors.Wrap(err, "fetch member service")
	}

	patched := service.DeepCopy()
	patched.Labels[naming.NodeMaintenanceLabel] = ""

	if err := util.PatchService(ctx, service, patched, a.kubeClient); err != nil {
		return ActionFailure, errors.Wrap(err, "patch service")
	}

	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) disableMaintenanceMode(ctx context.Context) (fsm.EventType, error) {
	node, err := a.getCurrentNode(ctx)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "get node")
	}
	service := &corev1.Service{}
	if err := a.cc.Get(ctx, naming.NamespacedName(naming.ServiceNameFromPod(node), a.Cluster.Namespace), service); err != nil {
		return ActionFailure, errors.Wrap(err, "fetch member service")
	}

	patched := service.DeepCopy()
	delete(patched.Labels, naming.NodeMaintenanceLabel)

	if err := util.PatchService(ctx, service, patched, a.kubeClient); err != nil {
		return ActionFailure, errors.Wrap(err, "patch service")
	}

	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) drainNode(ctx context.Context) (fsm.EventType, error) {
	node, err := a.getCurrentNode(ctx)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "get node")
	}
	a.logger.Info(ctx, "Draining node", "node", node.Name)

	host := a.ipMapping[node.Name]

	op, err := a.ScyllaClient.OperationMode(ctx, host)
	if err != nil {
		a.logger.Error(ctx, "get operation mode", "error", err)
		return ActionFailure, errors.Wrap(err, "get node operation mode")
	}
	if op.IsDrained() {
		return ActionSuccess, nil
	}
	if op.IsDraining() {
		if err := wait.PollImmediate(retryInterval, actionTimeout, func() (bool, error) {
			op, err := a.ScyllaClient.OperationMode(ctx, host)
			if err != nil {
				return false, err
			}
			return op.IsDrained(), nil
		}); err != nil {
			return ActionFailure, errors.Wrap(err, "wait until node is drained")
		}
	} else {
		if err := a.ScyllaClient.Drain(ctx, host); err != nil {
			a.logger.Error(ctx, "node drain", "error", err)
			return ActionFailure, errors.Wrap(err, "node drain")
		}
		if err := wait.PollImmediate(retryInterval, actionTimeout, func() (bool, error) {
			op, err := a.ScyllaClient.OperationMode(ctx, host)
			if err != nil {
				return false, err
			}
			return op.IsDrained(), nil
		}); err != nil {
			return ActionFailure, errors.Wrap(err, "wait until node is drained")
		}
	}

	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) createDataBackup(ctx context.Context) (fsm.EventType, error) {
	a.logger.Info(ctx, "Backing up the data")
	node, err := a.getCurrentNode(ctx)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "get node")
	}
	host := a.ipMapping[node.Name]

	keyspaces, err := a.ScyllaClient.Keyspaces(ctx)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "list keyspaces")
	}

	dataKeyspaces := strset.New(keyspaces...)
	dataKeyspaces.Remove(systemKeyspaces...)

	if err := a.backupKeyspaces(ctx, a.Cluster.Status.Upgrade.DataSnapshotTag, dataKeyspaces.List(), host); err != nil {
		return ActionFailure, errors.Wrap(err, "keyspace backup")
	}

	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) updateRackSpec(ctx context.Context) (fsm.EventType, error) {
	rack, err := a.getCurrentRack(ctx)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "get rack")
	}
	a.logger.Info(ctx, "Updating rack spec", "rack", rack.Name)

	sts := &appsv1.StatefulSet{}
	err = a.cc.Get(ctx, naming.NamespacedName(naming.StatefulSetNameForRack(*rack, a.Cluster), a.Cluster.Namespace), sts)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "get rack sts")
	}

	upgradeSts := sts.DeepCopy()
	upgradeSts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type: appsv1.OnDeleteStatefulSetStrategyType,
	}

	idx, err := naming.FindScyllaContainer(upgradeSts.Spec.Template.Spec.Containers)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "find scylla container")
	}

	image := resource.ImageForCluster(a.Cluster)
	upgradeSts.Spec.Template.Spec.Containers[idx].Image = image

	if err := util.PatchStatefulSet(ctx, sts, upgradeSts, a.kubeClient); err != nil {
		return ActionFailure, errors.Wrap(err, "update rack sts")
	}

	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) nextRack(ctx context.Context) (*scyllav1alpha1.RackSpec, error) {
	for _, r := range a.Cluster.Spec.Datacenter.Racks {
		pods := &corev1.PodList{}
		if err := a.cc.List(ctx, pods, &client.ListOptions{
			LabelSelector: naming.RackSelector(r, a.Cluster),
		}); err != nil {
			return nil, errors.Wrap(err, "get rack pods")
		}

		for _, p := range pods.Items {
			idx, err := naming.FindScyllaContainer(p.Spec.Containers)
			if err != nil {
				return nil, errors.Wrap(err, "find scylla container")
			}

			containerVersion, err := versionFromImage(p.Spec.Containers[idx].Image)
			if err != nil {
				return nil, errors.Wrap(err, "parse scylla container version")
			}
			if containerVersion != a.Cluster.Status.Upgrade.ToVersion {
				a.logger.Info(ctx, "Next rack", "rack", r.Name)
				return &r, nil
			}
		}
	}

	return nil, nil
}

func (a *ClusterVersionUpgrade) findNextRack(ctx context.Context) (fsm.EventType, error) {
	r, err := a.nextRack(ctx)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "find next rack")
	}

	if err := a.upgradeClusterStatus(ctx, func(status *scyllav1alpha1.ClusterStatus) {
		if r == nil {
			status.Upgrade.CurrentRack = ""
		} else {
			status.Upgrade.CurrentRack = r.Name
		}
	}); err != nil {
		return ActionFailure, errors.Wrap(err, "upgrade next rack in status")
	}

	a.currentRack = r

	if r == nil {
		a.logger.Info(ctx, "All racks upgraded")
		return AllRacksUpgraded, nil
	}

	a.recorder.Event(a.Cluster, corev1.EventTypeNormal, naming.SuccessSynced, fmt.Sprintf("Upgrading rack %s to version %s", r.Name, a.Cluster.Spec.Version))
	a.logger.Info(ctx, "Upgrading next rack", "rack", r.Name)
	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) getCurrentRack(ctx context.Context) (*scyllav1alpha1.RackSpec, error) {
	if a.currentRack == nil {
		cluster := &scyllav1alpha1.ScyllaCluster{}
		if err := a.cc.Get(ctx, naming.NamespacedName(a.Cluster.Name, a.Cluster.Namespace), cluster); err != nil {
			return nil, errors.Wrap(err, "refresh cluster")
		}

		for _, r := range cluster.Spec.Datacenter.Racks {
			if r.Name == cluster.Status.Upgrade.CurrentRack {
				a.currentRack = &r
				break
			}
		}
	}

	return a.currentRack, nil
}

func (a *ClusterVersionUpgrade) getCurrentNode(ctx context.Context) (*corev1.Pod, error) {
	if a.currentNode == nil {
		cluster := &scyllav1alpha1.ScyllaCluster{}
		if err := a.cc.Get(ctx, naming.NamespacedName(a.Cluster.Name, a.Cluster.Namespace), cluster); err != nil {
			return nil, errors.Wrap(err, "refresh cluster")
		}

		pod := &corev1.Pod{}
		if err := a.cc.Get(ctx, naming.NamespacedName(cluster.Status.Upgrade.CurrentNode, a.Cluster.Namespace), pod); err != nil {
			return nil, errors.Wrap(err, "fetch current pod")
		}

		a.currentNode = pod
	}
	return a.currentNode, nil
}

func (a *ClusterVersionUpgrade) nextNode(ctx context.Context) (*corev1.Pod, error) {
	for _, r := range a.Cluster.Spec.Datacenter.Racks {
		pods := &corev1.PodList{}
		if err := a.cc.List(ctx, pods, &client.ListOptions{
			LabelSelector: naming.RackSelector(r, a.Cluster),
		}); err != nil {
			return nil, errors.Wrap(err, "get pods")
		}

		// Sort by name to have consistent order
		// Last node first
		sort.Slice(pods.Items, func(i, j int) bool {
			return pods.Items[i].Name > pods.Items[j].Name
		})

		for _, p := range pods.Items {
			idx, err := naming.FindScyllaContainer(p.Spec.Containers)
			if err != nil {
				return nil, errors.Wrap(err, "find scylla container")
			}

			containerVersion, err := versionFromImage(p.Spec.Containers[idx].Image)
			if err != nil {
				return nil, errors.Wrap(err, "parse scylla container image version")
			}
			if containerVersion != a.Cluster.Status.Upgrade.ToVersion {
				a.logger.Info(ctx, "Next node", "node", r.Name)
				return &p, nil
			}
		}
	}

	return nil, nil
}

func (a *ClusterVersionUpgrade) findNextNode(ctx context.Context) (fsm.EventType, error) {
	n, err := a.nextNode(ctx)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "find next node")
	}

	if err := a.upgradeClusterStatus(ctx, func(status *scyllav1alpha1.ClusterStatus) {
		if n == nil {
			status.Upgrade.CurrentNode = ""
		} else {
			status.Upgrade.CurrentNode = n.Name
		}
	}); err != nil {
		return ActionFailure, errors.Wrap(err, "upgrade next node in status")
	}

	if n == nil {
		a.logger.Info(ctx, "All nodes upgraded")
		return AllNodesUpgraded, nil
	}

	// Save it in cache
	a.currentNode = n

	a.logger.Info(ctx, "Upgrading next node", "node", n.Name)
	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) deletePod(ctx context.Context) (fsm.EventType, error) {
	node, err := a.getCurrentNode(ctx)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "get node")
	}

	// Refresh pod obj
	pod := &corev1.Pod{}
	if err := a.cc.Get(ctx, naming.NamespacedName(node.Name, node.Namespace), pod); err != nil {
		if apierrors.IsNotFound(err) {
			return ActionSuccess, nil
		}

		return ActionFailure, errors.Wrap(err, "get pod")
	}

	if err := a.cc.Delete(ctx, pod); err != nil {
		return ActionFailure, errors.Wrap(err, "delete pod")
	}

	return ActionSuccess, nil
}

// TODO: configurable in cluster spec
const validationTimeout = 30 * time.Minute

func (a *ClusterVersionUpgrade) validateUpgrade(ctx context.Context) (fsm.EventType, error) {
	err := wait.PollImmediate(retryInterval, validationTimeout, func() (done bool, err error) {
		node, err := a.getCurrentNode(ctx)
		if err != nil {
			return false, errors.Wrap(err, "get current node")
		}

		if err := a.cc.Get(ctx, naming.NamespacedName(node.Name, node.Namespace), node); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, errors.Wrap(err, "refresh pod")
		}

		idx, err := naming.FindScyllaContainer(node.Spec.Containers)
		if err != nil {
			return false, errors.Wrap(err, "find scylla container")
		}

		ver, err := versionFromImage(node.Spec.Containers[idx].Image)
		if err != nil {
			return false, errors.Wrap(err, "parse container image")
		}

		a.logger.Debug(ctx, "Node validation", "node", node.Name, "ready", podReady(node), "ver", ver)
		return podReady(node) && a.Cluster.Status.Upgrade.ToVersion == ver, nil
	})
	if err != nil {
		return ActionFailure, errors.Wrap(err, "wait for ready pods")
	}

	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) clearDataBackup(ctx context.Context) (fsm.EventType, error) {
	if err := a.clearBackup(ctx, a.Cluster.Status.Upgrade.DataSnapshotTag); err != nil {
		return ActionFailure, errors.Wrap(err, "clear data backup")
	}
	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) clearSystemBackup(ctx context.Context) (fsm.EventType, error) {
	if err := a.clearBackup(ctx, a.Cluster.Status.Upgrade.SystemSnapshotTag); err != nil {
		return ActionFailure, errors.Wrap(err, "clear system backup")
	}
	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) upgradeClusterStatus(ctx context.Context, f func(cluster *scyllav1alpha1.ClusterStatus)) error {
	cluster := &scyllav1alpha1.ScyllaCluster{}
	patched := &scyllav1alpha1.ScyllaCluster{}

	resourceVersion := ""
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := a.cc.Get(ctx, naming.NamespacedName(a.Cluster.Name, a.Cluster.Namespace), cluster); err != nil {
			return err
		}
		patched = cluster.DeepCopy()
		f(&patched.Status)
		resourceVersion = cluster.ResourceVersion

		if err := a.cc.Status().Update(ctx, patched); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Wait until change propagates
	err = wait.PollImmediate(retryInterval, actionTimeout, func() (done bool, err error) {
		if err := a.cc.Get(ctx, naming.NamespacedName(a.Cluster.Name, a.Cluster.Namespace), cluster); err != nil {
			return false, err
		}
		if cluster.ResourceVersion != resourceVersion || reflect.DeepEqual(cluster.Status, patched.Status) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return err
	}

	a.Cluster = patched

	return err
}

func (a *ClusterVersionUpgrade) onStateTransition(ctx context.Context, currentState, nextState fsm.StateType, event fsm.EventType) error {
	a.logger.Info(ctx, "Upgrade state transition", "event", event, "from", currentState, "to", nextState)

	err := a.upgradeClusterStatus(ctx, func(status *scyllav1alpha1.ClusterStatus) {
		status.Upgrade.State = string(nextState)
	})
	if err != nil {
		return errors.Wrap(err, "patch cluster status")
	}

	return nil
}

func (a *ClusterVersionUpgrade) finishUpgrade(ctx context.Context) (fsm.EventType, error) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cluster := &scyllav1alpha1.ScyllaCluster{}
		if err := a.cc.Get(ctx, naming.NamespacedName(a.Cluster.Name, a.Cluster.Namespace), cluster); err != nil {
			return err
		}
		cluster.Status.Upgrade = nil
		if err := a.cc.Status().Update(ctx, cluster); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return ActionFailure, errors.Wrap(err, "update cluster status")
	}

	a.recorder.Event(a.Cluster, corev1.EventTypeNormal, naming.SuccessSynced, fmt.Sprintf("Cluster upgraded to version %s", a.Cluster.Spec.Version))

	// Return NoOp which will stop state machine.
	return fsm.NoOp, nil
}

func (a *ClusterVersionUpgrade) setRollingUpgradeStrategy(ctx context.Context, rack *scyllav1alpha1.RackSpec) error {
	sts := &appsv1.StatefulSet{}
	err := a.cc.Get(ctx, naming.NamespacedName(naming.StatefulSetNameForRack(*rack, a.Cluster), a.Cluster.Namespace), sts)
	if err != nil {
		return errors.Wrap(err, "get rack sts")
	}

	upgradeSts := sts.DeepCopy()
	upgradeSts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type: appsv1.RollingUpdateStatefulSetStrategyType,
	}

	if err := util.PatchStatefulSet(ctx, sts, upgradeSts, a.kubeClient); err != nil {
		return errors.Wrap(err, "update rack sts")
	}
	return nil
}

func (a *ClusterVersionUpgrade) restoreUpgradeStrategy(ctx context.Context) (fsm.EventType, error) {
	rack, err := a.getCurrentRack(ctx)
	if err != nil {
		return ActionFailure, errors.Wrap(err, "get rack")
	}

	if rack != nil {
		if err := a.setRollingUpgradeStrategy(ctx, rack); err != nil {
			return ActionFailure, errors.Wrap(err, "set upgrade strategy")
		}
	} else {
		// When all nodes upgraded, validate if every has proper upgrade strategy
		for _, r := range a.Cluster.Spec.Datacenter.Racks {
			if err := a.setRollingUpgradeStrategy(ctx, &r); err != nil {
				return ActionFailure, errors.Wrap(err, "set upgrade strategy")
			}
		}
	}

	return ActionSuccess, nil
}

func (a *ClusterVersionUpgrade) fsm() *fsm.StateMachine {
	state := BeginUpgrade
	if a.Cluster.Status.Upgrade != nil {
		state = fsm.StateType(a.Cluster.Status.Upgrade.State)
	}

	return &fsm.StateMachine{
		TransitionHook: a.onStateTransition,
		Current:        state,
		States: map[fsm.StateType]fsm.State{
			BeginUpgrade: {
				Action: a.beginUpgrade,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: CheckSchemaAgreement,
				}},
			CheckSchemaAgreement: {
				Action: a.checkSchemaAgreement,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: CreateSystemBackup,
				}},
			CreateSystemBackup: {
				Action: a.createSystemBackup,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: FindNextRack,
				}},
			FindNextRack: {
				Action: a.findNextRack,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess:    UpgradeImageInPodSpec,
					AllRacksUpgraded: ClearSystemBackup,
				},
			},
			UpgradeImageInPodSpec: {
				Action: a.updateRackSpec,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: FindNextNode,
				},
			},
			RestoreUpgradeStrategy: {
				Action: a.restoreUpgradeStrategy,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: FindNextRack,
				},
			},
			FindNextNode: {
				Action: a.findNextNode,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess:    EnableMaintenanceMode,
					AllNodesUpgraded: RestoreUpgradeStrategy,
				},
			},
			EnableMaintenanceMode: {
				Action: a.enableMaintenanceMode,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: DrainNode,
				},
			},
			DrainNode: {
				Action: a.drainNode,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: BackupData,
				},
			},
			BackupData: {
				Action: a.createDataBackup,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: DisableMaintenanceMode,
				},
			},
			DisableMaintenanceMode: {
				Action: a.disableMaintenanceMode,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: DeletePod,
				},
			},
			DeletePod: {
				Action: a.deletePod,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: ValidateUpgrade,
				},
			},
			ValidateUpgrade: {
				Action: a.validateUpgrade,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: ClearDataBackup,
				},
			},
			ClearDataBackup: {
				Action: a.clearDataBackup,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: FindNextNode,
				},
			},
			ClearSystemBackup: {
				Action: a.clearSystemBackup,
				Events: map[fsm.EventType]fsm.StateType{
					ActionSuccess: FinishUpgrade,
				},
			},
			FinishUpgrade: {
				Action: a.finishUpgrade,
			},
		},
	}
}

func contains(arr []string, v string) bool {
	for _, elem := range arr {
		if elem == v {
			return true
		}
	}
	return false
}
