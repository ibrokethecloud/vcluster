package pods

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/loft-sh/vcluster/pkg/controllers/syncer"
	synccontext "github.com/loft-sh/vcluster/pkg/controllers/syncer/context"
	"github.com/loft-sh/vcluster/pkg/controllers/syncer/translator"

	translatepods "github.com/loft-sh/vcluster/pkg/controllers/resources/pods/translate"
	"github.com/loft-sh/vcluster/pkg/util/loghelper"
	"github.com/loft-sh/vcluster/pkg/util/toleration"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var (
	// Default grace period in seconds
	minimumGracePeriodInSeconds int64 = 30
	zero                              = int64(0)
)

func New(ctx *synccontext.RegisterContext) (syncer.Object, error) {
	virtualClusterClient, err := kubernetes.NewForConfig(ctx.VirtualManager.GetConfig())
	if err != nil {
		return nil, err
	}
	physicalClusterClient, err := kubernetes.NewForConfig(ctx.PhysicalManager.GetConfig())
	if err != nil {
		return nil, err
	}

	// parse node selector
	var nodeSelector *metav1.LabelSelector
	if ctx.Options.EnforceNodeSelector && ctx.Options.NodeSelector != "" {
		nodeSelector, err = metav1.ParseToLabelSelector(ctx.Options.NodeSelector)
		if err != nil {
			return nil, errors.Wrap(err, "parse node selector")
		} else if len(nodeSelector.MatchExpressions) > 0 {
			return nil, errors.New("match expressions in the node selector are not supported")
		} else if len(nodeSelector.MatchLabels) == 0 {
			return nil, errors.New("at least one label=value pair has to be defined in the label selector")
		}
	}

	// parse tolerations
	var tolerations []*corev1.Toleration
	if len(ctx.Options.Tolerations) > 0 {
		for _, t := range ctx.Options.Tolerations {
			tol, err := toleration.ParseToleration(t)
			if err == nil {
				tolerations = append(tolerations, &tol)
			}
		}
	}

	// create new namespaced translator
	namespacedTranslator := translator.NewNamespacedTranslator(ctx, "pod", &corev1.Pod{})

	// create pod translator
	podTranslator, err := translatepods.NewTranslator(ctx, namespacedTranslator.EventRecorder())
	if err != nil {
		return nil, errors.Wrap(err, "create pod translator")
	}

	name := ctx.Options.Name
	if name == "" {
		name = ctx.Options.ServiceName
	}

	return &podSyncer{
		NamespacedTranslator: namespacedTranslator,

		serviceName:     ctx.Options.ServiceName,
		enableScheduler: ctx.Options.EnableScheduler,

		virtualClusterClient:  virtualClusterClient,
		physicalClusterClient: physicalClusterClient,
		podTranslator:         podTranslator,
		nodeSelector:          nodeSelector,
		tolerations:           tolerations,

		podSecurityStandard: ctx.Options.EnforcePodSecurityStandard,
		virtualLogsPath:     fmt.Sprintf(VirtualLogsPathTemplate, ctx.TargetNamespace, name),
	}, nil
}

type podSyncer struct {
	translator.NamespacedTranslator

	serviceName     string
	enableScheduler bool

	podTranslator         translatepods.Translator
	virtualClusterClient  kubernetes.Interface
	physicalClusterClient kubernetes.Interface
	nodeSelector          *metav1.LabelSelector
	tolerations           []*corev1.Toleration

	podSecurityStandard string
	virtualLogsPath     string
}

var _ syncer.IndicesRegisterer = &podSyncer{}

func (s *podSyncer) RegisterIndices(ctx *synccontext.RegisterContext) error {
	return s.NamespacedTranslator.RegisterIndices(ctx)
}

var _ syncer.ControllerModifier = &podSyncer{}

