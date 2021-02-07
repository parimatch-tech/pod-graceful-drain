package core

import (
	"context"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/targetgroupbinding"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"strings"
	"time"
)

const (
	fallbackAdmissionDelayTimeout         = 30 * time.Second
	admissionDelayOverhead                = 2 * time.Second
	defaultPodGracefulDrainCleanupTimeout = 10 * time.Second
)

type PodGracefulDrain struct {
	k8sClient client.Client
	logger    logr.Logger
	config    *PodGracefulDrainConfig
	delayer   Delayer
}

var _ manager.Runnable = &PodGracefulDrain{}

func NewPodGracefulDrain(k8sClient client.Client, logger logr.Logger, config *PodGracefulDrainConfig) PodGracefulDrain {
	return PodGracefulDrain{
		k8sClient: k8sClient,
		logger:    logger.WithName("pod-graceful-drain"),
		config:    config,
		delayer:   NewDelayer(logger),
	}
}

func (d *PodGracefulDrain) HandlePodRemove(ctx context.Context, pod *corev1.Pod) (*InterceptedAdmissionResponse, error) {
	now := time.Now()
	spec, err := d.getPodDelayedRemoveSpec(ctx, pod, now)
	if err != nil || spec == nil {
		return nil, err
	}

	d.logSpec(pod, spec)

	if err := d.executeSpec(ctx, pod, spec); err != nil {
		return nil, err
	}
	return &spec.admission, nil
}

type podDelayedRemoveSpec struct {
	isolate         bool
	deleteAt        time.Time
	asyncDeleteTask DelayedTask
	sleepTask       DelayedTask
	reason          string
	admission       InterceptedAdmissionResponse
}

func (d *PodGracefulDrain) getPodDelayedRemoveSpec(ctx context.Context, pod *corev1.Pod, now time.Time) (spec *podDelayedRemoveSpec, err error) {
	if !IsPodReady(pod) {
		return nil, nil
	}

	delayInfo, err := GetPodDeletionDelayInfo(pod)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get pod deletion info")
	} else if delayInfo.Isolated {
		spec, err := d.handleReentry(ctx, pod, delayInfo, now)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to getPodDelayedRemoveSpec pod deletion reentry")
		}
		return spec, nil
	}

	shouldIntercept, err := d.shouldIntercept(ctx, pod)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to determine whether the pod deletion should be deleted")
	} else if !shouldIntercept {
		return nil, nil
	}

	shouldDeny, reason, err := d.shouldDenyAdmission(ctx, pod)
	if err != nil {
		return nil, errors.Wrap(err, "unable to determine whether it should be denied")
	} else if !shouldDeny {
		deleteAfter := getAdmissionDelayTimeout(ctx, now)
		spec = &podDelayedRemoveSpec{
			isolate:   true,
			deleteAt:  now.Add(deleteAfter),
			sleepTask: d.getSleepTask(deleteAfter),
			reason:    reason,
			admission: InterceptedAdmissionResponse{
				Allow:  true,
				Reason: "Pod deletion is delayed enough",
			},
		}
	} else {
		spec = &podDelayedRemoveSpec{
			isolate:         true,
			deleteAt:        now.Add(d.config.DeleteAfter),
			asyncDeleteTask: d.getDelayedPodRemoveTask(pod, d.config.DeleteAfter),
			reason:          reason,
			admission: InterceptedAdmissionResponse{
				Allow:  false,
				Reason: "Pod cannot be removed immediately. It will be eventually removed after waiting for the load balancer to start",
			},
		}
	}
	return
}

func getAdmissionDelayTimeout(ctx context.Context, now time.Time) time.Duration {
	timeout := fallbackAdmissionDelayTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = deadline.Sub(now) - admissionDelayOverhead
		if timeout < 0 {
			timeout = time.Duration(0)
		}
	}
	return timeout
}

