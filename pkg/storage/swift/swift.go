// Copyright 2017 Thiago da Silva
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package swift

import (
	"encoding/json"

	"github.com/coreos/quartermaster/pkg/operator"
	"github.com/coreos/quartermaster/pkg/spec"
	qmstorage "github.com/coreos/quartermaster/pkg/storage"
	"github.com/heketi/utils"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	restclient "k8s.io/client-go/rest"
)

var (
	logger              = utils.NewLogger("swift", utils.LEVEL_DEBUG)
	waitForDeploymentFn = func(client kubernetes.Interface, namespace, name string, available int32) error {
		return operator.WaitForDeploymentReady(client, namespace, name, available)
	}
)

// This mock storage system serves as an example driver for developers
func New(client kubernetes.Interface, qm restclient.Interface) (qmstorage.StorageType, error) {
	s := &SwiftStorage{
		client: client,
		qm:     qm,
	}

	// Use the StorageHandlerFuncs struct to insulate
	// the driver from interface incompabilities
	return &qmstorage.StorageHandlerFuncs{

		// Save the storage handler.  Great for unit tests
		StorageHandler: s,

		// Provide a function which returns the Type of storage system
		// Required
		TypeFunc: s.Type,

		// This function is called when QM is started
		// Optional
		InitFunc: s.Init,

		// ---------------- Cluster Functions ---------------

		// Called after a new StorageCluster object has been submitted
		// QM takes care of creating StorageNodes for each one defined
		// in the StorageCluster so there is no need for the driver to
		// create them
		AddClusterFunc: s.AddCluster,

		// Called when the StorageCluster has been updated
		UpdateClusterFunc: s.UpdateCluster,

		// Called when the StorageCluster has been deleted. After this
		// call, QM will delete all StorageNode objects.  You may want
		// to wait until all your StorageNodes are deleted, but it all
		// depends on your storage system
		DeleteClusterFunc: s.DeleteCluster,

		// ---------------- Node Functions ---------------

		// Return a Deployment object which requests the installation
		// of the containerized storage software.
		MakeDeploymentFunc: s.MakeDeployment,

		// New StorageNode is ready and it is called after the
		// deployment is available and running.
		AddNodeFunc: s.AddNode,

		// Called when a StorageNode has been updated
		UpdateNodeFunc: s.UpdateNode,

		// Called when a StorageNode has been deleted
		DeleteNodeFunc: s.DeleteNode,
	}, nil
}

type SwiftStorage struct {
	client kubernetes.Interface
	qm     restclient.Interface
}

func (st *SwiftStorage) Init() error {
	logger.Debug("called")
	return nil
}

func (st *SwiftStorage) AddCluster(c *spec.StorageCluster) (*spec.StorageCluster, error) {
	logger.Info("Add cluster %v", c.GetName())

	// Create rings
	err := st.createRings(c)
	if err != nil {
		return nil, err
	}

	// Deploy swift proxies
	err = st.deployProxy(c.Namespace)
	if err != nil {
		return nil, err
	}

	// Create service to access Swift Proxy API
	err = st.deploySwiftProxyService(c.Namespace)
	if err != nil {
		return nil, err
	}

	return nil, nil
}

func (st *SwiftStorage) UpdateCluster(old *spec.StorageCluster,
	new *spec.StorageCluster) error {
	logger.Info("Updating cluster %v", old.GetName())
	return nil
}

func (st *SwiftStorage) DeleteCluster(c *spec.StorageCluster) error {
	logger.Info("Deleting cluster %v", c.GetName())

	services := st.client.Core().Services(c.Namespace)
	err := services.Delete("swiftservice", nil)
	if err != nil {
		return err
	}

	err = services.Delete("swift-ring-master-svc", nil)
	if err != nil {
		return err
	}

	// TODO: deployment and replica set are being deleted, but the pod is not.
	deployments := st.client.Extensions().Deployments(c.Namespace)
	orphanDependents := false
	err = deployments.Delete("swift-proxy-deploy",
		&meta.DeleteOptions{OrphanDependents: &orphanDependents})
	if err != nil {
		return err
	}

	err = deployments.Delete("swift-ring-master-deploy",
		&meta.DeleteOptions{OrphanDependents: &orphanDependents})
	if err != nil {
		return err
	}

	configMaps := st.client.Core().ConfigMaps(c.Namespace)
	err = configMaps.Delete("swift-cluster-configmap", nil)
	if err != nil {
		return err
	}

	return nil
}

