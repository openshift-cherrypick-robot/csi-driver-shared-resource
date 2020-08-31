package controller

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	objcache "github.com/openshift/csi-driver-projected-resource/pkg/cache"
	"github.com/openshift/csi-driver-projected-resource/pkg/client"
)

const (
	defaultResyncDuration = 10 * time.Minute
)

var (
	singleton *Controller
)

type Controller struct {
	kubeRestConfig *rest.Config

	cfgMapWorkqueue workqueue.RateLimitingInterface
	secretWorkqueue workqueue.RateLimitingInterface

	cfgMapInformer cache.SharedIndexInformer
	secInformer    cache.SharedIndexInformer

	informerFactory informers.SharedInformerFactory

	listers *client.Listers
}

func NewController() (*Controller, error) {
	kubeRestConfig, err := client.GetConfig()
	if err != nil {
		return nil, err
	}

	kubeClient, err := kubernetes.NewForConfig(kubeRestConfig)
	if err != nil {
		return nil, err
	}

	informerFactory := informers.NewSharedInformerFactoryWithOptions(kubeClient,
		defaultResyncDuration,
		informers.WithNamespace("openshift-config"))

	c := &Controller{
		kubeRestConfig: kubeRestConfig,
		cfgMapWorkqueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(),
			"projected-resource-configmap-changes"),
		secretWorkqueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(),
			"projected-resource-secret-changes"),
		informerFactory: informerFactory,
		cfgMapInformer:  informerFactory.Core().V1().ConfigMaps().Informer(),
		secInformer:     informerFactory.Core().V1().Secrets().Informer(),
		listers:         &client.Listers{},
	}

	c.cfgMapInformer.AddEventHandler(c.configMapEventHandler())
	c.secInformer.AddEventHandler(c.secretEventHandler())

	singleton = c

	return c, nil
}

func (c *Controller) Run(stopCh <-chan struct{}) error {
	defer c.cfgMapWorkqueue.ShutDown()
	defer c.secretWorkqueue.ShutDown()

	c.informerFactory.Start(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.cfgMapInformer.HasSynced, c.secInformer.HasSynced) {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	go wait.Until(c.configMapEventProcessor, time.Second, stopCh)
	go wait.Until(c.secretEventProcessor, time.Second, stopCh)

	<-stopCh

	return nil
}

func (c *Controller) addConfigMapToQueue(cm *corev1.ConfigMap, verb client.ObjectAction) {
	event := client.Event{
		Object: cm,
		Verb:   verb,
	}
	c.cfgMapWorkqueue.Add(event)
}

// as the actions we have to take *MAY* vary to a significant enough degree between add, update, and delete,
// especially if we move off of vanilla os.MkdirAll / os.Create, we propagate that distinction down the line
func (c *Controller) configMapEventHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(o interface{}) {
			switch v := o.(type) {
			case *corev1.ConfigMap:
				c.addConfigMapToQueue(v, client.AddObjectAction)
			default:
				//log unrecognized type
			}
		},
		UpdateFunc: func(o, n interface{}) {
			switch v := o.(type) {
			case *corev1.ConfigMap:
				c.addConfigMapToQueue(v, client.UpdateObjectAction)
			default:
				//log unrecognized type
			}
		},
		DeleteFunc: func(o interface{}) {
			switch v := o.(type) {
			case cache.DeletedFinalStateUnknown:
				switch vv := v.Obj.(type) {
				case *corev1.ConfigMap:
					// log recovered deleted obj from tombstone via vv.GetName()
					c.addConfigMapToQueue(vv, client.DeleteObjectAction)
				default:
					// log  error decoding obj tombstone
				}
			case *corev1.ConfigMap:
				c.addConfigMapToQueue(v, client.DeleteObjectAction)
			default:
				//log unrecognized type
			}
		},
	}
}

func (c *Controller) configMapEventProcessor() {
	for {
		obj, shutdown := c.cfgMapWorkqueue.Get()
		if shutdown {
			return
		}

		func() {
			defer c.cfgMapWorkqueue.Done(obj)

			event, ok := obj.(client.Event)
			if !ok {
				c.cfgMapWorkqueue.Forget(obj)
				return
			}

			if err := c.syncConfigMap(event); err != nil {
				c.cfgMapWorkqueue.AddRateLimited(obj)
			} else {
				c.cfgMapWorkqueue.Forget(obj)
			}
		}()
	}
}

