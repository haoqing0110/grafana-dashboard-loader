// Copyright (c) 2021 Red Hat, Inc.

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"

	"github.com/open-cluster-management/grafana-dashboard-loader/pkg/util"
)

// DashboardLoader ...
type DashboardLoader struct {
	coreClient corev1client.CoreV1Interface
	informer   cache.SharedIndexInformer
}

var (
	grafanaURI = "http://127.0.0.1:3001"
	//retry on errors
	retry = 10
)

// RunGrafanaDashboardController ...
func RunGrafanaDashboardController(stop <-chan struct{}) {
	config, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		klog.Error("Failed to get cluster config", "error", err)
	}
	// Build kubeclient client and informer for managed cluster
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal("Failed to build kubeclient", "error", err)
	}

	go newKubeInformer(kubeClient.CoreV1()).Run(stop)
	<-stop
}

func isDesiredDashboardConfigmap(obj interface{}) bool {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || cm == nil {
		return false
	}

	labels := cm.ObjectMeta.Labels
	if strings.ToLower(labels["grafana-custom-dashboard"]) == "true" {
		return true
	}

	owners := cm.GetOwnerReferences()
	for _, owner := range owners {
		if strings.Contains(cm.Name, "grafana-dashboard") && owner.Kind == "MultiClusterObservability" {
			return true
		}
	}

	return false
}

func newKubeInformer(coreClient corev1client.CoreV1Interface) cache.SharedIndexInformer {
	// get watched namespace
	watchedNS := os.Getenv("POD_NAMESPACE")
	watchlist := &cache.ListWatch{
		ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
			return coreClient.ConfigMaps(watchedNS).List(context.TODO(), metav1.ListOptions{})
		},
		WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) {
			return coreClient.ConfigMaps(watchedNS).Watch(context.TODO(), metav1.ListOptions{})
		},
	}
	kubeInformer := cache.NewSharedIndexInformer(
		watchlist,
		&corev1.ConfigMap{},
		time.Second,
		cache.Indexers{},
	)

	kubeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if !isDesiredDashboardConfigmap(obj) {
				return
			}
			klog.Infof("detect there is a new dashboard %v created", obj.(*corev1.ConfigMap).Name)
			updateDashboard(obj, false)
		},
		UpdateFunc: func(old, new interface{}) {
			if !isDesiredDashboardConfigmap(new) {
				return
			}
			if !reflect.DeepEqual(old.(*corev1.ConfigMap).Data, new.(*corev1.ConfigMap).Data) {
				klog.Infof("detect there is a dashboard %v updated", new.(*corev1.ConfigMap).Name)
				updateDashboard(new, false)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if !isDesiredDashboardConfigmap(obj) {
				return
			}
			klog.Infof("detect there is a dashboard %v deleted", obj.(*corev1.ConfigMap).Name)
			deleteDashboard(obj)
		},
	})

	return kubeInformer
}

func hasCustomFolder(folderTitle string) float64 {
	grafanaURL := grafanaURI + "/api/folders"
	body, _ := util.SetRequest("GET", grafanaURL, nil, retry)

	folders := []map[string]interface{}{}
	err := json.Unmarshal(body, &folders)
	if err != nil {
		klog.Error("Failed to unmarshall response body", "error", err)
		return 0
	}

	for _, folder := range folders {
		if folder["title"] == folderTitle {
			return folder["id"].(float64)
		}
	}
	return 0
}

func createCustomFolder(folderTitle string) float64 {
	folderID := hasCustomFolder(folderTitle)
	if folderID == 0 {
		grafanaURL := grafanaURI + "/api/folders"
		body, _ := util.SetRequest("POST", grafanaURL, strings.NewReader("{\"title\":\""+folderTitle+"\"}"), retry)
		folder := map[string]interface{}{}
		err := json.Unmarshal(body, &folder)
		if err != nil {
			klog.Error("Failed to unmarshall response body", "error", err)
			return 0
		}
		return folder["id"].(float64)
	}
	return folderID
}

// updateDashboard is used to update the customized dashboards via calling grafana api
func updateDashboard(obj interface{}, overwrite bool) {
	folderID := 0.0
	labels := obj.(*corev1.ConfigMap).ObjectMeta.Labels
	if labels["general-folder"] == "" || strings.ToLower(labels["general-folder"]) != "true" {
		annotations := obj.(*corev1.ConfigMap).ObjectMeta.Annotations
		folderTitle, ok := annotations["observability.open-cluster-management.io/dashboard-folder"]
		if !ok || folderTitle == "" {
			folderTitle = "Custom"
		}

		folderID = createCustomFolder(folderTitle)
		if folderID == 0 {
			klog.Error("Failed to get custom folder id")
			return
		}
	}
	for _, value := range obj.(*corev1.ConfigMap).Data {

		dashboard := map[string]interface{}{}
		err := json.Unmarshal([]byte(value), &dashboard)
		if err != nil {
			klog.Error("Failed to unmarshall data", "error", err)
			return
		}
		if dashboard["uid"] == nil {
			dashboard["uid"], _ = util.GenerateUID(obj.(*corev1.ConfigMap).GetName(),
				obj.(*corev1.ConfigMap).GetNamespace())
		}
		dashboard["id"] = nil
		data := map[string]interface{}{
			"folderId":  folderID,
			"overwrite": overwrite,
			"dashboard": dashboard,
		}

		b, err := json.Marshal(data)
		if err != nil {
			klog.Error("failed to marshal body", "error", err)
			return
		}

		grafanaURL := grafanaURI + "/api/dashboards/db"
		body, respStatusCode := util.SetRequest("POST", grafanaURL, bytes.NewBuffer(b), retry)

		if respStatusCode != http.StatusOK {
			if respStatusCode == http.StatusPreconditionFailed {
				if strings.Contains(string(body), "version-mismatch") {
					updateDashboard(obj, true)
				} else if strings.Contains(string(body), "name-exists") {
					klog.Info("the dashboard name already existed")
				} else {
					klog.Infof("failed to create/update: %v", respStatusCode)
				}
			} else {
				klog.Infof("failed to create/update: %v", respStatusCode)
			}
		} else {
			klog.Info("Dashboard created/updated")
		}
	}

}

// DeleteDashboard ...
func deleteDashboard(obj interface{}) {
	for _, value := range obj.(*corev1.ConfigMap).Data {

		dashboard := map[string]interface{}{}
		err := json.Unmarshal([]byte(value), &dashboard)
		if err != nil {
			klog.Error("Failed to unmarshall data", "error", err)
			return
		}

		uid, _ := util.GenerateUID(obj.(*corev1.ConfigMap).Name, obj.(*corev1.ConfigMap).Namespace)
		if dashboard["uid"] != nil {
			uid = dashboard["uid"].(string)
		}

		grafanaURL := grafanaURI + "/api/dashboards/uid/" + uid

		_, respStatusCode := util.SetRequest("DELETE", grafanaURL, nil, retry)
		if respStatusCode != http.StatusOK {
			klog.Errorf("failed to delete dashboard %v with %v", obj.(*corev1.ConfigMap).Name, respStatusCode)
		} else {
			klog.Info("Dashboard deleted")
		}
	}
	return
}