func (st *SwiftStorage) MakeDeployment(s *spec.StorageNode,
	old *v1beta1.Deployment) (*v1beta1.Deployment, error) {

	logger.Debug("Make deployment for node %v", s.GetName())
	if s.Spec.Image == "" {
		s.Spec.Image = "thiagodasilva/swift-storage:dev-v1"
	}
	spec, err := st.makeDeploymentSpec(s)
	if err != nil {
		return nil, err
	}
	lmap := make(map[string]string)
	for k, v := range s.Labels {
		lmap[k] = v
	}
	lmap["quartermaster"] = s.Name
	deployment := &v1beta1.Deployment{
		ObjectMeta: meta.ObjectMeta{
			Name:        s.Name,
			Namespace:   s.Namespace,
			Annotations: s.Annotations,
			Labels:      lmap,
		},
		Spec: *spec,
	}
	if old != nil {
		deployment.Annotations = old.Annotations
	}
	return deployment, nil
}

func (st *SwiftStorage) makeDeploymentSpec(s *spec.StorageNode) (*v1beta1.DeploymentSpec, error) {

	volumes := []v1.Volume{
		v1.Volume{
			Name: "swift-storage-etc",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/var/lib/swift_storage/etc",
				},
			},
		},
	}

	mounts := []v1.VolumeMount{
		v1.VolumeMount{
			Name:      "swift-storage-etc",
			MountPath: "/etc/swift",
		},
	}
	replicas := int32(1)
	spec := &v1beta1.DeploymentSpec{
		Replicas: &replicas,
		Template: v1.PodTemplateSpec{
			ObjectMeta: meta.ObjectMeta{
				Labels: map[string]string{
					// Drivers *should* add a quartermaster label
					"quartermaster": s.Name,
					"swift_storage": s.GetName(),
				},
			},
			Spec: v1.PodSpec{
				NodeName:     s.Spec.NodeName,
				NodeSelector: s.Spec.NodeSelector,
				Containers: []v1.Container{
					v1.Container{
						Name:            s.Name,
						Image:           s.Spec.Image,
						ImagePullPolicy: v1.PullIfNotPresent,
						VolumeMounts:    mounts,
						Ports: []v1.ContainerPort{
							v1.ContainerPort{
								// object server
								ContainerPort: 6200,
							},
							v1.ContainerPort{
								// container server
								ContainerPort: 6201,
							},
							v1.ContainerPort{
								// account server
								ContainerPort: 6200,
							},
						},
					},
					v1.Container{
						Name:            "swift-ring-minion",
						Image:           "thiagodasilva/swift_ring_minion:dev-v5",
						ImagePullPolicy: v1.PullIfNotPresent,
						VolumeMounts:    mounts,
					},
				},
				Volumes: volumes,
			},
		},
	}
	return spec, nil
}

func (st *SwiftStorage) AddNode(s *spec.StorageNode) (*spec.StorageNode, error) {
	logger.Info("Adding node %v", s.GetName())
	svc := &v1.Service{
		ObjectMeta: meta.ObjectMeta{
			Name:      s.GetName() + "-svc",
			Namespace: s.Namespace,
			Labels: map[string]string{
				"swift": "swift-storage",
			},
			Annotations: map[string]string{
				"description": "Exposes Swift Storage Service",
			},
		},
		Spec: v1.ServiceSpec{
			Selector: map[string]string{
				"swift_storage": s.GetName(),
			},
			ClusterIP: s.Spec.StorageNetwork.IPs[0],
			Type:      v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{
				v1.ServicePort{
					Name: "account",
					Port: 6200,
					TargetPort: intstr.IntOrString{
						IntVal: 6200,
					},
				},
				v1.ServicePort{
					Name: "container",
					Port: 6201,
					TargetPort: intstr.IntOrString{
						IntVal: 6201,
					},
				},
				v1.ServicePort{
					Name: "object",
					Port: 6202,
					TargetPort: intstr.IntOrString{
						IntVal: 6202,
					},
				},
			},
		},
	}

	// Submit the service
	services := st.client.Core().Services(s.Namespace)
	_, err := services.Create(svc)
	if apierrors.IsAlreadyExists(err) {
		return nil, nil
	} else if err != nil {
		logger.Err(err)
	}

	return nil, nil
}

