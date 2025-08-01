/*
Copyright 2022 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package extender

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"volcano.sh/volcano/pkg/scheduler/api"
	"volcano.sh/volcano/pkg/scheduler/framework"
	"volcano.sh/volcano/pkg/scheduler/plugins/util"
)

const (
	// PluginName indicates name of volcano scheduler plugin.
	PluginName = "extender"

	// ExtenderURLPrefix is the key for providing extender endpoint address
	ExtenderURLPrefix = "extender.urlPrefix"
	// ExtenderHTTPTimeout is the timeout for extender http calls
	ExtenderHTTPTimeout = "extender.httpTimeout"
	// ExtenderOnSessionOpenVerb is the verb of OnSessionOpen method
	ExtenderOnSessionOpenVerb = "extender.onSessionOpenVerb"
	// ExtenderOnSessionCloseVerb is the verb of OnSessionClose method
	ExtenderOnSessionCloseVerb = "extender.onSessionCloseVerb"
	// ExtenderPredicateVerb is the verb of Predicate method
	ExtenderPredicateVerb = "extender.predicateVerb"
	// ExtenderPrioritizeVerb is the verb of Prioritize method
	ExtenderPrioritizeVerb = "extender.prioritizeVerb"
	// ExtenderPreemptableVerb is the verb of Preemptable method
	ExtenderPreemptableVerb = "extender.preemptableVerb"
	// ExtenderReclaimableVerb is the verb of Reclaimable method
	ExtenderReclaimableVerb = "extender.reclaimableVerb"
	// ExtenderQueueOverusedVerb is the verb of QueueOverused method
	ExtenderQueueOverusedVerb = "extender.queueOverusedVerb"
	// ExtenderJobEnqueueableVerb is the verb of JobEnqueueable method
	ExtenderJobEnqueueableVerb = "extender.jobEnqueueableVerb"
	// ExtenderJobReadyVerb is the verb of JobReady method
	ExtenderJobReadyVerb = "extender.jobReadyVerb"
	// ExtenderAllocateFuncVerb is the verb of AllocateFunc method
	ExtenderAllocateFuncVerb = "extender.allocateFuncVerb"
	// ExtenderDeallocateFuncVerb is the verb of DeallocateFunc method
	ExtenderDeallocateFuncVerb = "extender.deallocateFuncVerb"
	// ExtenderIgnorable indicates whether the extender can ignore unexpected errors
	ExtenderIgnorable = "extender.ignorable"

	// 10MB
	maxBodySize = 10 << 20
	// ExtenderManagedResources is the managed resources list split by ","
	ExtenderManagedResources = "extender.managedResources"
)

type extenderConfig struct {
	urlPrefix          string
	httpTimeout        time.Duration
	onSessionOpenVerb  string
	onSessionCloseVerb string
	predicateVerb      string
	prioritizeVerb     string
	preemptableVerb    string
	reclaimableVerb    string
	queueOverusedVerb  string
	jobEnqueueableVerb string
	jobReadyVerb       string
	allocateFuncVerb   string
	deallocateFuncVerb string
	ignorable          bool
	managedResources   sets.Set[string]
}

type extenderPlugin struct {
	client http.Client
	config *extenderConfig
}

func parseExtenderConfig(arguments framework.Arguments) *extenderConfig {
	/*
		   actions: "reclaim, allocate, backfill, preempt"
		   tiers:
		   - plugins:
		     - name: priority
		     - name: gang
		     - name: conformance
		   - plugins:
		     - name: drf
		     - name: predicates
			 - name: extender
		       arguments:
				   extender.urlPrefix: http://127.0.0.1
				   extender.httpTimeout: 100ms
				   extender.onSessionOpenVerb: onSessionOpen
				   extender.onSessionCloseVerb: onSessionClose
				   extender.predicateVerb: predicate
				   extender.prioritizeVerb: prioritize
				   extender.preemptableVerb: preemptable
				   extender.reclaimableVerb: reclaimable
				   extender.queueOverusedVerb: queueOverused
				   extender.jobEnqueueableVerb: jobEnqueueable
				   extender.ignorable: true
				   extender.managedResources:
				   - nvidia.com/gpu
				   - nvidia.com/gpumem
		     - name: proportion
		     - name: nodeorder
	*/
	ec := &extenderConfig{}
	ec.urlPrefix, _ = arguments[ExtenderURLPrefix].(string)
	ec.onSessionOpenVerb, _ = arguments[ExtenderOnSessionOpenVerb].(string)
	ec.onSessionCloseVerb, _ = arguments[ExtenderOnSessionCloseVerb].(string)
	ec.predicateVerb, _ = arguments[ExtenderPredicateVerb].(string)
	ec.prioritizeVerb, _ = arguments[ExtenderPrioritizeVerb].(string)
	ec.preemptableVerb, _ = arguments[ExtenderPreemptableVerb].(string)
	ec.reclaimableVerb, _ = arguments[ExtenderReclaimableVerb].(string)
	ec.queueOverusedVerb, _ = arguments[ExtenderQueueOverusedVerb].(string)
	ec.jobEnqueueableVerb, _ = arguments[ExtenderJobEnqueueableVerb].(string)
	ec.jobReadyVerb, _ = arguments[ExtenderJobReadyVerb].(string)
	ec.allocateFuncVerb, _ = arguments[ExtenderAllocateFuncVerb].(string)
	ec.deallocateFuncVerb, _ = arguments[ExtenderDeallocateFuncVerb].(string)

	arguments.GetBool(&ec.ignorable, ExtenderIgnorable)

	ec.httpTimeout = time.Second
	if httpTimeout, _ := arguments[ExtenderHTTPTimeout].(string); httpTimeout != "" {
		if timeoutDuration, err := time.ParseDuration(httpTimeout); err == nil {
			ec.httpTimeout = timeoutDuration
		}
	}
	managedResources, ok := framework.Get[[]string](arguments, ExtenderManagedResources)
	if ok {
		ec.managedResources = sets.New[string](managedResources...)
	}

	return ec
}

