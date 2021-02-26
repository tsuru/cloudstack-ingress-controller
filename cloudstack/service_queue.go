package cloudstack

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog"
)

const (
	defaultUpdateLBWorkers = 5

	promNamespace = "csccm"
	promSubsystem = "update_lb_queue"

	eventReasonUpdateFailed = "UpdateLoadBalancerFailed"
)

var (
	defaultFailureBackoff = 15 * time.Second

	processedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: promNamespace,
		Subsystem: promSubsystem,
		Name:      "processed_total",
		Help:      "The number of processed queue items",
	}, []string{"namespace", "service"})

	failuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: promNamespace,
		Subsystem: promSubsystem,
		Name:      "failures_total",
		Help:      "The number of failed queue items",
	}, []string{"namespace", "service"})

	processedDuration = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: promNamespace,
		Subsystem: promSubsystem,
		Name:      "item_duration_seconds",
		Help:      "The duration of the last processed queue item",
	}, []string{"namespace", "service"})
)

type updateLBNodeQueue struct {
	sync.Mutex
	cs     *CSCloud
	queue  map[serviceKey]queueEntry
	doneWG sync.WaitGroup
	stopCh chan struct{}
}

type queueEntry struct {
	service      *corev1.Service
	lb           *loadBalancer
	updatePool   bool
	start        time.Time
	backoffUntil time.Time
}

type queueEntryWithNodeRecent struct {
	queueEntry
	topRevision uint64
}

func newServiceNodeQueue(cs *CSCloud) *updateLBNodeQueue {
	return &updateLBNodeQueue{
		cs:    cs,
		queue: map[serviceKey]queueEntry{},
	}
}

func (q *updateLBNodeQueue) pushWithBackoff(entry queueEntry, backoff time.Duration) error {
	entry.backoffUntil = time.Now().Add(backoff)
	entry.start = time.Now()
	entry.lb = nil
	return q.push(entry)
}

func (q *updateLBNodeQueue) push(entry queueEntry) error {
	q.Lock()
	defer q.Unlock()

	key := serviceKey{
		namespace: entry.service.Namespace,
		name:      entry.service.Name,
	}

	if existing, ok := q.queue[key]; ok {
		existing.service = entry.service.DeepCopy()
		existing.lb = nil
		q.queue[key] = existing
		return nil
	}

	// Simply validate that there are nodes available, we'll fetch them
	// directly from the registry when running the task.
	_, err := q.cs.nodeRegistry.nodesForService(entry.service)
	if err != nil {
		return err
	}

	if q.queue == nil {
		q.queue = map[serviceKey]queueEntry{}
	}

	entry.service = entry.service.DeepCopy()
	q.queue[key] = entry

	return nil
}

func (q *updateLBNodeQueue) pop() (queueEntry, bool, error) {
	q.Lock()
	defer q.Unlock()

	var extendEntries []queueEntryWithNodeRecent

	for svcKey, entry := range q.queue {
		topRevision := uint64(0)
		svcNodes := q.cs.nodeRegistry.nodesContainingService(svcKey)
		for _, node := range svcNodes {
			if node.revision > topRevision {
				topRevision = node.revision
			}
		}
		extendEntries = append(extendEntries, queueEntryWithNodeRecent{
			queueEntry:  entry,
			topRevision: topRevision,
		})
	}

	if len(extendEntries) == 0 {
		return queueEntry{}, false, nil
	}

	sort.Slice(extendEntries, func(i, j int) bool {
		if time.Until(extendEntries[i].backoffUntil) > 0 {
			return false
		}
		if time.Until(extendEntries[j].backoffUntil) > 0 {
			return true
		}
		if extendEntries[i].topRevision == extendEntries[j].topRevision {
			return extendEntries[i].start.Before(extendEntries[j].start)
		}
		return extendEntries[i].topRevision > extendEntries[j].topRevision
	})

	entry := extendEntries[0].queueEntry
	if time.Until(entry.backoffUntil) > 0 {
		return queueEntry{}, false, nil
	}

	klog.V(4).Infof("Popping queued service %v/%v revision %d start %v", entry.service.Namespace, entry.service.Name, extendEntries[0].topRevision, entry.start)

	delete(q.queue, serviceKey{namespace: entry.service.Namespace, name: entry.service.Name})
	return extendEntries[0].queueEntry, true, nil
}

func (q *updateLBNodeQueue) stopWait() {
	close(q.stopCh)
	q.doneWG.Wait()
}

func (q *updateLBNodeQueue) start(ctx context.Context) {
	workers := q.cs.config.Global.UpdateLBWorkers
	if workers == 0 {
		workers = defaultUpdateLBWorkers
	}
	q.stopCh = make(chan struct{})
	for i := 0; i < workers; i++ {
		q.doneWG.Add(1)
		go func() {
			defer q.doneWG.Done()
			waitTimer := time.NewTimer(0)
			exitWhenDone := false
			for {
				select {
				case <-ctx.Done():
					return
				case <-q.stopCh:
					exitWhenDone = true
				case <-waitTimer.C:
					waitTimer.Reset(0)
				}

				item, ok, err := q.pop()
				if err != nil {
					klog.Errorf("unable to pop item from queue: %v", err)
					continue
				}

				if !ok {
					if exitWhenDone {
						return
					}

					if !waitTimer.Stop() {
						<-waitTimer.C
					}
					waitTimer.Reset(time.Second)
					continue
				}

				err = q.processQueueEntry(item)
				processedTotal.WithLabelValues(item.service.Namespace, item.service.Name).Inc()
				processedDuration.WithLabelValues(item.service.Namespace, item.service.Name).Set(time.Since(item.start).Seconds())
				if err != nil {
					msg := fmt.Sprintf("Error updating load balancer with new hosts: %v", err)
					klog.Error(msg)
					failuresTotal.WithLabelValues(item.service.Namespace, item.service.Name).Inc()
					q.cs.recorder.Event(item.service, corev1.EventTypeWarning, eventReasonUpdateFailed, msg)
					q.pushWithBackoff(item, defaultFailureBackoff)
				}
			}
		}()
	}
}

func (q *updateLBNodeQueue) processQueueEntry(entry queueEntry) error {
	q.cs.svcLock.Lock(entry.service)
	defer q.cs.svcLock.Unlock(entry.service)

	nodes, err := q.cs.nodeRegistry.nodesForService(entry.service)
	if err != nil {
		return err
	}

	klog.V(4).Infof("Processing lb update for service %v/%v with nodes %v", entry.service.Namespace, entry.service.Name, nodeInfoNames(nodes))

	hostIDs, networkIDs, projectID := idsForNodes(nodes)

	lb := entry.lb
	if lb == nil {
		// Get the load balancer details and existing rules.
		lb, err = q.cs.getLoadBalancer(entry.service, projectID, networkIDs)
		if err != nil {
			return err
		}

		if lb.rule == nil {
			return nil
		}

		err = shouldManageLB(lb)
		if err != nil {
			klog.V(3).Infof("Skipping UpdateLoadBalancer for service %s/%s: %v", entry.service.Namespace, entry.service.Name, err)
			return nil
		}
	}

	err = lb.syncNodes(hostIDs, networkIDs)
	if err != nil {
		return err
	}

	if entry.updatePool {
		err = lb.updateLoadBalancerPool()
		if err != nil {
			return err
		}
	}

	return nil
}
