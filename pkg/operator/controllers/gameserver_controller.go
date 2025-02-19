/*
Copyright 2021.

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

package controllers

import (
	"context"
	"runtime"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	mpsv1alpha1 "github.com/playfab/thundernetes/pkg/operator/api/v1alpha1"
)

var (
	ownerKey = ".metadata.controller"
	apiGVStr = mpsv1alpha1.GroupVersion.String()

	podsUnderCreation = sync.Map{}
)

const SafeToEvictPodAttribute string = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// GameServerReconciler reconciles a GameServer object
type GameServerReconciler struct {
	client.Client
	Scheme                  *k8sruntime.Scheme
	Recorder                record.EventRecorder
	PortRegistry            *PortRegistry
	InitContainerImageLinux string
	InitContainerImageWin   string
	GetNodeDetailsProvider  func(ctx context.Context, r client.Reader, nodeName string) (string, string, int, error) // we abstract this for testing purposes
}

// NewGameServerReconciler returns a pointer to a new GameServerReconciler
func NewGameServerReconciler(mgr manager.Manager,
	portRegistry *PortRegistry,
	getNodeDetailsProvider func(ctx context.Context, r client.Reader, nodeName string) (string, string, int, error),
	initContainerImageLinux string,
	initContainerImageWin string) *GameServerReconciler {
	return &GameServerReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		PortRegistry:            portRegistry,
		Recorder:                mgr.GetEventRecorderFor("GameServer"),
		GetNodeDetailsProvider:  getNodeDetailsProvider,
		InitContainerImageLinux: initContainerImageLinux,
		InitContainerImageWin:   initContainerImageWin,
	}
}

// we request secret RBAC access here so they can be potentially used by the allocation API service (for GameServer allocations)
// the gameserverapi uses the same manager role, so we need to add get, list and watch for gameserverdetails

//+kubebuilder:rbac:groups=mps.playfab.com,resources=gameservers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=mps.playfab.com,resources=gameserverdetails,verbs=get;list;watch
//+kubebuilder:rbac:groups=mps.playfab.com,resources=gameservers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=mps.playfab.com,resources=gameservers/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *GameServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var gs mpsv1alpha1.GameServer
	if err := r.Get(ctx, req.NamespacedName, &gs); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Unable to fetch GameServer, it has probably been deleted. Trying to deregister ports")
			ports, err := r.PortRegistry.DeregisterPorts(req.Namespace, req.Name)
			if err != nil {
				return ctrl.Result{}, err
			}
			if len(ports) > 0 {
				log.V(1).Info("Deregistered ports", "ports", ports)
			}
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch GameServer")
		return ctrl.Result{}, err
	}

	// get the pod that is owned by this GameServer
	var pod corev1.Pod
	podFoundInCache := true
	if err := r.Get(ctx, types.NamespacedName{Namespace: gs.Namespace, Name: gs.Name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			podFoundInCache = false
		} else {
			// there has been an error other than NotFound
			return ctrl.Result{}, err
		}
	}

	_, podUnderCreation := podsUnderCreation.Load(gs.Name)
	// we have zero pods for this game server and we have recorded that one is being created
	if !podFoundInCache && podUnderCreation {
		// pod is being created, cache hasn't been updated yet
		return ctrl.Result{}, nil
	} else if podUnderCreation {
		podsUnderCreation.Delete(gs.Name)
	}

	if !podFoundInCache {
		log.Info("Creating a new pod for GameServer", GameServerKind, gs.Name)
		newPod := NewPodForGameServer(&gs, r.InitContainerImageLinux, r.InitContainerImageWin)
		if err := r.Create(ctx, newPod); err != nil {
			return ctrl.Result{}, err
		}
		podsUnderCreation.Store(gs.Name, struct{}{})
		r.Recorder.Eventf(&gs, corev1.EventTypeNormal, "Created", "Created new pod %s for GameServer %s", newPod.Name, gs.Name)
		return ctrl.Result{}, nil
	}

	// check if the pod process has exited (i.e. GameServer session has exited gracefully or crashed)
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if !containerStatus.Ready && containerStatus.State.Terminated != nil {
			exitCode := containerStatus.State.Terminated.ExitCode
			r.Recorder.Eventf(&gs, corev1.EventTypeNormal, "GameServerProcessExited", "GameServer process exited with code %d", exitCode)
			patch := client.MergeFrom(gs.DeepCopy())
			if exitCode == 0 {
				gs.Status.State = mpsv1alpha1.GameServerStateGameCompleted
			} else {
				gs.Status.State = mpsv1alpha1.GameServerStateCrashed
			}
			// updating GameServer with the new state
			if err := r.Status().Patch(ctx, &gs, patch); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// if a game server is active, there are players present.
	// When using the cluster autoscaler, an annotation will be added
	// to prevent the node from being scaled down.
	r.Recorder.Eventf(&gs, corev1.EventTypeNormal, "Update", "Gameserver %s state is %s, health is %s", gs.Name, gs.Status.State, gs.Status.Health)
	err := r.addSafeToEvictAnnotationIfNecessary(ctx, &gs, &pod)
	if err != nil {
		return ctrl.Result{}, err
	}

	// if we don't have a Public IP set, we need to get and set it on the status
	if gs.Status.PublicIP == "" {
		if pod.Spec.NodeName == "" {
			// nodename is empty, maybe the Pod hasn't been scheduled yet?
			return ctrl.Result{}, nil // will requeue when the Pod is scheduled
		}
		nodeName, publicIP, nodeAgeInDays, err := r.GetNodeDetailsProvider(ctx, r, pod.Spec.NodeName)
		if err != nil {
			return ctrl.Result{}, err
		}

		patch := client.MergeFrom(gs.DeepCopy())
		gs.Status.PublicIP = publicIP
		gs.Status.Ports = getContainerHostPortTuples(&pod)
		gs.Status.NodeAge = nodeAgeInDays
		gs.Status.NodeName = nodeName
		err = r.Status().Patch(ctx, &gs, patch)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// we're adding the Label here so the DaemonSet watch can get the update information about the GameServer
	// unfortunately, we can't track CRDs on a Watch via .status yet. If this was the case, we could PATCH the NodeName via the patch code above
	// https://github.com/kubernetes/kubernetes/issues/53459
	if _, exists := gs.Labels[LabelNodeName]; !exists {
		// code from: https://sdk.operatorframework.io/docs/building-operators/golang/references/client/#patch
		// also: https://github.com/coderanger/controller-utils/blob/main/core/reconciler.go#L306-L330
		patch := client.MergeFrom(gs.DeepCopy())
		if gs.Labels == nil {
			gs.Labels = make(map[string]string)
		}
		gs.Labels[LabelNodeName] = pod.Spec.NodeName
		err := r.Patch(ctx, &gs, patch)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// object was probably deleted, no reason to reconcile again
				log.Info("Trying to update Labels for deleted GameServer: " + err.Error())
				return ctrl.Result{}, nil
			} else {
				log.Error(err, "Error updating GameServer labels")
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GameServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, ownerKey, func(rawObj client.Object) []string {
		// grab the Pod object, extract the owner...
		pod := rawObj.(*corev1.Pod)
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			return nil
		}
		// ...make sure it's a GameServer...
		if owner.APIVersion != apiGVStr || owner.Kind != "GameServer" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&mpsv1alpha1.GameServer{}).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: runtime.NumCPU(),
		}).
		Complete(r)
}

// addSafeToEvictAnnotationIfNecessary will set the safe-to-evict attribute to true on Initalizing or StandingBy GameServers
// and will set it to false for Active (so they don't go down during a potential cluster scale down)
func (r *GameServerReconciler) addSafeToEvictAnnotationIfNecessary(ctx context.Context, gs *mpsv1alpha1.GameServer, pod *corev1.Pod) error {
	// we don't need to check if pod.ObjectMeta.Annotations is nil since the check below accommodates for that
	// https://go.dev/play/p/O9QmzPnKsOK
	if gs.Status.State == mpsv1alpha1.GameServerStateInitializing || gs.Status.State == mpsv1alpha1.GameServerStateStandingBy {
		if _, ok := pod.ObjectMeta.Annotations[SafeToEvictPodAttribute]; !ok {
			return r.patchPodSafeToEvictAnnotation(ctx, pod, true)
		}
	} else if gs.Status.State == mpsv1alpha1.GameServerStateActive {
		val, ok := pod.ObjectMeta.Annotations[SafeToEvictPodAttribute]
		if !ok || val == strconv.FormatBool(true) {
			return r.patchPodSafeToEvictAnnotation(ctx, pod, false)
		}
	}
	return nil
}

// patchPodSafeToEvictAnnotation will set the safeToEvictPodAttribute annotation on the Pod
func (r *GameServerReconciler) patchPodSafeToEvictAnnotation(ctx context.Context, pod *corev1.Pod, safeToEvict bool) error {
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.ObjectMeta.Annotations == nil {
		pod.ObjectMeta.Annotations = map[string]string{}
	}
	pod.ObjectMeta.Annotations[SafeToEvictPodAttribute] = strconv.FormatBool(safeToEvict)
	err := r.Patch(ctx, pod, patch)
	if err != nil {
		return err
	}
	return nil
}