func (s *podSyncer) ModifyController(ctx *synccontext.RegisterContext, builder *builder.Builder) (*builder.Builder, error) {
	eventHandler := handler.Funcs{
		UpdateFunc: func(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
			// no need to reconcile pods if namespace labels didn't change
			if reflect.DeepEqual(e.ObjectNew.GetLabels(), e.ObjectOld.GetLabels()) {
				return
			}

			ns := e.ObjectNew.GetName()
			pods := &corev1.PodList{}
			err := ctx.VirtualManager.GetClient().List(context.TODO(), pods, client.InNamespace(ns))
			if err != nil {
				log := loghelper.New("pods-syncer-ns-watch-handler")
				log.Infof("failed to list pods in the %s namespace when handling namespace update: %v", ns, err)
				return
			}
			for _, pod := range pods.Items {
				q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      pod.GetName(),
					Namespace: ns,
				}})
			}
		},
	}

	return builder.Watches(&source.Kind{Type: &corev1.Namespace{}}, eventHandler), nil
}

var _ syncer.Syncer = &podSyncer{}

func (s *podSyncer) SyncDown(ctx *synccontext.SyncContext, vObj client.Object) (ctrl.Result, error) {
	vPod := vObj.(*corev1.Pod)
	// in some scenarios it is possible that the pod was already started and the physical pod
	// was deleted without vcluster's knowledge. In this case we are deleting the virtual pod
	// as well, to avoid conflicts with nodes if we would resync the same pod to the host cluster again.
	if vPod.DeletionTimestamp != nil || vPod.Status.StartTime != nil {
		// delete pod immediately
		ctx.Log.Infof("delete pod %s/%s immediately, because it is being deleted & there is no physical pod", vPod.Namespace, vPod.Name)
		err := ctx.VirtualClient.Delete(ctx.Context, vPod, &client.DeleteOptions{
			GracePeriodSeconds: &zero,
		})
		if kerrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// validate virtual pod before syncing it to the host cluster
	if s.podSecurityStandard != "" {
		valid, err := s.isPodSecurityStandardsValid(ctx.Context, vPod, ctx.Log)
		if err != nil {
			return ctrl.Result{}, err
		} else if !valid {
			return ctrl.Result{}, nil
		}
	}

	// translate the pod
	pPod, err := s.translate(ctx, vPod)
	if err != nil {
		return ctrl.Result{}, err
	}

	// ensure tolerations
	for _, tol := range s.tolerations {
		pPod.Spec.Tolerations = append(pPod.Spec.Tolerations, *tol)
	}

	// ensure node selector
	if s.nodeSelector != nil {
		// 2 cases:
		// 1. Pod already has a nodeName -> then we check if the node exists in the virtual cluster
		// 2. Pod has no nodeName -> then we set the nodeSelector
		if pPod.Spec.NodeName == "" {
			if pPod.Spec.NodeSelector == nil {
				pPod.Spec.NodeSelector = map[string]string{}
			}
			for k, v := range s.nodeSelector.MatchLabels {
				pPod.Spec.NodeSelector[k] = v
			}
		} else {
			// make sure the node does exist in the virtual cluster
			err = ctx.VirtualClient.Get(ctx.Context, types.NamespacedName{Name: pPod.Spec.NodeName}, &corev1.Node{})
			if err != nil {
				if !kerrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}

				s.EventRecorder().Eventf(vPod, "Warning", "SyncWarning", "Given nodeName %s does not exist in virtual cluster", pPod.Spec.NodeName)
				return ctrl.Result{RequeueAfter: time.Second * 15}, nil
			}
		}
	}

	ctx.Log.Infof("checking if pod mounts any volume")
	pPod = s.checkAndRewriteHostPath(ctx, pPod)

	// if scheduler is enabled we only sync if the pod has a node name
	if s.enableScheduler && pPod.Spec.NodeName == "" {
		return ctrl.Result{}, nil
	}

	return s.SyncDownCreate(ctx, vPod, pPod)
}

