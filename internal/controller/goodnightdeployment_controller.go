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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samber/lo"
	"github.com/spf13/cast"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	TimeLayout = "15:04"

	GoodnightPrefix = "goodnight.joshmeyer.dev"

	AwakeTimeLabel     = "goodnight.joshmeyer.dev/awake-time"
	AwakeReplicasLabel = "goodnight.joshmeyer.dev/awake-replicas"
	SleepTimeLabel     = "goodnight.joshmeyer.dev/sleep-time"
	SleepReplicasLabel = "goodnight.joshmeyer.dev/sleep-replicas"

	IsSleepingLabel = "goodnight.joshmeyer.dev/sleeping"
)

// GoodnightDeploymentReconciler reconciles a GoodnightDeployment object
type GoodnightDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	SleepList
}

type SleepList struct {
	sync.RWMutex
	SleepyDeployments map[string]SleepyDeployment
}

type SleepyDeployment struct {
	Deployment    *appsv1.Deployment
	AwakeTime     time.Time
	AwakeReplicas int32
	SleepTime     time.Time
	SleepReplicas int32
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
			delete(r.SleepyDeployments, req.Name)
			return ctrl.Result{}, nil
		} else {
			log.Error(err, "failed to retrieve deployment")
			return ctrl.Result{}, err
		}
	}
	r.Lock()
	defer r.Unlock()
	s, isSleepy := loadSleepyDeployment(ctx, deployment)
	_, loadedDeployment := r.SleepyDeployments[req.Name]

	if !isSleepy {
		if loadedDeployment {
			log.Info("removing deployment from watch list", "deployment", req.Name)
			delete(r.SleepyDeployments, req.Name)
		}
		return ctrl.Result{}, nil
	}
	if !loadedDeployment {
		log.Info("adding deployment to watch list", "deployment", req.Name)
	} else {
		log.Info("updating deployment in watch list", "deployment", req.Name)
	}
	r.SleepyDeployments[req.Name] = s
	r.EnterSandman(ctx, s)
	return ctrl.Result{}, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *GoodnightDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		For(&appsv1.Deployment{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return hasGoodnightAnnotation(e.Object)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				return hasGoodnightAnnotation(e.ObjectOld)
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return hasGoodnightAnnotation(e.Object)
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
		if !hasGoodnightAnnotation(&d) {
			continue
		}
		r.Lock()
		defer r.Unlock()
		log.Info("Loading deployment", "name", d.Name)
		s, isSleepy := loadSleepyDeployment(ctx, &d)
		if isSleepy {
			r.SleepyDeployments[d.Name] = s
		} else {

		}
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

			for _, d := range r.SleepyDeployments {
				err := r.EnterSandman(ctx, d)
				if err != nil {
					log.Error(err, "failed to handle deployment", "deployment", d.Deployment.Name)
				}
			}
			r.RUnlock()
		case <-ctx.Done():
			return
		}
	}
}

func (r *GoodnightDeploymentReconciler) EnterSandman(ctx context.Context, s SleepyDeployment) error {
	log := log.FromContext(ctx)
	t := time.Now()
	ns, _ := time.Parse(TimeLayout, strconv.Itoa(t.Hour())+":"+strconv.Itoa(t.Minute()))

	isBedTime := inTimeSpan(s.SleepTime, s.AwakeTime, ns)
	targetReplicas := s.AwakeReplicas
	if isBedTime {
		targetReplicas = s.SleepReplicas
	}
	if (isBedTime && lo.FromPtr(s.Deployment.Spec.Replicas) == targetReplicas) || (!isBedTime && lo.FromPtr(s.Deployment.Spec.Replicas) == targetReplicas) {
		return nil
	}

	s.Deployment.Spec.Replicas = &targetReplicas
	s.Deployment.Annotations[IsSleepingLabel] = cast.ToString(isBedTime)
	if isBedTime {
		log.Info("putting deployment to sleep", "deployment", s.Deployment.Name)
	} else {
		log.Info("waking deployment up", "deployment", s.Deployment.Name)
	}
	return r.Client.Update(ctx, s.Deployment)
}

func hasGoodnightAnnotation(e client.Object) bool {
	for k := range e.GetAnnotations() {
		if strings.HasPrefix(k, GoodnightPrefix) {
			return true
		}
	}
	return false
}

func loadSleepyDeployment(ctx context.Context, deployment *appsv1.Deployment) (SleepyDeployment, bool) {
	logger := log.FromContext(ctx)

	annotations := deployment.GetAnnotations()

	sleepTime, err := timeAnnotation(annotations, SleepTimeLabel)
	if err != nil {
		logger.Error(err, "failed to parse annotation", "annotation", SleepTimeLabel)
		return SleepyDeployment{}, false
	}

	sleepReplicas, err := replicaAnnotation(annotations, SleepReplicasLabel)
	if err != nil {
		logger.Error(err, "failed to parse annotation", "annotation", SleepReplicasLabel)
		return SleepyDeployment{}, false
	}

	awakeTime, err := timeAnnotation(annotations, AwakeTimeLabel)
	if err != nil {
		logger.Error(err, "failed to parse annotation", "annotation", AwakeTimeLabel)
		return SleepyDeployment{}, false
	}

	awakeReplicas, err := replicaAnnotation(annotations, AwakeReplicasLabel)
	if err != nil {
		logger.Error(err, "failed to parse annotation", "annotation", AwakeReplicasLabel)
		return SleepyDeployment{}, false
	}

	if !sleepTime.IsZero() && !awakeTime.IsZero() {
		return SleepyDeployment{
			Deployment:    deployment,
			AwakeTime:     awakeTime,
			AwakeReplicas: awakeReplicas,
			SleepTime:     sleepTime,
			SleepReplicas: sleepReplicas,
		}, true
	}

	return SleepyDeployment{}, false
}

func timeAnnotation(annotations map[string]string, label string) (time.Time, error) {
	if value, ok := annotations[label]; ok {
		t, err := time.Parse(TimeLayout, value)
		if err != nil {
			return time.Time{}, err
		}
		return t, nil
	}
	return time.Time{}, errors.NewBadRequest("missing annotation")
}

func replicaAnnotation(annotations map[string]string, label string) (int32, error) {
	if value, ok := annotations[label]; ok {
		t, err := strconv.Atoi(value)
		if err != nil {
			return 0, err
		}
		return int32(t), err
	}
	return 0, errors.NewBadRequest("missing annotation")
}

func inTimeSpan(start, end, check time.Time) bool {
	if start.Before(end) {
		return !check.Before(start) && !check.After(end)
	}
	if start.Equal(end) {
		return check.Equal(start)
	}
	return !start.After(check) || !end.Before(check)
}
