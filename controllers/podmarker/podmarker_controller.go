/*
Copyright 2022.

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

package podmarker

import (
	"bytes"
	"context"
	"encoding/base64"
	"math"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/jsonpath"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	podmarkerv1 "kube-stack.me/apis/podmarker/v1"
)

// PodMarkerReconciler reconciles a PodMarker object
type PodMarkerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type PodMarkCount struct {
	Pod   *corev1.Pod
	Count int
}

var (
	llog = ctrl.Log.WithName("podMarkerReconciler")
)

//+kubebuilder:rbac:groups=podmarker.kube-stack.me,resources=podmarkers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=podmarker.kube-stack.me,resources=podmarkers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=podmarker.kube-stack.me,resources=podmarkers/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PodMarker object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *PodMarkerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	var podMarkers podmarkerv1.PodMarkerList
	if err := r.List(ctx, &podMarkers, client.InNamespace(req.Namespace)); err != nil {
		llog.Error(err, "unable to list podMarkers")
		return ctrl.Result{}, err
	}

	// add labels
	for _, podMarker := range podMarkers.Items {
		var pods corev1.PodList
		if err := r.List(ctx, &pods, client.InNamespace(req.Namespace), client.MatchingLabels(podMarker.Spec.Selector.MatchLabels)); err != nil {
			llog.Error(err, "unable to list pods")
			return ctrl.Result{}, err
		}

		for i := range pods.Items {
			for key, val := range podMarker.Spec.AddLabels {
				pods.Items[i].Labels[key] = extractValueByJsonPath(&pods.Items[i], val)
			}
			if err := r.Update(ctx, &pods.Items[i]); err != nil {
				llog.Error(err, "update pod")
				return ctrl.Result{}, err
			}
		}
	}

	// mark labels
	for _, podMarker := range podMarkers.Items {
		for _, markLabel := range podMarker.Spec.MarkLabels {
			var pods corev1.PodList
			if err := r.List(ctx, &pods, client.InNamespace(req.Namespace), client.MatchingLabels(podMarker.Spec.Selector.MatchLabels), client.MatchingLabels(markLabel.Labels)); err != nil {
				llog.Error(err, "unable to list pods")
				return ctrl.Result{}, err
			}

			readyCount := r.readyCount(&pods)
			if readyCount < markLabel.Replicas {
				plist, err := r.sortPods(ctx, req.Namespace, &podMarker)
				if err != nil {
					llog.Error(err, "sortPods")
					return ctrl.Result{}, err
				}
				for i := 0; i < markLabel.Replicas-readyCount && i < len(plist); i++ {
					for k, v := range markLabel.Labels {
						plist[i].Pod.Labels[k] = v
					}
					if err := r.Update(ctx, plist[i].Pod); err != nil {
						llog.Error(err, "update pod")
						return ctrl.Result{}, err
					}
				}
			} else if readyCount > markLabel.Replicas {
				plist, err := r.sortPods(ctx, req.Namespace, &podMarker)
				if err != nil {
					llog.Error(err, "sortPods")
					return ctrl.Result{}, err
				}
				for i := len(plist) - 1; i >= 0 && len(plist)-i <= readyCount-markLabel.Replicas; i-- {
					for k := range markLabel.Labels {
						delete(plist[i].Pod.Labels, k)
					}
					if e := r.Update(ctx, plist[i].Pod); e != nil {
						llog.Error(e, "update error")
						return ctrl.Result{}, e
					}
				}
			} else {
				plist, err := r.sortPods(ctx, req.Namespace, &podMarker)
				if err != nil {
					llog.Error(err, "sortPods")
					return ctrl.Result{}, err
				}
				if len(plist) > 1 && plist[len(plist)-1].Count > int(math.Ceil(totalReplica(&podMarker)/float64(len(plist)))) {
					for k, v := range markLabel.Labels {
						plist[0].Pod.Labels[k] = v
					}
					if err := r.Update(ctx, plist[0].Pod); err != nil {
						llog.Error(err, "update pod")
						return ctrl.Result{}, err
					}
				}
			}
		}
	}
	return ctrl.Result{}, nil
}

func totalReplica(pm *podmarkerv1.PodMarker) float64 {
	r := 0
	for i := range pm.Spec.MarkLabels {
		r += pm.Spec.MarkLabels[i].Replicas
	}
	return float64(r)
}

func (r *PodMarkerReconciler) sortPods(ctx context.Context, namespace string, pm *podmarkerv1.PodMarker) ([]*PodMarkCount, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels(pm.Spec.Selector.MatchLabels)); err != nil {
		llog.Error(err, "unable to list pods")
		return nil, err
	}

	pc := make([]*PodMarkCount, 0)
	for i := range pods.Items {
		if !r.isReady(&pods.Items[i]) {
			continue
		}
		count := 0
		for _, markLabel := range pm.Spec.MarkLabels {
			if labels.Set(markLabel.Labels).AsSelector().Matches(labels.Set(pods.Items[i].Labels)) {
				count++
			}
		}
		pc = append(pc, &PodMarkCount{Pod: &pods.Items[i], Count: count})
	}
	sort.SliceStable(pc, func(i, j int) bool {
		return pc[i].Count < pc[j].Count
	})
	return pc, nil
}

func (r *PodMarkerReconciler) readyCount(pods *corev1.PodList) int {
	result := 0
	for i := range pods.Items {
		if r.isReady(&pods.Items[i]) {
			result++
		}
	}
	return result
}

func (r *PodMarkerReconciler) isReady(pod *corev1.Pod) bool {
	if r.hasReadyContition(pod) && pod.Status.Phase == corev1.PodRunning && pod.GetDeletionTimestamp() == nil {
		return true
	}
	return false
}

func (r *PodMarkerReconciler) hasReadyContition(pod *corev1.Pod) bool {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReady && pod.Status.Conditions[i].Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func extractValueByJsonPath(pod *corev1.Pod, jsonPathExpr string) string {
	var (
		err    error
		unstct map[string]interface{}
	)

	if unstct, err = runtime.DefaultUnstructuredConverter.ToUnstructured(pod); err != nil {
		llog.Error(err, "runtime.DefaultUnstructuredConverter.ToUnstructured(pod)")
		return ""
	}

	j := jsonpath.New("")
	j.AllowMissingKeys(true)
	if err = j.Parse(jsonPathExpr); err != nil {
		llog.Error(err, "jsonpath parse err")
		return ""
	}
	buf := new(bytes.Buffer)
	if err = j.Execute(buf, unstct); err != nil {
		llog.Error(err, "jsonpath exec err")
		return ""
	}

	if len(validation.IsValidLabelValue(buf.String())) <= 0 {
		return buf.String()
	}
	return strings.Replace(base64.StdEncoding.EncodeToString(buf.Bytes()), "=", "", -1)
}

func (r *PodMarkerReconciler) findObjectForPodMaker(pod client.Object) []reconcile.Request {
	reqs := make([]reconcile.Request, 0)
	reqs = append(reqs,
		reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: pod.GetNamespace(),
			},
		},
	)
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodMarkerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&podmarkerv1.PodMarker{}).
		Watches(
			&source.Kind{Type: &corev1.Pod{}},
			handler.EnqueueRequestsFromMapFunc(r.findObjectForPodMaker),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Complete(r)
}