func (s *podSyncer) checkAndRewriteHostPath(ctx *synccontext.SyncContext, pPod *corev1.Pod) *corev1.Pod {
	if len(pPod.Spec.Volumes) > 0 {
		ctx.Log.Infof("checking for hostpath volumes")

		for i, volume := range pPod.Spec.Volumes {
			if volume.HostPath != nil {
				if volume.HostPath.Path == PodLoggingHostpathPath &&
					// avoid recursive rewriting of HostPaths across reconciles
					!strings.HasSuffix(volume.Name, PhysicalLogVolumeNameSuffix) {
					// we can't just mount the new hostpath to the virtual log path
					// we also need the actual 'physical' hostpath to be mounted
					// at a separate location and added to the correct containers as
					// only then the symlink targets created by logmapper would be
					// able to point to the actual log files to be traced.
					// Also we need to make sure this physical log path is not a
					// path used by the scraping agent - which should only see the
					// virtual log path
					ctx.Log.Infof("rewriting hostPath for pPod %s", pPod.Name)
					pPod.Spec.Volumes[i].HostPath.Path = s.virtualLogsPath + "/pods"

					ctx.Log.Infof("adding original hostPath to relevant containers")
					pPod = s.addPhysicalLogPathToVolumesAndCorrectContainers(ctx, volume.Name, volume.HostPath.Type, pPod)
				}

				if volume.HostPath.Path == LogHostpathPath {
					pPod.Spec.Volumes[i].HostPath.Path = s.virtualLogsPath
				}
			}
		}
	}

	return pPod
}

func (s *podSyncer) addPhysicalLogPathToVolumesAndCorrectContainers(ctx *synccontext.SyncContext, volName string, hostPathType *corev1.HostPathType, pPod *corev1.Pod) *corev1.Pod {
	// add another volume with the correct suffix
	pPod.Spec.Volumes = append(pPod.Spec.Volumes, corev1.Volume{
		Name: fmt.Sprintf("%s-%s", volName, PhysicalLogVolumeNameSuffix),
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: PodLoggingHostpathPath,
				Type: hostPathType,
			},
		},
	})

	// find containers using the original volume and mount this new volume
	// under /var/physical/log/pods - this will be used by the logmapper
	// as the target directory for the symlinks
	for i, container := range pPod.Spec.Containers {
		if len(container.VolumeMounts) > 0 {
			for _, volumeMount := range container.VolumeMounts {
				if volumeMount.Name == volName {
					// this container uses the original volume
					// keeping it as it is, we mount the physical volume
					// at the above specified mount point
					pVolMount := volumeMount.DeepCopy()
					pVolMount.Name = fmt.Sprintf("%s-%s", volName, PhysicalLogVolumeNameSuffix)
					pVolMount.MountPath = PhysicalLogVolumeMountPath

					pPod.Spec.Containers[i].VolumeMounts = append(pPod.Spec.Containers[i].VolumeMounts, *pVolMount)
				}
			}
		}
	}

	return pPod
}