func (c *Controller) syncConfigMap(event client.Event) error {
	cm, ok := event.Object.(*corev1.ConfigMap)
	if cm == nil || !ok {
		msg := fmt.Sprintf("unexpected object vs. configmap: %v", event.Object.GetObjectKind().GroupVersionKind())
		fmt.Print(msg)
		return fmt.Errorf(msg)
	}
	klog.V(0).Infof("verb %s obj namespace %s name %s", event.Verb, cm.Namespace, cm.Name)
	// since we don't mutate we do not copy
	switch event.Verb {
	case client.DeleteObjectAction:
		objcache.DelConfigMap(cm)
	case client.AddObjectAction:
		// again, add vs. update distinctions upheld for now, even though the path is common, in case
		// host filesystem interactions changes such that different methods for add vs. update are needed
		objcache.UpsertConfigMap(cm)
	case client.UpdateObjectAction:
		// again, add vs. update distinctions upheld for now, even though the path is common, in case
		// host filesystem interactions changes such that different methods for add vs. update are needed
		objcache.UpsertConfigMap(cm)
	default:
		return fmt.Errorf("unexpected configmap event action: %s", event.Verb)
	}
	return nil
}

func (c *Controller) addSecretToQueue(cm *corev1.Secret, verb client.ObjectAction) {
	event := client.Event{
		Object: cm,
		Verb:   verb,
	}
	c.secretWorkqueue.Add(event)
}

// as the actions we have to take vary to a significant enough degree between add, update, and delete
// we propagate that distinction down the line
func (c *Controller) secretEventHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(o interface{}) {
			switch v := o.(type) {
			case *corev1.Secret:
				c.addSecretToQueue(v, client.AddObjectAction)
			default:
				//log unrecognized type
			}
		},
		UpdateFunc: func(o, n interface{}) {
			switch v := o.(type) {
			case *corev1.Secret:
				c.addSecretToQueue(v, client.UpdateObjectAction)
			default:
				//log unrecognized type
			}
		},
		DeleteFunc: func(o interface{}) {
			switch v := o.(type) {
			case cache.DeletedFinalStateUnknown:
				switch vv := v.Obj.(type) {
				case *corev1.Secret:
					// log recovered deleted obj from tombstone via vv.GetName()
					c.addSecretToQueue(vv, client.DeleteObjectAction)
				default:
					// log  error decoding obj tombstone
				}
			case *corev1.Secret:
				c.addSecretToQueue(v, client.DeleteObjectAction)
			default:
				//log unrecognized type
			}
		},
	}
}

func (c *Controller) secretEventProcessor() {
	for {
		obj, shutdown := c.secretWorkqueue.Get()
		if shutdown {
			return
		}

		func() {
			defer c.secretWorkqueue.Done(obj)

			event, ok := obj.(client.Event)
			if !ok {
				c.secretWorkqueue.Forget(obj)
				return
			}

			if err := c.syncSecret(event); err != nil {
				c.secretWorkqueue.AddRateLimited(obj)
			} else {
				c.secretWorkqueue.Forget(obj)
			}
		}()
	}
}

func (c *Controller) syncSecret(event client.Event) error {
	secret, ok := event.Object.(*corev1.Secret)
	if secret == nil || !ok {
		return fmt.Errorf("unexpected object vs. secret: %v", event.Object.GetObjectKind().GroupVersionKind())
	}
	// since we don't mutate we do not copy
	klog.V(0).Infof("verb %s obj namespace %s name %s", event.Verb, secret.Namespace, secret.Name)
	switch event.Verb {
	case client.DeleteObjectAction:
		objcache.DelSecret(secret)
	case client.AddObjectAction:
		// again, add vs. update distinctions upheld for now, even though the path is common, in case
		// host filesystem interactions changes such that different methods for add vs. update are needed
		objcache.UpsertSecret(secret)
	case client.UpdateObjectAction:
		// again, add vs. update distinctions upheld for now, even though the path is common, in case
		// host filesystem interactions changes such that different methods for add vs. update are needed
		objcache.UpsertSecret(secret)
	default:
		return fmt.Errorf("unexpected secret event action: %s", event.Verb)
	}
	return nil
}
