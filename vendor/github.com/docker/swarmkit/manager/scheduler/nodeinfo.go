package scheduler

import (
	"time"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/api/genericresource"
	"github.com/docker/swarmkit/log"
	"golang.org/x/net/context"
)

// hostPortSpec specifies a used host port.
type hostPortSpec struct {
	protocol      api.PortConfig_Protocol
	publishedPort uint32
}

// NodeInfo contains a node and some additional metadata.
type NodeInfo struct {
	*api.Node
	Tasks                     map[string]*api.Task
	ActiveTasksCount          int
	ActiveTasksCountByService map[string]int
	AvailableResources        *api.Resources
	usedHostPorts             map[hostPortSpec]struct{}

	// recentFailures is a map from service ID to the timestamps of the
	// most recent failures the node has experienced from replicas of that
	// service.
	// TODO(aaronl): When spec versioning is supported, this should track
	// the version of the spec that failed.
	recentFailures map[string][]time.Time
}

func newNodeInfo(n *api.Node, tasks map[string]*api.Task, availableResources api.Resources) NodeInfo {
	nodeInfo := NodeInfo{
		Node:  n,
		Tasks: make(map[string]*api.Task),
		ActiveTasksCountByService: make(map[string]int),
		AvailableResources:        availableResources.Copy(),
		usedHostPorts:             make(map[hostPortSpec]struct{}),
		recentFailures:            make(map[string][]time.Time),
	}

	for _, t := range tasks {
		nodeInfo.addTask(t)
	}

	return nodeInfo
}

// removeTask removes a task from nodeInfo if it's tracked there, and returns true
// if nodeInfo was modified.
func (nodeInfo *NodeInfo) removeTask(t *api.Task) bool {
	oldTask, ok := nodeInfo.Tasks[t.ID]
	if !ok {
		return false
	}

	delete(nodeInfo.Tasks, t.ID)
	if oldTask.DesiredState <= api.TaskStateRunning {
		nodeInfo.ActiveTasksCount--
		nodeInfo.ActiveTasksCountByService[t.ServiceID]--
	}

	reservations := taskReservations(t.Spec)
	resources := nodeInfo.AvailableResources

	resources.MemoryBytes += reservations.MemoryBytes
	resources.NanoCPUs += reservations.NanoCPUs

	if nodeInfo.Description == nil || nodeInfo.Description.Resources == nil ||
		nodeInfo.Description.Resources.Generic == nil {
		return true
	}

	taskAssigned := t.AssignedGenericResources
	nodeAvailableResources := &resources.Generic
	nodeRes := nodeInfo.Description.Resources.Generic
	genericresource.Reclaim(nodeAvailableResources, taskAssigned, nodeRes)

	if t.Endpoint != nil {
		for _, port := range t.Endpoint.Ports {
			if port.PublishMode == api.PublishModeHost && port.PublishedPort != 0 {
				portSpec := hostPortSpec{protocol: port.Protocol, publishedPort: port.PublishedPort}
				delete(nodeInfo.usedHostPorts, portSpec)
			}
		}
	}

	return true
}

// addTask adds or updates a task on nodeInfo, and returns true if nodeInfo was
// modified.
func (nodeInfo *NodeInfo) addTask(t *api.Task) bool {
	oldTask, ok := nodeInfo.Tasks[t.ID]
	if ok {
		if t.DesiredState <= api.TaskStateRunning && oldTask.DesiredState > api.TaskStateRunning {
			nodeInfo.Tasks[t.ID] = t
			nodeInfo.ActiveTasksCount++
			nodeInfo.ActiveTasksCountByService[t.ServiceID]++
			return true
		} else if t.DesiredState > api.TaskStateRunning && oldTask.DesiredState <= api.TaskStateRunning {
			nodeInfo.Tasks[t.ID] = t
			nodeInfo.ActiveTasksCount--
			nodeInfo.ActiveTasksCountByService[t.ServiceID]--
			return true
		}
		return false
	}

	nodeInfo.Tasks[t.ID] = t

	reservations := taskReservations(t.Spec)
	resources := nodeInfo.AvailableResources

	resources.MemoryBytes -= reservations.MemoryBytes
	resources.NanoCPUs -= reservations.NanoCPUs

	// minimum size required
	t.AssignedGenericResources = make([]*api.GenericResource, 0, len(resources.Generic))
	taskAssigned := &t.AssignedGenericResources

	genericresource.Claim(&resources.Generic, taskAssigned, reservations.Generic)

	if t.Endpoint != nil {
		for _, port := range t.Endpoint.Ports {
			if port.PublishMode == api.PublishModeHost && port.PublishedPort != 0 {
				portSpec := hostPortSpec{protocol: port.Protocol, publishedPort: port.PublishedPort}
				nodeInfo.usedHostPorts[portSpec] = struct{}{}
			}
		}
	}

	if t.DesiredState <= api.TaskStateRunning {
		nodeInfo.ActiveTasksCount++
		nodeInfo.ActiveTasksCountByService[t.ServiceID]++
	}

	return true
}

func taskReservations(spec api.TaskSpec) (reservations api.Resources) {
	if spec.Resources != nil && spec.Resources.Reservations != nil {
		reservations = *spec.Resources.Reservations
	}
	return
}

// taskFailed records a task failure from a given service.
func (nodeInfo *NodeInfo) taskFailed(ctx context.Context, serviceID string) {
	expired := 0
	now := time.Now()
	for _, timestamp := range nodeInfo.recentFailures[serviceID] {
		if now.Sub(timestamp) < monitorFailures {
			break
		}
		expired++
	}

	if len(nodeInfo.recentFailures[serviceID])-expired == maxFailures-1 {
		log.G(ctx).Warnf("underweighting node %s for service %s because it experienced %d failures or rejections within %s", nodeInfo.ID, serviceID, maxFailures, monitorFailures.String())
	}

	nodeInfo.recentFailures[serviceID] = append(nodeInfo.recentFailures[serviceID][expired:], now)
}

// countRecentFailures returns the number of times the service has failed on
// this node within the lookback window monitorFailures.
func (nodeInfo *NodeInfo) countRecentFailures(now time.Time, serviceID string) int {
	recentFailureCount := len(nodeInfo.recentFailures[serviceID])
	for i := recentFailureCount - 1; i >= 0; i-- {
		if now.Sub(nodeInfo.recentFailures[serviceID][i]) > monitorFailures {
			recentFailureCount -= i + 1
			break
		}
	}

	return recentFailureCount
}