func (d *PodGracefulDrain) logSpec(pod *corev1.Pod, spec *podDelayedRemoveSpec) {
	details := map[string]interface{}{}
	if spec.isolate {
		details["isolate"] = map[string]interface{}{
			"deleteAt": spec.deleteAt,
		}
	}
	if spec.asyncDeleteTask != nil {
		details["asyncDelete"] = map[string]interface{}{
			"taskId":   spec.asyncDeleteTask.GetId(),
			"duration": spec.asyncDeleteTask.GetDuration(),
		}
	}
	if spec.sleepTask != nil {
		details["sleep"] = map[string]interface{}{
			"taskId":   spec.sleepTask.GetId(),
			"duration": spec.sleepTask.GetDuration(),
		}
	}

	d.getLoggerFor(pod).Info("delayed pod remove spec",
		"details", details,
		"reason", spec.reason,
		"admission", spec.admission.Allow)
}

func (d *PodGracefulDrain) executeSpec(ctx context.Context, pod *corev1.Pod, spec *podDelayedRemoveSpec) error {
	if spec.isolate {
		d.getLoggerFor(pod).Info("isolating")
		if err := Isolate(d.k8sClient, ctx, pod, spec.deleteAt); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return errors.Wrap(err, "unable to isolate the pod")
		}
		d.getLoggerFor(pod).V(1).Info("isolated")
	}

	if spec.asyncDeleteTask != nil {
		spec.asyncDeleteTask.RunAsync()
	}

	if spec.sleepTask != nil {
		if err := spec.sleepTask.RunWait(ctx); err != nil {
			return err
		}
	}

	return nil
}

// handleReentry handles these cases:
// * apiserver immediately retried the deletion when we patched the pod and denied the admission
//   since it is indistinguishable from the collision. So it should keep deny.
// * We disabled wait sentinel label and deleted the pod, but the patch hasn't been propagated fast enough
//   so ValidatingAdmissionWebhook read the wait label of the old version
//   => deletePodAfter will retry with back-offs, so we keep denying the admission.
// * Users and controllers manually tries to delete the pod before deleteAt.
//   => User can see the admission report message. Controller should getPodDelayedRemoveSpec admission failures.
func (d *PodGracefulDrain) handleReentry(ctx context.Context, pod *corev1.Pod, info PodDeletionDelayInfo, now time.Time) (spec *podDelayedRemoveSpec, err error) {
	if !info.Wait {
		return nil, nil
	}

	remainingTime := info.GetRemainingTime(now)
	if remainingTime == time.Duration(0) {
		return nil, nil
	}

	shouldDeny, reason, err := d.shouldDenyAdmission(ctx, pod)
	if err != nil {
		return nil, errors.Wrap(err, "cannot determine whether it should be denied")
	} else if !shouldDeny {
		timeout := getAdmissionDelayTimeout(ctx, now)
		if remainingTime > timeout {
			remainingTime = timeout
		}
		// All admissions should be delayed. Pods will be deleted if any of admissions is finished.
		spec = &podDelayedRemoveSpec{
			sleepTask: d.getSleepTask(remainingTime),
			reason:    reason,
			admission: InterceptedAdmissionResponse{
				Allow:  true,
				Reason: "Pod deletion is delayed enough (reentry)",
			},
		}
	} else {
		spec = &podDelayedRemoveSpec{
			reason: reason,
			admission: InterceptedAdmissionResponse{
				Allow:  false,
				Reason: "Pod cannot be removed immediately. It will be eventually removed after waiting for the load balancer to start (reentry)",
			},
		}
	}
	return
}

