/*
Copyright 2024.

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

package controller

import (
	"context"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// GoodnightDeploymentReconciler reconciles a GoodnightDeployment object
type GoodnightDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	WatchList
}

type WatchList struct {
	sync.RWMutex
	Deployments map[string]appsv1.Deployment
}

// +kubebuilder:rbac:groups=goodnight-kube.joshmeyer.dev,resources=goodnightdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=goodnight-kube.joshmeyer.dev,resources=goodnightdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=goodnight-kube.joshmeyer.dev,resources=goodnightdeployments/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the GoodnightDeployment object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *GoodnightDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, req.NamespacedName, deployment)
	if err != nil {
		if errors.IsNotFound(err) {
			delete(r.Deployments, req.Name)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to retrieve deployment")
			return ctrl.Result{}, err
		}
	}
	r.Lock()
	defer r.Unlock()
	if hasAnnotation(deployment) {
		r.Deployments[req.Name] = *deployment
		log.Info("adding deployment to watch list")
	} else if _, ok := r.Deployments[req.Name]; ok {
		log.Info("removing deployment from watch list")
		delete(r.Deployments, req.Name)
	}

	return ctrl.Result{}, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *GoodnightDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		For(&appsv1.Deployment{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return hasAnnotation(e.Object)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return hasAnnotation(e.ObjectOld)
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return hasAnnotation(e.Object)
			},
		}).
		Complete(r)
}

func (r *GoodnightDeploymentReconciler) LoadDeployments(ctx context.Context) {
	log := log.FromContext(ctx)

	log.Info("Initializing")
	deployments := &appsv1.DeploymentList{}

	err := r.Client.List(ctx, deployments)
	if err != nil {
		log.Error(err, "Unable to load initial deployments")
	}

	for _, d := range deployments.Items {
		if !hasAnnotation(&d) {
			continue
		}
		r.Lock()
		defer r.Unlock()
		log.Info("Loading deployment", "name", d.Name)
		r.Deployments[d.Name] = d
	}
}

func (r *GoodnightDeploymentReconciler) Monitor(ctx context.Context) {
	log := log.FromContext(ctx)

	ticker := time.NewTicker(time.Second * 10)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Info("checking if the deployments are sleepy")
			r.RLock()
			defer r.RUnlock()

			for _, d := range r.Deployments {
				log.Info("Deployment", "name", d.Name, "time", timeAnnotation(ctx, &d, "goodnight.joshmeyer.dev/managed-by"))
			}
		case <-ctx.Done():
			return
		}
	}
}

func hasAnnotation(e client.Object) bool {
	annotations := e.GetAnnotations()
	if value, ok := annotations["goodnight.joshmeyer.dev/managed-by"]; ok {
		return value == "true"
	}
	return false
}

func timeAnnotation(ctx context.Context, e client.Object, annotation string) time.Time {
	log := log.FromContext(ctx)

	annotations := e.GetAnnotations()
	if value, ok := annotations[annotation]; ok {
		t, err := time.Parse(value, time.TimeOnly)
		if err != nil {
			log.Error(err, "unable to parse sleep time", "annotation", annotation)
			return time.Time{}
		}
		return t
	}
	return time.Time{}
}