func (st *SwiftStorage) UpdateNode(s *spec.StorageNode) (*spec.StorageNode, error) {
	logger.Info("Updating storage node %v", s.GetName())
	return nil, nil
}

func (st *SwiftStorage) DeleteNode(s *spec.StorageNode) error {
	logger.Info("Deleting storage node %v", s.GetName())
	services := st.client.Core().Services(s.Namespace)
	err := services.Delete(s.GetName()+"-svc", nil)
	if err != nil {
		return err
	}
	return nil
}

func (st *SwiftStorage) Type() spec.StorageTypeIdentifier {

	// This variable must be defined under the spec pkg
	return spec.StorageTypeIdentifierSwift
}

func (st *SwiftStorage) deployProxy(namespace string) error {
	volumes := []v1.Volume{
		v1.Volume{
			Name: "swift-proxy-etc",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/var/lib/swift_proxy/etc",
				},
			},
		},
	}

	mounts := []v1.VolumeMount{
		v1.VolumeMount{
			Name:      "swift-proxy-etc",
			MountPath: "/etc/swift",
		},
	}

	replicas := int32(1)
	proxyDeploy := &v1beta1.Deployment{
		ObjectMeta: meta.ObjectMeta{
			Name:      "swift-proxy-deploy",
			Namespace: namespace,
			Annotations: map[string]string{
				"description": "Deployment spec for Swift proxy",
			},
			Labels: map[string]string{
				"swift":         "swift-proxy",
				"quartermaster": "swift",
			},
		},
		Spec: v1beta1.DeploymentSpec{
			Replicas: &replicas,
			Template: v1.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{
						"swift":         "swift-proxy",
						"quartermaster": "swift",
					},
					Name: "swift-proxy-pod",
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						v1.Container{
							Name:            "swift-proxy",
							Image:           "thiagodasilva/swift-proxy:dev-v1",
							ImagePullPolicy: v1.PullIfNotPresent,
							VolumeMounts:    mounts,
							Ports: []v1.ContainerPort{
								v1.ContainerPort{
									ContainerPort: 8080,
								},
							},
						},
						v1.Container{
							Name:            "swift-ring-minion",
							Image:           "thiagodasilva/swift_ring_minion:dev-v5",
							ImagePullPolicy: v1.PullIfNotPresent,
							VolumeMounts:    mounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	deployments := st.client.Extensions().Deployments(namespace)
	_, err := deployments.Create(proxyDeploy)
	if apierrors.IsAlreadyExists(err) {
		return nil
	} else if err != nil {
		logger.Err(err)
	}

	// Wait until deployment ready
	err = waitForDeploymentFn(st.client, namespace, proxyDeploy.GetName(),
		*proxyDeploy.Spec.Replicas)
	if err != nil {
		return logger.Err(err)
	}

	logger.Debug("swift-proxy pod deployed")
	return nil
}

func (st *SwiftStorage) deploySwiftProxyService(namespace string) error {
	s := &v1.Service{
		ObjectMeta: meta.ObjectMeta{
			Name:      "swiftservice",
			Namespace: namespace,
			Labels: map[string]string{
				"swift": "swift-service",
			},
			Annotations: map[string]string{
				"description": "Exposes Swift Proxy Service",
			},
		},
		Spec: v1.ServiceSpec{
			Selector: map[string]string{
				"swift": "swift-proxy",
			},
			Type: v1.ServiceTypeNodePort,
			Ports: []v1.ServicePort{
				v1.ServicePort{
					Port: 8080,
					TargetPort: intstr.IntOrString{
						IntVal: 8080,
					},
				},
			},
		},
	}

	// Submit the service
	services := st.client.Core().Services(namespace)
	_, err := services.Create(s)
	if apierrors.IsAlreadyExists(err) {
		return nil
	} else if err != nil {
		logger.Err(err)
		return err
	}

	logger.Debug("swift proxy service created")
	return nil
}

func (st *SwiftStorage) createRings(c *spec.StorageCluster) error {
	// Create configMap with cluster topology
	err := st.createConfigMap(c)
	if err != nil {
		return err
	}

	volumes := []v1.Volume{
		v1.Volume{
			Name: "config-swift-cluster",
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: "swift-cluster-configmap"},
					Items: []v1.KeyToPath{{
						Key:  "cluster.json",
						Path: "cluster_topology.json",
					}},
				},
			},
		},
	}

	mounts := []v1.VolumeMount{
		v1.VolumeMount{
			Name:      "config-swift-cluster",
			MountPath: "/etc/swift_config",
		},
	}

	replicas := int32(1)
	ringMasterDeploy := &v1beta1.Deployment{
		ObjectMeta: meta.ObjectMeta{
			Name:      "swift-ring-master-deploy",
			Namespace: c.Namespace,
			Annotations: map[string]string{
				"description": "Deployment spec for Swift Ring Master",
			},
			Labels: map[string]string{
				"swift":         "swift-ring-master",
				"quartermaster": "swift",
			},
		},
		Spec: v1beta1.DeploymentSpec{
			Replicas: &replicas,
			Template: v1.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{
						"swift":         "swift-ring-master",
						"quartermaster": "swift",
					},
					Name: "swift-ring-master-pod",
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						v1.Container{
							Name:            "swift-ring-master",
							Image:           "thiagodasilva/swift_ring_master:dev-v1",
							ImagePullPolicy: v1.PullIfNotPresent,
							VolumeMounts:    mounts,
							Ports: []v1.ContainerPort{
								v1.ContainerPort{
									ContainerPort: 8090,
								},
							},
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	deployments := st.client.Extensions().Deployments(c.Namespace)
	_, err = deployments.Create(ringMasterDeploy)
	if apierrors.IsAlreadyExists(err) {
		return nil
	} else if err != nil {
		logger.Err(err)
	}

	// Wait until deployment ready
	err = waitForDeploymentFn(st.client, c.Namespace,
		ringMasterDeploy.GetName(), *ringMasterDeploy.Spec.Replicas)
	if err != nil {
		return logger.Err(err)
	}

	err = st.deploySwiftRingMasterService(c.Namespace)
	if err != nil {
		return logger.Err(err)
	}

	logger.Debug("rings master deploy created")

	return nil
}

func (st *SwiftStorage) deploySwiftRingMasterService(namespace string) error {
	s := &v1.Service{
		ObjectMeta: meta.ObjectMeta{
			Name:      "swift-ring-master-svc",
			Namespace: namespace,
			Labels: map[string]string{
				"swift": "swift-ring-master-svc",
			},
			Annotations: map[string]string{
				"description": "Exposes Swift Ring Master Service",
			},
		},
		Spec: v1.ServiceSpec{
			Selector: map[string]string{
				"swift": "swift-ring-master",
			},
			ClusterIP: "10.96.0.248", //10.96.253.129
			Type:      v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{
				v1.ServicePort{
					Port: 8090,
					TargetPort: intstr.IntOrString{
						IntVal: 8090,
					},
				},
			},
		},
	}

	// Submit the service
	services := st.client.Core().Services(namespace)
	_, err := services.Create(s)
	if apierrors.IsAlreadyExists(err) {
		return nil
	} else if err != nil {
		logger.Err(err)
		return err
	}

	logger.Debug("swift ring master service created")
	return nil
}

func (st *SwiftStorage) createConfigMap(c *spec.StorageCluster) error {
	cluster, _ := json.Marshal(c)
	clusterConfMap := &v1.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name: "swift-cluster-configmap",
		},
		Data: map[string]string{
			"cluster.json": string(cluster),
		},
	}
	configMaps := st.client.Core().ConfigMaps(c.Namespace)
	_, err := configMaps.Create(clusterConfMap)
	if apierrors.IsAlreadyExists(err) {
		return nil
	} else if err != nil {
		logger.Err(err)
		return err
	}
	logger.Debug("created config map")
	return nil
}