func New(arguments framework.Arguments) framework.Plugin {
	cfg := parseExtenderConfig(arguments)
	klog.V(4).Infof("Initialize extender plugin with endpoint address %s", cfg.urlPrefix)
	return &extenderPlugin{client: http.Client{Timeout: cfg.httpTimeout}, config: cfg}
}

func (ep *extenderPlugin) Name() string {
	return PluginName
}

func (ep *extenderPlugin) OnSessionOpen(ssn *framework.Session) {
	if ep.config.onSessionOpenVerb != "" {
		err := ep.send(ep.config.onSessionOpenVerb, &OnSessionOpenRequest{
			Jobs:           ssn.Jobs,
			Nodes:          ssn.Nodes,
			Queues:         ssn.Queues,
			NamespaceInfo:  ssn.NamespaceInfo,
			RevocableNodes: ssn.RevocableNodes,
		}, nil)
		if err != nil {
			klog.Warningf("OnSessionClose failed with error %v", err)
		}
		if err != nil && !ep.config.ignorable {
			return
		}
	}

	if ep.config.predicateVerb != "" {
		ssn.AddPredicateFn(ep.Name(), func(task *api.TaskInfo, node *api.NodeInfo) error {
			if !ep.IsInterested(task) {
				return nil
			}

			resp := &PredicateResponse{}
			err := ep.send(ep.config.predicateVerb, &PredicateRequest{Task: task, Node: node}, resp)
			if err != nil {
				klog.Warningf("Predicate failed with error %v", err)

				if ep.config.ignorable {
					return nil
				}
				return api.NewFitError(task, node, err.Error())
			}

			if len(resp.ErrorMessage) == 0 {
				return nil
			}
			// keep compatibility with old behavior: error messages length is not zero,
			// but didn't return a code, and code will be 0 for default. Change code to Error for corresponding
			if resp.Code == api.Success {
				resp.Code = api.Error
			}
			return api.NewFitErrWithStatus(task, node, &api.Status{Code: resp.Code, Reason: resp.ErrorMessage, Plugin: PluginName})
		})
	}

	if ep.config.prioritizeVerb != "" {
		ssn.AddBatchNodeOrderFn(ep.Name(), func(task *api.TaskInfo, nodes []*api.NodeInfo) (map[string]float64, error) {
			if !ep.IsInterested(task) {
				return map[string]float64{}, nil
			}

			resp := &PrioritizeResponse{}
			err := ep.send(ep.config.prioritizeVerb, &PrioritizeRequest{Task: task, Nodes: nodes}, resp)
			if err != nil {
				klog.Warningf("Prioritize failed with error %v", err)

				if ep.config.ignorable {
					return nil, nil
				}
				return nil, err
			}

			if resp.ErrorMessage == "" && resp.NodeScore != nil {
				return resp.NodeScore, nil
			}
			return nil, errors.New(resp.ErrorMessage)
		})
	}

	if ep.config.preemptableVerb != "" {
		ssn.AddPreemptableFn(ep.Name(), func(evictor *api.TaskInfo, evictees []*api.TaskInfo) ([]*api.TaskInfo, int) {
			if !ep.IsInterested(evictor) {
				return []*api.TaskInfo{}, util.Abstain
			}

			resp := &PreemptableResponse{}
			err := ep.send(ep.config.preemptableVerb, &PreemptableRequest{Evictor: evictor, Evictees: evictees}, resp)
			if err != nil {
				klog.Warningf("Preemptable failed with error %v", err)

				if ep.config.ignorable {
					return nil, util.Permit
				}
				return nil, util.Reject
			}

			return resp.Victims, resp.Status
		})
	}

	if ep.config.reclaimableVerb != "" {
		ssn.AddReclaimableFn(ep.Name(), func(evictor *api.TaskInfo, evictees []*api.TaskInfo) ([]*api.TaskInfo, int) {
			if !ep.IsInterested(evictor) {
				return []*api.TaskInfo{}, util.Abstain
			}

			resp := &ReclaimableResponse{}
			err := ep.send(ep.config.reclaimableVerb, &ReclaimableRequest{Evictor: evictor, Evictees: evictees}, resp)
			if err != nil {
				klog.Warningf("Reclaimable failed with error %v", err)

				if ep.config.ignorable {
					return nil, util.Permit
				}
				return nil, util.Reject
			}

			return resp.Victims, resp.Status
		})
	}

	if ep.config.jobEnqueueableVerb != "" {
		ssn.AddJobEnqueueableFn(ep.Name(), func(obj interface{}) int {
			job := obj.(*api.JobInfo)
			resp := &JobEnqueueableResponse{}
			err := ep.send(ep.config.jobEnqueueableVerb, &JobEnqueueableRequest{Job: job}, resp)
			if err != nil {
				klog.Warningf("JobEnqueueable failed with error %v", err)

				if ep.config.ignorable {
					return util.Permit
				}
				return util.Reject
			}

			return resp.Status
		})
	}

	if ep.config.queueOverusedVerb != "" {
		ssn.AddOverusedFn(ep.Name(), func(obj interface{}) bool {
			queue := obj.(*api.QueueInfo)
			resp := &QueueOverusedResponse{}
			err := ep.send(ep.config.queueOverusedVerb, &QueueOverusedRequest{Queue: queue}, resp)
			if err != nil {
				klog.Warningf("QueueOverused failed with error %v", err)

				return !ep.config.ignorable
			}

			return resp.Overused
		})
	}

	if ep.config.jobReadyVerb != "" {
		ssn.AddJobReadyFn(ep.Name(), func(obj interface{}) bool {
			job := obj.(*api.JobInfo)
			resp := &JobReadyResponse{}
			err := ep.send(ep.config.jobReadyVerb, &JobReadyRequest{Job: job}, resp)
			if err != nil {
				klog.Warningf("JobReady failed with error %v", err)

				return !ep.config.ignorable
			}

			return resp.Status
		})
	}

	addEventHandler(ssn, ep)
}

