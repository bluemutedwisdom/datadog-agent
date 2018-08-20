// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2018 Datadog, Inc.

// +build kubeapiserver

package apiserver

import (
	"fmt"
	"time"

	agentcache "github.com/DataDog/datadog-agent/pkg/util/cache"
	"github.com/DataDog/datadog-agent/pkg/util/log"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// ClusterTagsController is responsible for synchronizing objects from the Kubernetes
// apiserver to build and cache cluster-level tags (like service tags) for each node.
//
// The controller takes care to garbage collect any data while processing updates/deletes
// so that the cache does not contain data for deleted pods/services.
//
// This controller is used by the Datadog Cluster Agent and supports Kubernetes 1.4+.
type ClusterTagsController struct {
	nodeLister       corelisters.NodeLister
	nodeListerSynced cache.InformerSynced

	endpointsLister       corelisters.EndpointsLister
	endpointsListerSynced cache.InformerSynced

	// Endpoints that need to be added to services mapping.
	queue workqueue.RateLimitingInterface

	// used in unit tests to wait until endpoints are synced
	endpoints chan interface{}
}

func NewClusterTagsController(nodeInformer coreinformers.NodeInformer, endpointsInformer coreinformers.EndpointsInformer) *ClusterTagsController {
	c := &ClusterTagsController{
		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "cluster-tags"),
	}
	c.nodeLister = nodeInformer.Lister()
	c.nodeListerSynced = nodeInformer.Informer().HasSynced

	endpointsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addEndpoints,
		UpdateFunc: c.updateEndpoints,
		DeleteFunc: c.deleteEndpoints,
	})
	c.endpointsLister = endpointsInformer.Lister()
	c.endpointsListerSynced = endpointsInformer.Informer().HasSynced

	return c
}

func (c *ClusterTagsController) Run(stopCh <-chan struct{}) {
	defer c.queue.ShutDown()

	log.Infof("Starting metadata controller")
	defer log.Infof("Stopping metadata controller")

	if !cache.WaitForCacheSync(stopCh, c.nodeListerSynced, c.endpointsListerSynced) {
		return
	}

	go wait.Until(c.worker, time.Second, stopCh)

	<-stopCh
}

func (c *ClusterTagsController) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *ClusterTagsController) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.syncEndpoints(key.(string))
	if err != nil {
		log.Debugf("Error syncing endpoints %v: %v", key, err)
	}

	if c.endpoints != nil {
		c.endpoints <- key
	}

	return true
}

func (c *ClusterTagsController) addEndpoints(obj interface{}) {
	endpoints, ok := obj.(*corev1.Endpoints)
	if !ok {
		return
	}
	log.Debugf("Adding endpoints %s/%s", endpoints.Namespace, endpoints.Name)
	c.enqueue(obj)
}

func (c *ClusterTagsController) updateEndpoints(_, cur interface{}) {
	newEndpoints, ok := cur.(*corev1.Endpoints)
	if !ok {
		return
	}
	log.Tracef("Updating endpoints %s/%s", newEndpoints.Namespace, newEndpoints.Name)
	c.enqueue(cur)
}

func (c *ClusterTagsController) deleteEndpoints(obj interface{}) {
	endpoints, ok := obj.(*corev1.Endpoints)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			log.Debugf("Couldn't get object from tombstone %#v", obj)
			return
		}
		endpoints, ok = tombstone.Obj.(*corev1.Endpoints)
		if !ok {
			log.Debugf("Tombstone contained object that is not an endpoint %#v", obj)
			return
		}
	}
	log.Debugf("Deleting endpoints %s/%s", endpoints.Namespace, endpoints.Name)
	c.enqueue(obj)
}

func (c *ClusterTagsController) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Debugf("Couldn't get key for object %v: %v", obj, err)
		return
	}
	c.queue.Add(key)
}

func (c *ClusterTagsController) syncEndpoints(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	endpoints, err := c.endpointsLister.Endpoints(namespace).Get(name)
	switch {
	case errors.IsNotFound(err):
		// Endpoints absence in store means watcher caught the deletion, ensure metadata map is cleaned.
		log.Tracef("Endpoints has been deleted %v. Attempting to cleanup metadata map", key)
		err = c.deleteMappedEndpoints(namespace, name)
	case err != nil:
		log.Debugf("Unable to retrieve endpoints %v from store: %v", key, err)
	default:
		err = c.mapEndpoints(endpoints)
	}
	return err
}