func (d *PodGracefulDrain) shouldIntercept(ctx context.Context, pod *corev1.Pod) (bool, error) {
	svcs, err := getRegisteredServices(d.k8sClient, ctx, pod)
	if err != nil {
		return false, err
	}

	if len(svcs) == 0 {
		for _, item := range pod.Spec.ReadinessGates {
			if strings.HasPrefix(string(item.ConditionType), targetgroupbinding.TargetHealthPodConditionTypePrefix) {
				// The pod once had TargetGroupBindings, but it is somehow gone.
				// We don't know whether its TargetType is IP, it's target group, etc.
				// It might be worth to to give some time to ELB.
				return true, nil
			}
		}
		return false, nil
	}
	return true, nil
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (d *PodGracefulDrain) shouldDenyAdmission(ctx context.Context, pod *corev1.Pod) (bool, string, error) {
	if d.config.NoDenyAdmission {
		return false, "no-deny-admission config", nil
	}

	nodeName := pod.Spec.NodeName
	var node corev1.Node
	if err := d.k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return false, "", errors.Wrapf(err, "cannot get node %v", nodeName)
	}

	// Node is about to be drained.
	// `kubectl drain` will fail and stop if it meets the first pod that cannot be deleted.
	// It'll cordon a node before draining, so we detect it, and try not to deny the admission.
	if node.Spec.Unschedulable {
		return false, "node is Unschedulable", nil
	}
	for _, taint := range node.Spec.Taints {
		if taint.Key == corev1.TaintNodeUnschedulable {
			return false, "node has unschedulable taint", nil
		}
	}
	return true, "default", nil
}

func (d *PodGracefulDrain) Start(ctx context.Context) error {
	d.logger.Info("starting pod-graceful-drain")
	if err := d.cleanupPreviousRun(ctx); err != nil {
		d.logger.Error(err, "error while cleaning pods up that are not removed in the previous run")
	}

	<-ctx.Done()

	d.logger.Info("stopping pod-graceful-drain")

	drainTimeout := fallbackAdmissionDelayTimeout
	if drainTimeout < d.config.DeleteAfter {
		drainTimeout = d.config.DeleteAfter
	}

	d.delayer.Stop(drainTimeout, defaultPodGracefulDrainCleanupTimeout)
	d.logger.V(1).Info("stopped pod-graceful-drain")
	return nil
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=list;watch

func (d *PodGracefulDrain) cleanupPreviousRun(ctx context.Context) error {
	podList := &corev1.PodList{}
	// select all pods regardless of its value. These pods were about to be deleted anyway when its value is empty.
	if err := d.k8sClient.List(ctx, podList, client.HasLabels{WaitLabelKey}); err != nil {
		return errors.Wrapf(err, "cannot list pods with wait sentinel label")
	}

	now := time.Now()
	for idx := range podList.Items {
		pod := &podList.Items[idx]

		deleteAfter := d.config.DeleteAfter
		delayInfo, err := GetPodDeletionDelayInfo(pod)
		if err != nil {
			d.getLoggerFor(pod).Error(err, "cannot get pod deletion delay info, but it has wait sentinel label")
		} else {
			deleteAfter = delayInfo.GetRemainingTime(now)
		}

		d.getDelayedPodRemoveTask(pod, deleteAfter).RunAsync()
	}
	return nil
}

func (d *PodGracefulDrain) getLoggerFor(pod *corev1.Pod) logr.Logger {
	podName := types.NamespacedName{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}

	return d.logger.WithValues("pod", podName.String())
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=delete

func (d *PodGracefulDrain) getDelayedPodRemoveTask(pod *corev1.Pod, duration time.Duration) DelayedTask {
	return d.delayer.NewTask(duration, func(ctx context.Context, _ bool) error {
		logger := logr.FromContextOrDiscard(ctx)

		logger.Info("disabling label")
		if err := DisableWaitLabel(d.k8sClient, ctx, pod); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return errors.Wrap(err, "cannot disable wait sentinel label")
		}
		logger.V(1).Info("disabled label")

		logger.Info("deleting pod")
		err := wait.ExponentialBackoff(retry.DefaultBackoff, func() (bool, error) {
			if err := d.k8sClient.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil {
				if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
					// The pod is already deleted. Okay to ignore
					return true, nil
				}
				// Intercept might deny the deletion as too early until DisableWaitLabel patch is propagated.
				// TODO: error is actually admission denial
				return false, nil
			}
			return true, nil
		})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return errors.Wrap(err, "cannot delete the pod")
		}
		logger.V(1).Info("deleted pod")
		return nil
	})
}

func (d *PodGracefulDrain) getSleepTask(duration time.Duration) DelayedTask {
	return d.delayer.NewTask(duration, nil)
}