func (ep *extenderPlugin) OnSessionClose(ssn *framework.Session) {
	if ep.config.onSessionCloseVerb != "" {
		if err := ep.send(ep.config.onSessionCloseVerb, &OnSessionCloseRequest{}, nil); err != nil {
			klog.Warningf("OnSessionClose failed with error %v", err)
		}
	}
}

func (ep *extenderPlugin) send(action string, args interface{}, result interface{}) error {
	out, err := json.Marshal(args)
	if err != nil {
		return err
	}

	url := strings.TrimRight(ep.config.urlPrefix, "/") + "/" + action

	req, err := http.NewRequest("POST", url, bytes.NewReader(out))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := ep.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed %v with extender at URL %v, code %v", action, url, resp.StatusCode)
	}

	if result != nil {
		resp.Body = http.MaxBytesReader(nil, resp.Body, maxBodySize)
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// IsInterested returns true if at least one extended resource requested by
// this pod is managed by this extender.
//
// This code is adapted from the Kubernetes project:
// https://github.com/kubernetes/kubernetes/blob/master/pkg/scheduler/extender.go
func (ep *extenderPlugin) IsInterested(task *api.TaskInfo) bool {
	if ep.config.managedResources.Len() == 0 {
		return true
	}
	if ep.hasManagedResources(task.Pod.Spec.Containers) {
		return true
	}
	if ep.hasManagedResources(task.Pod.Spec.InitContainers) {
		return true
	}
	return false
}

func (ep *extenderPlugin) hasManagedResources(containers []corev1.Container) bool {
	for _, container := range containers {
		if ep.hasResourcesInList(container.Resources.Requests) ||
			ep.hasResourcesInList(container.Resources.Limits) {
			return true
		}
	}
	return false
}

// hasResourcesInList checks if any resource in the given ResourceList is managed by this extender
func (ep *extenderPlugin) hasResourcesInList(resources corev1.ResourceList) bool {
	for resourceName := range resources {
		if ep.config.managedResources.Has(string(resourceName)) {
			return true
		}
	}
	return false
}

func addEventHandler(ssn *framework.Session, ep *extenderPlugin) {
	const (
		AllocateFunc   = "AllocateFunc"
		DeallocateFunc = "DeallocateFunc"
	)
	eventHandlerFunc := func(funcName string) func(event *framework.Event) {
		return func(event *framework.Event) {
			if event == nil {
				klog.Errorf("%s event nil.", funcName)
				return
			}
			if !ep.IsInterested(event.Task) {
				return
			}
			resp := &EventHandlerResponse{}
			var verb string
			switch funcName {
			case AllocateFunc:
				verb = ep.config.allocateFuncVerb
			case DeallocateFunc:
				verb = ep.config.deallocateFuncVerb
			}
			err := ep.send(verb, &EventHandlerRequest{Task: event.Task}, resp)
			if err != nil {
				klog.Warningf("%s failed with error %v", funcName, err)

				if !ep.config.ignorable {
					event.Err = err
				}
			}
			if resp.ErrorMessage != "" {
				event.Err = errors.New(resp.ErrorMessage)
			}
		}
	}

	var eventHandler framework.EventHandler
	if ep.config.allocateFuncVerb != "" {
		eventHandler.AllocateFunc = eventHandlerFunc(AllocateFunc)
	}

	if ep.config.deallocateFuncVerb != "" {
		eventHandler.DeallocateFunc = eventHandlerFunc(DeallocateFunc)
	}

	ssn.AddEventHandler(&eventHandler)
}