// mapEndpoints matches pods to services via endpoint TargetRef objects. It supports Kubernetes 1.4+.
func (c *ClusterTagsController) mapEndpoints(endpoints *corev1.Endpoints) error {
	nodeToPods := make(map[string]map[string]sets.String)

	// Loop over the subsets to create a mapping of nodes to pods running on the node.
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.TargetRef == nil {
				// Endpoints are also used by the control plane as resource locks for leader election.
				// These endpoints will not have a TargetRef and can be ignored.
				log.Tracef("No TargetRef for endpoints %s/%s, skipping", endpoints.Namespace, endpoints.Name)
				continue
			}
			if address.TargetRef.Kind != "Pod" {
				continue
			}
			namespace := address.TargetRef.Namespace
			podName := address.TargetRef.Name
			if podName == "" || namespace == "" {
				log.Tracef("Incomplete reference for object %s/%s on service %s/%s, skipping",
					address.TargetRef.UID, endpoints.Namespace, endpoints.Name)
				continue
			}

			// TODO: Kubernetes 1.3.x does not include `NodeName`
			if address.NodeName == nil {
				continue
			}

			nodeName := *address.NodeName

			if _, ok := nodeToPods[nodeName]; !ok {
				nodeToPods[nodeName] = make(map[string]sets.String)
			}
			if _, ok := nodeToPods[nodeName][namespace]; !ok {
				nodeToPods[nodeName][namespace] = sets.NewString()
			}
			nodeToPods[nodeName][namespace].Insert(podName)
		}
	}

	svc := endpoints.Name
	namespace := endpoints.Namespace
	for nodeName, ns := range nodeToPods {
		metaBundle, err := getMetadataMapBundle(nodeName)
		if err != nil {
			log.Tracef("Could not get metadata for node %s", nodeName)
			metaBundle = newMetadataMapperBundle()
		}

		metaBundle.m.Lock()
		metaBundle.Services.Delete(namespace, svc) // cleanup pods deleted from the service
		for _, pods := range ns {
			for podName := range pods {
				metaBundle.Services.Set(namespace, podName, svc)
			}
		}
		metaBundle.m.Unlock()

		cacheKey := agentcache.BuildAgentKey(metadataMapperCachePrefix, nodeName)
		if metaBundle.Empty() {
			agentcache.Cache.Delete(cacheKey)
			continue
		}
		agentcache.Cache.Set(cacheKey, metaBundle, agentcache.NoExpiration)
	}

	return nil
}

func (c *ClusterTagsController) deleteMappedEndpoints(namespace, svc string) error {
	nodes, err := c.nodeLister.List(labels.Everything()) // list all nodes
	if err != nil {
		return err
	}

	// Delete the service from the metadata bundle for each node.
	for _, node := range nodes {
		metaBundle, err := getMetadataMapBundle(node.Name)
		if err != nil {
			// Nothing to delete.
			continue
		}

		metaBundle.m.Lock()
		metaBundle.Services.Delete(namespace, svc)
		metaBundle.m.Unlock()

		cacheKey := agentcache.BuildAgentKey(metadataMapperCachePrefix, node.Name)
		if metaBundle.Empty() {
			agentcache.Cache.Delete(cacheKey)
			continue
		}
		agentcache.Cache.Set(cacheKey, metaBundle, agentcache.NoExpiration)
	}
	return nil
}

// GetPodClusterTags returns a list of cluster-level tags for the specified pod, namespace, and node.
func GetPodClusterTags(nodeName, ns, podName string) ([]string, error) {
	var metaList []string
	cacheKey := agentcache.BuildAgentKey(metadataMapperCachePrefix, nodeName)

	metaBundleInterface, found := agentcache.Cache.Get(cacheKey)
	if !found {
		log.Tracef("no metadata was found for the pod %s on node %s", podName, nodeName)
		return nil, nil
	}

	metaBundle, ok := metaBundleInterface.(*MetadataMapperBundle)
	if !ok {
		return nil, fmt.Errorf("invalid cache format for the cacheKey: %s", cacheKey)
	}
	// The list of metadata collected in the metaBundle is extensible and is handled here.
	// If new cluster level tags need to be collected by the agent, only this needs to be modified.
	serviceList, foundServices := metaBundle.ServicesForPod(ns, podName)
	if !foundServices {
		log.Tracef("no cached services list found for the pod %s on the node %s", podName, nodeName)
		return nil, nil
	}
	log.Tracef("CacheKey: %s, with %d services", cacheKey, len(serviceList))
	for _, s := range serviceList {
		metaList = append(metaList, fmt.Sprintf("kube_service:%s", s))
	}

	return metaList, nil
}