func (s *podSyncer) Sync(ctx *synccontext.SyncContext, pObj client.Object, vObj client.Object) (ctrl.Result, error) {
	vPod := vObj.(*corev1.Pod)
	pPod := pObj.(*corev1.Pod)

	// should pod get deleted?
	if pPod.DeletionTimestamp != nil {
		if vPod.DeletionTimestamp == nil {
			gracePeriod := minimumGracePeriodInSeconds
			if vPod.Spec.TerminationGracePeriodSeconds != nil {
				gracePeriod = *vPod.Spec.TerminationGracePeriodSeconds
			}

			ctx.Log.Infof("delete virtual pod %s/%s, because the physical pod is being deleted", vPod.Namespace, vPod.Name)
			if err := ctx.VirtualClient.Delete(ctx.Context, vPod, &client.DeleteOptions{GracePeriodSeconds: &gracePeriod}); err != nil {
				return ctrl.Result{}, err
			}
		} else if *vPod.DeletionGracePeriodSeconds != *pPod.DeletionGracePeriodSeconds {
			ctx.Log.Infof("delete virtual pPod %s/%s with grace period seconds %v", vPod.Namespace, vPod.Name, *pPod.DeletionGracePeriodSeconds)
			if err := ctx.VirtualClient.Delete(ctx.Context, vPod, &client.DeleteOptions{GracePeriodSeconds: pPod.DeletionGracePeriodSeconds, Preconditions: metav1.NewUIDPreconditions(string(vPod.UID))}); err != nil {
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, nil
	} else if vPod.DeletionTimestamp != nil {
		ctx.Log.Infof("delete physical pod %s/%s, because virtual pod is being deleted", pPod.Namespace, pPod.Name)
		err := ctx.PhysicalClient.Delete(ctx.Context, pPod, &client.DeleteOptions{
			GracePeriodSeconds: vPod.DeletionGracePeriodSeconds,
			Preconditions:      metav1.NewUIDPreconditions(string(pPod.UID)),
		})
		if kerrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// make sure node exists for pod
	if pPod.Spec.NodeName != "" {
		requeue, err := s.ensureNode(ctx, pPod, vPod)
		if err != nil {
			return ctrl.Result{}, err
		} else if requeue {
			return ctrl.Result{Requeue: true}, nil
		}
	} else if pPod.Spec.NodeName != "" && vPod.Spec.NodeName != "" && pPod.Spec.NodeName != vPod.Spec.NodeName {
		// if physical pod nodeName is different from virtual pod nodeName, we delete the virtual one
		ctx.Log.Infof("delete virtual pod %s/%s, because node name is different between the two", vPod.Namespace, vPod.Name)
		err := ctx.VirtualClient.Delete(ctx.Context, vPod, &client.DeleteOptions{GracePeriodSeconds: &minimumGracePeriodInSeconds})
		if err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// has status changed?
	strippedPod := stripHostRewriteContainer(pPod)

	// update readiness gates & sync status virtual -> physical
	updated, err := UpdateConditions(ctx, strippedPod, vPod)
	if err != nil {
		return ctrl.Result{}, err
	} else if updated {
		return ctrl.Result{}, nil
	}

	// update status physical -> virtual
	if !equality.Semantic.DeepEqual(vPod.Status, strippedPod.Status) {
		newPod := vPod.DeepCopy()
		newPod.Status = strippedPod.Status
		ctx.Log.Infof("update virtual pod %s/%s, because status has changed", vPod.Namespace, vPod.Name)
		translator.PrintChanges(vPod, newPod, ctx.Log)
		err := ctx.VirtualClient.Status().Update(ctx.Context, newPod)
		if err != nil {
			if !kerrors.IsConflict(err) {
				s.EventRecorder().Eventf(vObj, "Warning", "SyncError", "Error updating pod: %v", err)
			}

			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// sync ephemeral containers
	if syncEphemeralContainers(vPod, strippedPod) {
		kubeIP, _, ptrServiceList, err := s.getK8sIpDnsIpServiceList(ctx, vPod)
		if err != nil {
			return ctrl.Result{}, err
		}

		// translate services to environment variables
		serviceEnv := translatepods.TranslateServicesToEnvironmentVariables(vPod.Spec.EnableServiceLinks, ptrServiceList, kubeIP)
		for i := range vPod.Spec.EphemeralContainers {
			envVar, envFrom := translatepods.TranslateContainerEnv(vPod.Spec.EphemeralContainers[i].Env, vPod.Spec.EphemeralContainers[i].EnvFrom, vPod, serviceEnv)
			vPod.Spec.EphemeralContainers[i].Env = envVar
			vPod.Spec.EphemeralContainers[i].EnvFrom = envFrom
		}

		// add ephemeralContainers subresource to physical pod
		err = AddEphemeralContainer(ctx, s.physicalClusterClient, pPod, vPod)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// validate virtual pod before syncing it to the host cluster
	if s.podSecurityStandard != "" {
		valid, err := s.isPodSecurityStandardsValid(ctx.Context, vPod, ctx.Log)
		if err != nil {
			return ctrl.Result{}, err
		} else if !valid {
			return ctrl.Result{}, nil
		}
	}

	// update the virtual pod if the spec has changed
	updatedPod, err := s.translateUpdate(pPod, vPod)
	if err != nil {
		return ctrl.Result{}, err
	} else if updatedPod != nil {
		translator.PrintChanges(pPod, updatedPod, ctx.Log)
	}

	return s.SyncDownUpdate(ctx, vPod, updatedPod)
}

func syncEphemeralContainers(vPod *corev1.Pod, pPod *corev1.Pod) bool {
	if vPod.Spec.EphemeralContainers == nil {
		return false
	}
	if len(vPod.Spec.EphemeralContainers) != len(pPod.Spec.EphemeralContainers) {
		return true
	}
	for i := range vPod.Spec.EphemeralContainers {
		if vPod.Spec.EphemeralContainers[i].Image != pPod.Spec.EphemeralContainers[i].Image {
			return true
		}
		if vPod.Spec.EphemeralContainers[i].Name != pPod.Spec.EphemeralContainers[i].Name {
			return true
		}
	}
	return false
}

func (s *podSyncer) ensureNode(ctx *synccontext.SyncContext, pObj *corev1.Pod, vObj *corev1.Pod) (bool, error) {
	if vObj.Spec.NodeName != pObj.Spec.NodeName && vObj.Spec.NodeName != "" {
		// node of virtual and physical pod are different, we delete the virtual pod to try to recover from this state
		ctx.Log.Infof("delete virtual pod %s/%s, because virtual and physical pods have different assigned nodes", vObj.Namespace, vObj.Name)
		err := ctx.VirtualClient.Delete(ctx.Context, vObj)
		if err != nil {
			return false, err
		}

		return true, nil
	}

	// ensure the node is available in the virtual cluster, if not and we sync the pod to the virtual cluster,
	// it will get deleted automatically by kubernetes so we ensure the node is synced
	vNode := &corev1.Node{}
	err := ctx.VirtualClient.Get(ctx.Context, types.NamespacedName{Name: pObj.Spec.NodeName}, vNode)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			ctx.Log.Infof("error retrieving virtual node %s: %v", pObj.Spec.NodeName, err)
			return false, err
		}

		return true, nil
	}

	if vObj.Spec.NodeName != pObj.Spec.NodeName {
		err = s.assignNodeToPod(ctx, pObj, vObj)
		if err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

func (s *podSyncer) assignNodeToPod(ctx *synccontext.SyncContext, pObj *corev1.Pod, vObj *corev1.Pod) error {
	ctx.Log.Infof("bind virtual pod %s/%s to node %s, because node name between physical and virtual is different", vObj.Namespace, vObj.Name, pObj.Spec.NodeName)
	err := s.virtualClusterClient.CoreV1().Pods(vObj.Namespace).Bind(ctx.Context, &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vObj.Name,
			Namespace: vObj.Namespace,
		},
		Target: corev1.ObjectReference{
			Kind:       "Node",
			Name:       pObj.Spec.NodeName,
			APIVersion: "v1",
		},
	}, metav1.CreateOptions{})
	if err != nil {
		s.EventRecorder().Eventf(vObj, "Warning", "SyncError", "Error binding pod: %v", err)
		return err
	}

	// wait until cache is updated
	err = wait.PollImmediate(time.Millisecond*50, time.Second*2, func() (done bool, err error) {
		vPod := &corev1.Pod{}
		err = ctx.VirtualClient.Get(ctx.Context, types.NamespacedName{Namespace: vObj.Namespace, Name: vObj.Name}, vPod)
		if err != nil {
			if kerrors.IsNotFound(err) {
				return true, nil
			}

			return false, err
		}

		return vPod.Spec.NodeName != "", nil
	})
	return err
}

func stripHostRewriteContainer(pPod *corev1.Pod) *corev1.Pod {
	if pPod.Annotations == nil || pPod.Annotations[translatepods.HostsRewrittenAnnotation] != "true" {
		return pPod
	}

	newPod := pPod.DeepCopy()
	newInitContainerStatuses := []corev1.ContainerStatus{}
	if len(newPod.Status.InitContainerStatuses) > 0 {
		for _, v := range newPod.Status.InitContainerStatuses {
			if v.Name == translatepods.HostsRewriteContainerName {
				continue
			}
			newInitContainerStatuses = append(newInitContainerStatuses, v)
		}
		newPod.Status.InitContainerStatuses = newInitContainerStatuses
	}
	return newPod
}
